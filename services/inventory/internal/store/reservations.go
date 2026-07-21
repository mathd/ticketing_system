package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Group/agency reservations (TKT-79 / ADR-027): staff-placed claims with an explicit
// expiry (not the cart TTL) and a named counterparty, drawn down into buyer holds
// partially and repeatedly. They reuse the ADR-023 mechanics wholesale: the claim_history
// idempotency registry, the quantity-neutral carve under the pool lock, and the ADR-010
// lock order (pool row first, then claim rows). Expiry rides the buyer lifecycle —
// liveClaims keys on expires_at alone — so unconverted quantity returns to sale lazily,
// by DB time, with no sweeper.

type GroupReservation struct {
	ID           uuid.UUID  `json:"hold_id"`
	OrganizerID  uuid.UUID  `json:"organizer_id"`
	PoolID       uuid.UUID  `json:"slot_id"`
	Quantity     int32      `json:"quantity"`
	Counterparty string     `json:"counterparty"`
	Channel      string     `json:"channel,omitempty"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	ServerTime   time.Time  `json:"server_time"`
}

func (p *Postgres) PlaceGroupReservation(ctx context.Context, org, slot uuid.UUID, qty int32, counterparty string, expiresAt time.Time, channel, actor, reason, key string) (GroupReservation, bool, error) {
	if qty <= 0 {
		return GroupReservation{}, false, fmt.Errorf("quantity must be positive")
	}
	fp := opFingerprint("grp-place", org, slot, qty, counterparty, expiresAt.UTC().Format(time.RFC3339Nano), channel)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return GroupReservation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	var target sql.NullInt32
	var lifecycle, closure, kind string
	// closure_status stays last before FROM (lock-handshake pattern; see CreateHold).
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupReservation{}, false, ErrNotFound
	}
	if err != nil {
		return GroupReservation{}, false, err
	}
	// A seated pool holds seat-by-seat only (TKT-80 AC2); draw-down acts on reservations
	// placed here, so this Place entry point is the guard.
	if kind == "seated" {
		return GroupReservation{}, false, ErrPoolKindMismatch
	}
	// Reservations are new demand: a draining cut (TKT-76) bounds them by the target.
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	prior, found, err := registryLookup(ctx, tx, org, key, fp)
	if err != nil {
		return GroupReservation{}, false, err
	}
	if found {
		h := GroupReservation{ID: prior.claimID, OrganizerID: org, PoolID: slot, Quantity: prior.quantity, Counterparty: counterparty, Channel: channel, Status: prior.statusAfter, ExpiresAt: &expiresAt, ServerTime: time.Now().UTC()}
		return h, true, tx.Commit()
	}
	// Same replay-then-guard order as every staff op (TKT-75 AC2).
	if err = guardOffering(lifecycle, closure); err != nil {
		return GroupReservation{}, false, err
	}
	// The explicit expiry must still be in the future at decision time
	// (clock_timestamp, not now()): a placement queued on the pool lock across its own
	// expiry would otherwise admit with stale transaction time — the TKT-78 lock-queue
	// cutoff rule. liveClaims itself deliberately stays on now() (ADR-024/ADR-027).
	var future bool
	if err = tx.QueryRowContext(ctx, `SELECT $1::timestamptz > clock_timestamp()`, expiresAt).Scan(&future); err != nil {
		return GroupReservation{}, false, err
	}
	if !future {
		return GroupReservation{}, false, ErrConflict
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return GroupReservation{}, false, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return GroupReservation{}, false, err
	}
	// int64 math: three valid int32 counters can wrap a 32-bit sum.
	if int64(confirmed)+int64(held)+int64(qty) > int64(limit) {
		return GroupReservation{}, false, ErrUnavailable
	}
	if channel != "" {
		// A channel reservation needs an active allocation with headroom (ADR-024).
		var chCap int32
		err = tx.QueryRowContext(ctx, `SELECT cap FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation, slot, channel).Scan(&chCap)
		if errors.Is(err, sql.ErrNoRows) {
			return GroupReservation{}, false, ErrUnavailable
		}
		if err != nil {
			return GroupReservation{}, false, err
		}
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
			return GroupReservation{}, false, err
		}
		if consumed+int64(qty) > int64(chCap) {
			return GroupReservation{}, false, ErrUnavailable
		}
	} else {
		// An unchanneled reservation may not eat capacity reserved for active allocations.
		var reserved int64
		if err = tx.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
			return GroupReservation{}, false, err
		}
		if int64(confirmed)+int64(held)+int64(qty)+reserved > int64(limit) {
			return GroupReservation{}, false, ErrUnavailable
		}
	}
	h := GroupReservation{ID: uuid.New(), OrganizerID: org, PoolID: slot, Quantity: qty, Counterparty: counterparty, Channel: channel, Status: "held"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,reservation_counterparty)
		VALUES($1,$2,$3,$4,'held',$5,$6,$7,'reservation',NULLIF($8,''),$9) RETURNING expires_at,now()`,
		h.ID, org, slot, qty, expiresAt, "grp-place:"+key, fp, channel, counterparty).Scan(&h.ExpiresAt, &h.ServerTime)
	if err != nil {
		return GroupReservation{}, false, err
	}
	if err = appendHistory(ctx, tx, org, h.ID, nil, "reserve", actor, reason, qty, qty, "held", &key, &fp); err != nil {
		return GroupReservation{}, false, err
	}
	return h, false, tx.Commit()
}

// DrawDownGroupReservation atomically carves qty out of a group reservation into a normal
// buyer hold (TTL, ticket type, price), exactly like ConvertOperational: the swap is
// quantity-neutral under the pool lock, so the remainder is never publicly claimable in
// between (AC2). The child inherits the source's channel — commerce cannot reattribute
// consumption across allocations. The source's own expiry is decided at decision time
// (clock_timestamp): a draw-down queued on the pool lock across the cutoff rejects, and
// settles the lazy expiry in the same transaction.
func (p *Postgres) DrawDownGroupReservation(ctx context.Context, org, id, ticketType, expectedSlot uuid.UUID, qty int32, unitAmount int64, currency, actor, reason, key string) (ConvertResult, bool, error) {
	if qty <= 0 {
		return ConvertResult{}, false, fmt.Errorf("quantity must be positive")
	}
	fp := opFingerprint("grp-draw", org, id, qty, ticketType, expectedSlot, unitAmount, currency)
	pool, err := p.poolOf(ctx, org, id)
	if err != nil {
		return ConvertResult{}, false, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return ConvertResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// The comment marks this exact statement for the contention handshake
	// (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md).
	if _, err = tx.ExecContext(ctx, `SELECT 1 /* grp-draw pool lock */ FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return ConvertResult{}, false, err
	}
	prior, found, err := registryLookup(ctx, tx, org, key, fp)
	if err != nil {
		return ConvertResult{}, false, err
	}
	if found && prior.relatedID != nil {
		var child Claim
		err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),ticket_type_id,unit_amount,currency FROM claims WHERE id=$1`, *prior.relatedID).
			Scan(&child.ID, &child.OrganizerID, &child.PoolID, &child.Quantity, &child.Status, &child.ExpiresAt, &child.ServerTime, &child.TicketTypeID, &child.UnitAmount, &child.Currency)
		if err != nil {
			return ConvertResult{}, false, err
		}
		res := ConvertResult{Child: child, SourceID: id, SourceRemaining: prior.quantityAfter, SourceStatus: prior.statusAfter}
		return res, true, tx.Commit()
	}
	var c Claim
	var channel string
	var decision time.Time
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,clock_timestamp(),claim_kind FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &channel, &c.ExpiresAt, &decision, &c.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ConvertResult{}, false, ErrNotFound
	}
	if err != nil {
		return ConvertResult{}, false, err
	}
	if c.Kind != "reservation" {
		return ConvertResult{}, false, ErrNotFound
	}
	// Lazy expiry, decided at decision time: commit the settlement (like CreateHold's
	// expired-replay path), then reject the draw.
	if c.Status == "held" && c.ExpiresAt != nil && !c.ExpiresAt.After(decision) {
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1`, id); err != nil {
			return ConvertResult{}, false, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "expire", "system", "ttl_elapsed", c.Quantity, 0, "expired", nil, nil); err != nil {
			return ConvertResult{}, false, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return ConvertResult{}, false, err
		}
		if err = tx.Commit(); err != nil {
			return ConvertResult{}, false, err
		}
		return ConvertResult{}, false, ErrConflict
	}
	// Precondition, not post-check: a wrong ticket type rejects with the source intact.
	if c.PoolID != expectedSlot {
		return ConvertResult{}, false, ErrConflict
	}
	if c.Status != "held" || qty > c.Quantity {
		return ConvertResult{}, false, ErrConflict
	}
	child := Claim{ID: uuid.New(), OrganizerID: org, PoolID: pool, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Channel: channel, Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer',NULLIF($11,'')) RETURNING expires_at,now()`,
		child.ID, org, pool, ticketType, qty, unitAmount, currency, p.ttl.String(), "grp-draw:"+id.String()+":"+key, fp, channel).Scan(&child.ExpiresAt, &child.ServerTime)
	if err != nil {
		return ConvertResult{}, false, err
	}
	status, remaining := "held", c.Quantity-qty
	if qty == c.Quantity {
		// Whole draw: the claim keeps its original quantity for the record and turns
		// terminal; the child now reserves what the source reserved.
		status, remaining = "released", 0
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='released',updated_at=now() WHERE id=$1`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE claims SET quantity=quantity-$1,updated_at=now() WHERE id=$2`, qty, id)
	}
	if err != nil {
		return ConvertResult{}, false, err
	}
	if err = appendHistory(ctx, tx, org, id, &child.ID, "draw_down", actor, reason, qty, remaining, status, &key, &fp); err != nil {
		return ConvertResult{}, false, err
	}
	res := ConvertResult{Child: child, SourceID: id, SourceRemaining: remaining, SourceStatus: status}
	return res, false, tx.Commit()
}
