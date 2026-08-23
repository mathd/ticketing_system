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

func (p *Postgres) PlaceGroupReservation(ctx context.Context, org, slot uuid.UUID, qty int32, counterparty string, expiresAt time.Time, channel, actor, reason, key string, opts ...HoldOption) (GroupReservation, bool, error) {
	var o holdOptions
	for _, opt := range opts {
		opt(&o)
	}
	presaleCode := o.presaleCode
	if qty <= 0 {
		return GroupReservation{}, false, fmt.Errorf("quantity must be positive")
	}
	// The presale code enters the fingerprint (ai-review finding 2): without it,
	// reusing an idempotency key with a DIFFERENT, absent, exhausted or
	// wrong-channel code replays the original reservation as a success instead of
	// refusing — the placement path had the same defect the hold path's
	// plan-review caught.
	//
	// Appended only when non-empty, so pre-TKT-239 reservations keep their exact
	// fingerprints; framed with lengths for the reason fingerprint() documents —
	// channel and code are opaque strings that may contain the delimiter.
	fpParts := []any{"grp-place", org, slot, qty, counterparty, expiresAt.UTC().Format(time.RFC3339Nano), channel}
	if presaleCode != "" {
		fpParts = append(fpParts, fmt.Sprintf("c%d:%s:p%d:%s", len(channel), channel, len(presaleCode), presaleCode))
	}
	fp := opFingerprint(fpParts...)
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
		return h, true, p.commitAvailability(tx, slot)
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
	// The channel's SALES WINDOW is judged BEFORE any capacity arithmetic, exactly
	// as in CreateHold and for the same reason (TKT-238 ai-review finding 1): a
	// window is a property of the requested channel, capacity is a property of the
	// pool, and checking capacity first made a closed channel read as a sellout
	// precisely when the pool was busiest.
	//
	// PlaceGroupReservation IS gated because it creates NEW consumption.
	// DrawDownGroupReservation is deliberately NOT (ADR-054): a draw-down is
	// quantity-neutral — it inserts a child and decrements the source in one
	// pool-locked transaction, consuming nothing new — and ADR-027 already settled
	// the analogous case for release_at, on the clause that transfers exactly:
	// "the source already consumed it". Gating it would strand capacity an agency
	// was granted inside the window.
	var chCap int32
	var haveAllocation bool
	var requiresCode bool
	var soldBy uuid.NullUUID
	if channel != "" {
		var windowIsOpen bool
		err = tx.QueryRowContext(ctx, `SELECT cap, requires_code, sold_by, (`+windowOpen+`) FROM channel_allocations WHERE pool_id=$1 AND channel_code=$2 AND `+activeAllocation, slot, channel).Scan(&chCap, &requiresCode, &soldBy, &windowIsOpen)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No active allocation: the code-less capacity refusal, as before.
			// There is no channel here to be closed.
			haveAllocation = false
		case err != nil:
			return GroupReservation{}, false, err
		default:
			haveAllocation = true
			if !windowIsOpen {
				return GroupReservation{}, false, ErrChannelWindowClosed
			}
			// WHO may sell this — the same predicate, in the same slot in the same
			// order, as CreateHold (TKT-246). See sellerAdmits and CreateHold's
			// comment for why window -> seller -> code -> capacity.
			//
			// This path is here BECAUSE it is the one nobody was going to change.
			// TKT-240's post-mortem named the failure exactly: "every layer reasoned
			// about the path being changed", and two sibling paths shipped a hole for
			// that reason alone. A group placement takes a channel and carries no
			// credential, so without this it would consume a bound allocation freely
			// — the guard would bind only the caller that happened to be under
			// review. Placement is gated because it creates NEW consumption;
			// draw-down is not, for the reason stated above it.
			if !sellerAdmits(soldBy, o.reseller) {
				return GroupReservation{}, false, ErrUnavailable
			}
			// The unlock code, after the window and before capacity — the same
			// precedence CreateHold documents (TKT-239 / ADR-064).
			//
			// Placement IS gated for the same reason it is window-gated: it creates
			// NEW consumption. Draw-down is not, and must not be: a draw-down is
			// quantity-neutral, so it redeems nothing new either.
			if requiresCode {
				if err = redeemPresaleCode(ctx, tx, org, channel, presaleCode, qty); err != nil {
					return GroupReservation{}, false, err
				}
			}
		}
	}
	if !requiresCode {
		presaleCode = ""
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
		// The window was decided above; what remains is the cap, which is capacity
		// arithmetic and belongs beside the pool's.
		if !haveAllocation {
			return GroupReservation{}, false, ErrUnavailable
		}
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, channel).Scan(&consumed); err != nil {
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
	// The SOURCE reservation cites the code; its draw-down children deliberately do
	// NOT (ADR-064). consumingClaims counts a live source AND its live children, so
	// a code cited on both would have the same units counted twice and a code capped
	// at 10 would exhaust at 5. The redemption happened here, once.
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,reservation_counterparty,presale_code)
		VALUES($1,$2,$3,$4,'held',$5,$6,$7,'reservation',NULLIF($8,''),$9,NULLIF($10,'')) RETURNING expires_at,now()`,
		h.ID, org, slot, qty, expiresAt, "grp-place:"+key, fp, channel, counterparty, presaleCode).Scan(&h.ExpiresAt, &h.ServerTime)
	if err != nil {
		return GroupReservation{}, false, err
	}
	if err = appendHistory(ctx, tx, org, h.ID, nil, "reserve", actor, reason, qty, qty, "held", &key, &fp); err != nil {
		return GroupReservation{}, false, err
	}
	return h, false, p.commitAvailability(tx, slot)
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
		return res, true, p.commitAvailability(tx, pool)
	}
	var c Claim
	var channel string
	var decision time.Time
	// sourcePresaleCode is read under the SAME row lock as the rest of the source,
	// so the citation the child inherits cannot be a different code than the one
	// whose units are being moved.
	var sourcePresaleCode string
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),COALESCE(presale_code,''),expires_at,clock_timestamp(),claim_kind FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &channel, &sourcePresaleCode, &c.ExpiresAt, &decision, &c.Kind)
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
		if err = p.commitAvailability(tx, pool); err != nil {
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
	// The child INHERITS the source's presale code (TKT-239 ai-review finding 1).
	//
	// The first version of this deliberately did not, reasoning that the source and
	// its children would double-count. That reasoning was WRONG and running it
	// proved so: a draw-down DECREMENTS the source by exactly qty (or releases it
	// whole, above), so source + children always sums to the original quantity.
	// Citing both is conservative, not duplicative.
	//
	// Without the citation the units stay consumed while the redemption count
	// forgets them: drawing a 10-unit reservation fully down took usage from 10 to
	// ZERO and let the same "capped at 10" code grant 10 more. Measured: 20 units
	// from a cap of 10.
	// clock_timestamp(), not now(): a buyer TTL is a duration GRANTED, so it must be
	// anchored to insert time. now() freezes at transaction start, and this transaction
	// took the pool lock first (ADR-010) -- under contention the wait falls between the
	// two and is charged to the buyer's TTL, so a hold can be returned already expired.
	// RETURNING moves with it: server_time is the buyer's reference for the countdown
	// (expires_at - server_time), so a transaction-start server_time would overstate the
	// remaining time by exactly the wait. TKT-148; ADR-024's clock split.
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,presale_code)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',clock_timestamp()+$8::interval,$9,$10,'buyer',NULLIF($11,''),NULLIF($12,'')) RETURNING expires_at,clock_timestamp()`,
		child.ID, org, pool, ticketType, qty, unitAmount, currency, p.ttl.String(), "grp-draw:"+id.String()+":"+key, fp, channel, sourcePresaleCode).Scan(&child.ExpiresAt, &child.ServerTime)
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
	return res, false, p.commitAvailability(tx, pool)
}
