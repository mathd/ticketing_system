package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationsFS embed.FS

var (
	ErrNotFound    = errors.New("not found")
	ErrUnavailable = errors.New("insufficient capacity")
	ErrConflict    = errors.New("conflicting terminal state")
	ErrIdempotency = errors.New("idempotency key reused with different request")
)

func Migrate(ctx context.Context, db *sql.DB) error {
	f, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, f)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

type Claim struct {
	ID           uuid.UUID  `json:"hold_id"`
	OrganizerID  uuid.UUID  `json:"organizer_id"`
	PoolID       uuid.UUID  `json:"slot_id"`
	Quantity     int32      `json:"quantity"`
	TicketTypeID uuid.UUID  `json:"ticket_type_id,omitempty"`
	UnitAmount   int64      `json:"unit_amount,omitempty"`
	Currency     string     `json:"currency,omitempty"`
	Status       string     `json:"status"`
	Channel      string     `json:"channel,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	ServerTime   time.Time  `json:"server_time"`
	Kind         string     `json:"-"`
	Purpose      string     `json:"-"`
	Label        string     `json:"-"`
}

// liveClaims is the single predicate deciding which claims count against capacity:
// finalizing always, held while unexpired. Operational holds have expires_at NULL
// (enforced by claims_kind_shape) and therefore never expire. Every capacity read and
// every sweep must derive from this predicate or its complement — a hand-rolled
// `expires_at > now()` silently frees every operational hold.
const liveClaims = `((status='held' AND (expires_at IS NULL OR expires_at > now())) OR status='finalizing')`

func (c Claim) expired() bool {
	return c.Status == "held" && c.ExpiresAt != nil && !c.ExpiresAt.After(c.ServerTime)
}

// sweepExpired flips due buyer holds to expired and records the expiry in claim_history,
// inside the caller's pool-locked transaction (ADR-010: expiry precedes capacity accounting).
func sweepExpired(ctx context.Context, tx *sql.Tx, pool uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `WITH swept AS (
			UPDATE claims SET status='expired', updated_at=now()
			WHERE pool_id=$1 AND status='held' AND expires_at IS NOT NULL AND expires_at<=now()
			RETURNING id, organizer_id, quantity)
		INSERT INTO claim_history(id, organizer_id, claim_id, action, actor, reason, quantity, quantity_after, status_after)
		SELECT gen_random_uuid(), organizer_id, id, 'expire', 'system', 'ttl_elapsed', quantity, 0, 'expired' FROM swept`, pool)
	if err != nil {
		return err
	}
	// Expiry lowers demand: settle a draining capacity cut (TKT-76), same lock, no-op
	// without a pending target.
	return reconcileCapacity(ctx, tx, pool)
}

func appendHistory(ctx context.Context, tx *sql.Tx, org, claim uuid.UUID, related *uuid.UUID, action, actor, reason string, qty, qtyAfter int32, statusAfter string, key, fp *string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_history(id, organizer_id, claim_id, related_claim_id, action, actor, reason, quantity, quantity_after, status_after, idempotency_key, request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, uuid.New(), org, claim, related, action, actor, reason, qty, qtyAfter, statusAfter, key, fp)
	return err
}

type Availability struct {
	SlotID    uuid.UUID `json:"slot_id"`
	Capacity  int32     `json:"capacity"`
	Held      int32     `json:"held"`
	Confirmed int32     `json:"confirmed"`
	Available int32     `json:"available"`
	// OfferingStatus is open|closed|archived (archived wins). Counters stay factual
	// while not open; only claimable availability is zeroed (TKT-75 AC3).
	OfferingStatus string `json:"offering_status"`
}

type Postgres struct {
	db  *sql.DB
	ttl time.Duration
}

func New(db *sql.DB, ttl time.Duration) *Postgres { return &Postgres{db: db, ttl: ttl} }

func (p *Postgres) Provision(ctx context.Context, eventID, slotID, organizerID uuid.UUID, capacity int32) error {
	if capacity <= 0 {
		return fmt.Errorf("capacity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, eventID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id) VALUES($1,$2,$3,$4)
		ON CONFLICT(slot_id) DO UPDATE SET capacity=EXCLUDED.capacity, updated_at=now()
		WHERE inventory_pools.organizer_id=EXCLUDED.organizer_id AND inventory_pools.confirmed_quantity=0
		AND NOT EXISTS(SELECT 1 FROM claims WHERE pool_id=EXCLUDED.slot_id)`, slotID, organizerID, capacity, eventID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// fingerprint stays byte-identical to the pre-channel format when channel is empty, so
// idempotency records created before the channel migration keep replaying (ADR-009).
func fingerprint(org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel string) string {
	s := fmt.Sprintf("%s:%s:%s:%d:%d:%s", org, slot, ticketType, qty, unitAmount, currency)
	if channel != "" {
		s += ":" + channel
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func (p *Postgres) CreateHold(ctx context.Context, org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel, key string) (Claim, bool, error) {
	if qty <= 0 {
		return Claim{}, false, fmt.Errorf("quantity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	var target sql.NullInt32
	var lifecycle, closure string
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &target, &lifecycle, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, ErrNotFound
	}
	if err != nil {
		return Claim{}, false, err
	}
	// A draining cut (TKT-76) admits against the requested target, not the clamped
	// ceiling — new demand may never grow past what the organizer asked for.
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	var existing Claim
	var fp string
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,now(),request_fingerprint,ticket_type_id,unit_amount,currency FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&existing.ID, &existing.OrganizerID, &existing.PoolID, &existing.Quantity, &existing.Status, &existing.Channel, &existing.ExpiresAt, &existing.ServerTime, &fp, &existing.TicketTypeID, &existing.UnitAmount, &existing.Currency)
	if err == nil {
		if fp != fingerprint(org, slot, ticketType, qty, unitAmount, currency, channel) {
			return Claim{}, false, ErrIdempotency
		}
		if existing.expired() {
			if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1 AND status='held'`, existing.ID); err != nil {
				return Claim{}, false, err
			}
			if err = appendHistory(ctx, tx, org, existing.ID, nil, "expire", "system", "ttl_elapsed", existing.Quantity, 0, "expired", nil, nil); err != nil {
				return Claim{}, false, err
			}
			if err = tx.Commit(); err != nil {
				return Claim{}, false, err
			}
			return Claim{}, false, ErrConflict
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, err
	}
	// Offering guard sits AFTER idempotency replay: replaying a pre-closure hold
	// returns its original outcome — a replay is not a new hold (TKT-75 AC2).
	if err = guardOffering(lifecycle, closure); err != nil {
		return Claim{}, false, err
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return Claim{}, false, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return Claim{}, false, err
	}
	// int64 math: three valid int32 counters can wrap a 32-bit sum (ai-review finding 4).
	if int64(confirmed)+int64(held)+int64(qty) > int64(limit) {
		return Claim{}, false, ErrUnavailable
	}
	if channel != "" {
		// A channel hold needs an active allocation with headroom, on top of pool capacity.
		var chCap int32
		err = tx.QueryRowContext(ctx, `SELECT cap FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation, slot, channel).Scan(&chCap)
		if errors.Is(err, sql.ErrNoRows) {
			return Claim{}, false, ErrUnavailable
		}
		if err != nil {
			return Claim{}, false, err
		}
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
			return Claim{}, false, err
		}
		if consumed+int64(qty) > int64(chCap) {
			return Claim{}, false, ErrUnavailable
		}
	} else {
		// A public hold may not eat capacity still reserved for active allocations.
		var reserved int64
		if err = tx.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
			return Claim{}, false, err
		}
		if int64(confirmed)+int64(held)+int64(qty)+reserved > int64(limit) {
			return Claim{}, false, ErrUnavailable
		}
	}
	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Channel: channel, Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer',NULLIF($11,'')) RETURNING expires_at,now()`, c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, fingerprint(org, slot, ticketType, qty, unitAmount, currency, channel), channel).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return Claim{}, false, err
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "public_hold", qty, qty, "held", nil, nil); err != nil {
		return Claim{}, false, err
	}
	return c, false, tx.Commit()
}

func (p *Postgres) Transition(ctx context.Context, org, id uuid.UUID, target string) (Claim, error) {
	var pool uuid.UUID
	err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1 AND organizer_id=$2`, id, org).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return Claim{}, err
	}
	var c Claim
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,now(),COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,''),claim_kind FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &c.Channel, &c.ExpiresAt, &c.ServerTime, &c.TicketTypeID, &c.UnitAmount, &c.Currency, &c.Kind)
	if err != nil {
		return Claim{}, err
	}
	// Operational holds transition through their own staff endpoints only; the checkout
	// lifecycle must never finalize, confirm or release one.
	if c.Kind != "buyer" {
		return Claim{}, ErrConflict
	}
	if c.expired() {
		c.Status = "expired"
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1`, id); err != nil {
			return Claim{}, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "expire", "system", "ttl_elapsed", c.Quantity, 0, "expired", nil, nil); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if c.Status == target {
		return c, tx.Commit()
	}
	// A checkout may crash after confirm succeeds but before commerce persists
	// completion. Treat replaying its earlier finalize step as already satisfied.
	if target == "finalizing" && c.Status == "confirmed" {
		return c, tx.Commit()
	}
	if target == "finalizing" && c.Status == "held" {
		c.Status = target
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='finalizing',updated_at=now() WHERE id=$1`, id)
		if err != nil {
			return c, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "finalize", "commerce", "checkout", c.Quantity, c.Quantity, "finalizing", nil, nil); err != nil {
			return c, err
		}
		return c, tx.Commit()
	}
	if c.Status != "held" && c.Status != "finalizing" {
		return c, ErrConflict
	}
	if target == "confirmed" {
		if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET confirmed_quantity=confirmed_quantity+$1,updated_at=now() WHERE slot_id=$2`, c.Quantity, pool); err != nil {
			return Claim{}, err
		}
	}
	c.Status = target
	if _, err = tx.ExecContext(ctx, `UPDATE claims SET status=$1,updated_at=now() WHERE id=$2`, target, id); err != nil {
		return Claim{}, err
	}
	action, after := "confirm", c.Quantity
	if target == "released" {
		action, after = "release", 0
		// Release lowers demand: settle a draining capacity cut (TKT-76).
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if err = appendHistory(ctx, tx, org, id, nil, action, "commerce", "checkout", c.Quantity, after, target, nil, nil); err != nil {
		return Claim{}, err
	}
	return c, tx.Commit()
}

// Availability reports pool aggregates (capacity/held/confirmed) plus a channel-scoped
// `available`: with channel empty it is the public/default claimable quantity (net of
// capacity still reserved for active allocations); with a channel it is that channel's
// claimable quantity (zero when no active allocation exists). ADR-004: 5s cache tier.
func (p *Postgres) Availability(ctx context.Context, org, slot uuid.UUID, channel string) (Availability, error) {
	var a Availability
	a.SlotID = slot
	var target sql.NullInt32
	var lifecycle, closure string
	err := p.db.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,closure_status,(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims+`) FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`, slot, org).Scan(&a.Capacity, &a.Confirmed, &target, &lifecycle, &closure, &a.Held)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.OfferingStatus = offeringStatus(lifecycle, closure)
	// Effective capacity re-derives the clamp floor from live claims (TKT-76): during a
	// draining cut it follows demand down and settles at the target, sweeper or not.
	a.Capacity = effectiveCapacity(a.Capacity, target, a.Confirmed, a.Held)
	remaining := int64(a.Capacity) - int64(a.Confirmed) - int64(a.Held)
	if a.OfferingStatus != "open" {
		remaining = 0
	}
	if channel != "" {
		var chCap int32
		err = p.db.QueryRowContext(ctx, `SELECT cap FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation, slot, channel).Scan(&chCap)
		if errors.Is(err, sql.ErrNoRows) {
			a.Available = 0
			return a, nil
		}
		if err != nil {
			return a, err
		}
		var consumed int64
		if err = p.db.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
			return a, err
		}
		a.Available = clampAvailable(min(remaining, int64(chCap)-consumed))
		return a, nil
	}
	var reserved int64
	if err = p.db.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
		return a, err
	}
	a.Available = clampAvailable(remaining - reserved)
	return a, nil
}
