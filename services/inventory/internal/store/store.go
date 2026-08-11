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
	// ErrChannelWindowClosed: the channel has an allocation with headroom and the
	// pool has capacity, but the channel's sales window is not open right now
	// (TKT-238 / ADR-054).
	//
	// A SEPARATE sentinel from ErrUnavailable, and that is the point rather than
	// tidiness: "this channel is not selling yet" and "this channel is sold out"
	// lead a caller to opposite actions — wait for the window, versus join a
	// waitlist or stop offering. Collapsing them into the code-less sellout shape
	// is exactly what COS-3 forbids, and it is what would happen by default,
	// because ErrUnavailable is the natural thing to return.
	ErrChannelWindowClosed = errors.New("channel sales window closed")
	// ErrPresaleCodeInvalid: the allocation requires an unlock code and the one
	// presented will not do (TKT-239 / ADR-055).
	//
	// DELIBERATELY UNIFORM across five distinct causes — no code, unknown code, a
	// code issued on another channel, an exhausted code, and a code outside its
	// validity window. A refusal that distinguished them would be an ENUMERATION
	// ORACLE: an attacker submitting candidates learns "that code exists but is
	// spent" versus "no such code", which is how presale codes get scraped. The
	// message and the wire body are identical for all five; only the internal
	// operator read tells them apart.
	//
	// Name the adversary (ADR-021): this defeats a scraper reading response
	// bodies. It does NOT defeat one measuring latency — an exhausted-code
	// refusal costs a usage aggregation an unknown-code refusal does not — nor
	// one who simply observes that a channel is gated. Rate limiting is ADR-051's
	// and is not claimed here.
	ErrPresaleCodeInvalid = errors.New("invalid presale code")
	ErrIdempotency        = errors.New("idempotency key reused with different request")
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
	// A seated claim's seats are released in the SAME transaction as its expiry flip
	// (TKT-80/ADR-031): an already-expired claim is never revisited, so a decoupled
	// update would block the seat forever. No-op on GA pools (no claim_seats rows).
	if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
		return err
	}
	// Expiry lowers demand: settle a draining capacity cut (TKT-76), same lock, no-op
	// without a pending target.
	return reconcileCapacity(ctx, tx, pool)
}

// releaseSeatsForTerminal flips released_at on any live seat row whose claim has reached
// a terminal state (expired/released) in this pool. Idempotent (guarded by released_at
// IS NULL) and pool-scoped; a no-op for GA pools. Runs inside the caller's pool-locked
// transaction so the seat release commits atomically with the claim's terminal flip.
func releaseSeatsForTerminal(ctx context.Context, tx *sql.Tx, pool uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE claim_seats SET released_at=now()
		WHERE pool_id=$1 AND released_at IS NULL
		  AND claim_id IN (SELECT id FROM claims WHERE pool_id=$1 AND status IN ('expired','released'))`, pool)
	return err
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
	// quarantineCap overrides MaxCatalogQuarantinePending in tests; 0 = the constant.
	quarantineCap int
	// The availability-cache invalidation seam (TKT-205) —
	// availability_invalidation.go.
	invalidatorFields
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
		return p.commitAvailability(tx, slotID)
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id) VALUES($1,$2,$3,$4)
		ON CONFLICT(slot_id) DO NOTHING`, slotID, organizerID, capacity, eventID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return p.commitAvailability(tx, slotID)
	}
	// Existing pool: take the ADR-010 pool lock FIRST, then decide in a fresh statement
	// snapshot. A single upsert cannot do this safely — its WHERE subqueries evaluate
	// against the pre-wait snapshot, so an adjustment committed while the upsert queued
	// on the row lock stays invisible and gets overwritten (TKT-76 ai-review round 2).
	// The overwrite guard covers claims, confirmed quantity, AND adjustment history
	// (claim_history.pool_id is non-NULL only on adjustment records): inventory owns
	// capacity after any staff adjustment (ADR-005 amendment).
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slotID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET capacity=$1, updated_at=now()
		WHERE slot_id=$2 AND organizer_id=$3 AND confirmed_quantity=0
		AND NOT EXISTS(SELECT 1 FROM claims WHERE pool_id=$2)
		AND NOT EXISTS(SELECT 1 FROM claim_history WHERE pool_id=$2)`, capacity, slotID, organizerID)
	if err != nil {
		return err
	}
	return p.commitAvailability(tx, slotID)
}

// fingerprint stays byte-identical to the pre-channel format when channel is empty, so
// idempotency records created before the channel migration keep replaying (ADR-009).
func fingerprint(org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel, presaleCode string) string {
	s := fmt.Sprintf("%s:%s:%s:%d:%d:%s", org, slot, ticketType, qty, unitAmount, currency)
	if channel != "" {
		s += ":" + channel
	}
	// Appended ONLY when non-empty, for exactly the reason the channel above is:
	// an unconditional append rehashes EVERY claim in the database, so every
	// in-flight retry stops replaying and re-executes instead — a double-sell on
	// retry, system-wide, from what looks like adding a field (TKT-239
	// plan-review). TestFingerprintStaysByteIdenticalWithoutAPresaleCode pins it.
	//
	// It must be IN the fingerprint though: two holds sharing an idempotency key
	// but presenting different codes are different requests, and replaying one as
	// the other would grant the second the first's redemption.
	if presaleCode != "" {
		s += ":" + presaleCode
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// HoldOption carries the optional, rarely-set inputs to CreateHold.
//
// A variadic option rather than a tenth positional parameter, and that is a
// safety choice, not a style one: the signature already ends in FOUR adjacent
// bare strings (currency, channel, key) across 70 call sites, and a fifth would
// be transposable with any of them while still compiling. `WithPresaleCode(x)`
// cannot be passed in the wrong slot.
type HoldOption func(*holdOptions)

type holdOptions struct{ presaleCode string }

// WithPresaleCode supplies the unlock code for a gated channel (TKT-239).
// Absent, or on an ungated allocation, it is ignored.
func WithPresaleCode(code string) HoldOption {
	return func(o *holdOptions) { o.presaleCode = code }
}

func (p *Postgres) CreateHold(ctx context.Context, org, slot, ticketType uuid.UUID, qty int32, unitAmount int64, currency, channel, key string, opts ...HoldOption) (Claim, bool, error) {
	var o holdOptions
	for _, opt := range opts {
		opt(&o)
	}
	presaleCode := o.presaleCode
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
	var lifecycle, closure, kind string
	// closure_status stays the LAST column before FROM: the lock-handshake tests pin the
	// pool-lock statement by the LIKE pattern "%closure_status FROM inventory_pools%FOR
	// UPDATE%" (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md), so
	// new columns go before it, never between it and FROM.
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, ErrNotFound
	}
	if err != nil {
		return Claim{}, false, err
	}
	// A seated pool is claimed seat-by-seat (CreateSeatHold), never through the GA
	// quantity path — else fungible tickets would sell over reserved seats (AC2).
	if kind == "seated" {
		return Claim{}, false, ErrPoolKindMismatch
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
		if fp != fingerprint(org, slot, ticketType, qty, unitAmount, currency, channel, presaleCode) {
			return Claim{}, false, ErrIdempotency
		}
		if existing.expired() {
			if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1 AND status='held'`, existing.ID); err != nil {
				return Claim{}, false, err
			}
			if err = appendHistory(ctx, tx, org, existing.ID, nil, "expire", "system", "ttl_elapsed", existing.Quantity, 0, "expired", nil, nil); err != nil {
				return Claim{}, false, err
			}
			if err = p.commitAvailability(tx, slot); err != nil {
				return Claim{}, false, err
			}
			return Claim{}, false, ErrConflict
		}
		return existing, true, p.commitAvailability(tx, slot)
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
	// The channel's SALES WINDOW is judged before any capacity arithmetic
	// (TKT-238 ai-review finding 1).
	//
	// Precedence, not tidiness. A window is a property of the requested channel;
	// capacity is a property of the pool. Checking capacity first made the same
	// request answer `channel_window_closed` while the pool had room and the
	// code-less "insufficient capacity" once it did not — so a closed presale
	// read as a sellout exactly when the on-sale was busiest. Worse, the load
	// harness classifies a code-less 409 as an expected capacity rejection, so a
	// run against a closed channel could be accepted as contention evidence.
	//
	// Verified before fixing: with pool headroom the refusal was the window; with
	// an operational hold consuming the pool it became "insufficient capacity"
	// for an otherwise identical request.
	//
	// Still under the pool FOR UPDATE and still on clock_timestamp(): moving it
	// earlier changes which refusal wins, never when the boundary is judged.
	var channelWindowOpen = true
	var chCap int32
	var haveAllocation bool
	var requiresCode bool
	if channel != "" {
		err = tx.QueryRowContext(ctx,
			`SELECT cap, requires_code, (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation,
			slot, channel).Scan(&chCap, &requiresCode, &channelWindowOpen)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No ACTIVE allocation for this channel — released, or never
			// configured. That stays the code-less capacity refusal it has always
			// been: there is no channel here to be closed.
			haveAllocation = false
		case err != nil:
			return Claim{}, false, err
		default:
			haveAllocation = true
			if !channelWindowOpen {
				return Claim{}, false, ErrChannelWindowClosed
			}
			// The unlock code is judged AFTER the channel's window and BEFORE any
			// capacity arithmetic (TKT-239 / ADR-055).
			//
			// Window first: a closed channel is not selling to anyone, so "wrong
			// code" would be a misleading answer to a request that a valid code
			// would also have refused. Capacity last, for the reason TKT-238's
			// ai-review established — a channel-property refusal must not be
			// masked by a full pool, or a gated presale reads as a sellout exactly
			// when the on-sale is busiest.
			//
			// redeemPresaleCode takes the presale_codes ROW LOCK. The pool lock
			// above does NOT serialize a code: a code spans slots, so two holds on
			// different pools take different pool locks. See its comment.
			if requiresCode {
				if err := redeemPresaleCode(ctx, tx, org, channel, presaleCode, qty); err != nil {
					return Claim{}, false, err
				}
			}
		}
	}
	if !requiresCode {
		// A code presented to an ungated allocation is NOT an error — it is
		// ignored, and deliberately not recorded. Recording it would let a caller
		// write arbitrary strings into an attribution column that reporting reads,
		// on a path where nothing validates them.
		presaleCode = ""
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
		// The allocation and its window were read above, before the pool-capacity
		// check, so a closed window refuses as a closed window whatever the pool
		// looks like. What remains here is the CAP check, which is genuinely
		// capacity arithmetic and belongs beside the pool's.
		//
		// An absent active allocation is the code-less capacity refusal: there is
		// no channel here to be closed. That keeps "closed window" and "no such
		// channel" distinct, which selecting the predicate rather than filtering
		// on it is what makes possible.
		if !haveAllocation {
			return Claim{}, false, ErrUnavailable
		}
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
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
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,presale_code)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer',NULLIF($11,''),NULLIF($12,'')) RETURNING expires_at,now()`, c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, fingerprint(org, slot, ticketType, qty, unitAmount, currency, channel, presaleCode), channel, presaleCode).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return Claim{}, false, err
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "public_hold", qty, qty, "held", nil, nil); err != nil {
		return Claim{}, false, err
	}
	return c, false, p.commitAvailability(tx, slot)
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
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if c.Status == target {
		return c, p.commitAvailability(tx, pool)
	}
	// A checkout may crash after confirm succeeds but before commerce persists
	// completion. Treat replaying its earlier finalize step as already satisfied.
	if target == "finalizing" && c.Status == "confirmed" {
		return c, p.commitAvailability(tx, pool)
	}
	// A release against an expired claim is vacuously satisfied: expiry already freed
	// the seats (the branch above / an earlier pass), so the obligation a release
	// discharges is gone either way. Answering conflict here made expiry
	// indistinguishable from `confirmed` — a genuinely sold seat — and commerce
	// recovery parked refundable orders on the difference (TKT-115). Confirm of an
	// expired claim stays a conflict below: expired can never buy a seat.
	if target == "released" && c.Status == "expired" {
		return c, p.commitAvailability(tx, pool)
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
		return c, p.commitAvailability(tx, pool)
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
		// Release lowers demand: settle a draining capacity cut (TKT-76). A seated
		// claim's seats free in the same txn (ADR-031).
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if err = appendHistory(ctx, tx, org, id, nil, action, "commerce", "checkout", c.Quantity, after, target, nil, nil); err != nil {
		return Claim{}, err
	}
	return c, p.commitAvailability(tx, pool)
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
		// Same statement, one more column — NOT a third query. The availability read
		// is pinned at exactly two statements by smoke/onsale_read_proof_test.go,
		// matched on hardcoded SQL fragments, so a separate window lookup would
		// break that proof numerically rather than visibly.
		var chCap int32
		var windowIsOpen bool
		err = p.db.QueryRowContext(ctx,
			`SELECT cap, (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation,
			slot, channel).Scan(&chCap, &windowIsOpen)
		if errors.Is(err, sql.ErrNoRows) {
			a.Available = 0
			return a, nil
		}
		if err == nil && !windowIsOpen {
			// Nothing is claimable on a closed channel, so the read says 0 — the same
			// answer the claim path would give. The read cannot say WHY without a new
			// public field, and it deliberately does not get one: the staff breakdown
			// carries the window (ADR-054), and adding a public field later is
			// additive while retracting one is a contract break.
			a.Available = 0
			return a, nil
		}
		if err != nil {
			return a, err
		}
		var consumed int64
		if err = p.db.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
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
