package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Operational holds (TKT-77 / ADR-023): staff-placed claims (house, artist, kills) that
// count against pool capacity but never expire. Their idempotency registry is
// claim_history — staff keys never share the claims-table key namespace with buyer holds.
// Every mutation follows the ADR-010 lock order: pool row first, then claim rows.

type OperationalHold struct {
	ID          uuid.UUID `json:"hold_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	PoolID      uuid.UUID `json:"slot_id"`
	Quantity    int32     `json:"quantity"`
	Purpose     string    `json:"purpose"`
	Label       string    `json:"label"`
	Status      string    `json:"status"`
	ServerTime  time.Time `json:"server_time"`
}

type ConvertResult struct {
	Child           Claim     `json:"hold"`
	SourceID        uuid.UUID `json:"source_id"`
	SourceRemaining int32     `json:"source_remaining"`
	SourceStatus    string    `json:"source_status"`
}

type StaffAvailability struct {
	SlotID          uuid.UUID             `json:"slot_id"`
	Capacity        int32                 `json:"capacity"`
	TargetCapacity  *int32                `json:"target_capacity,omitempty"`
	BuyerHeld       int32                 `json:"buyer_held"`
	OperationalHeld int32                 `json:"operational_held"`
	ReservationHeld int32                 `json:"reservation_held"`
	Confirmed       int32                 `json:"confirmed"`
	Available       int32                 `json:"available"`
	PublicAvailable int32                 `json:"public_available"`
	OfferingStatus  string                `json:"offering_status"`
	Channels        []ChannelAvailability `json:"channels"`
}

type HistoryEntry struct {
	Action         string     `json:"action"`
	Actor          string     `json:"actor"`
	Reason         string     `json:"reason"`
	Quantity       int32      `json:"quantity"`
	QuantityAfter  int32      `json:"quantity_after"`
	StatusAfter    string     `json:"status_after"`
	RelatedClaimID *uuid.UUID `json:"related_claim_id,omitempty"`
	// TargetCapacity is set only on capacity-adjustment records (TKT-76): the requested
	// target while a clamped cut drains.
	TargetCapacity *int32    `json:"target_capacity,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
}

func opFingerprint(parts ...any) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%v", parts)))
}

// registryLookup consults the claim_history idempotency registry inside the caller's
// pool-locked transaction. found=true means the operation already happened: the returned
// row is its immutable outcome. A fingerprint mismatch is a key reuse.
type registryRow struct {
	claimID        uuid.UUID
	relatedID      *uuid.UUID
	action         string
	quantity       int32
	quantityAfter  int32
	statusAfter    string
	targetCapacity *int32
}

func registryLookup(ctx context.Context, tx *sql.Tx, org uuid.UUID, key, fp string) (registryRow, bool, error) {
	var row registryRow
	var storedFP string
	// Pool-level capacity records carry a NULL claim_id (TKT-76): coalesce so the scan
	// stays shape-agnostic; capacity callers read targetCapacity instead.
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(claim_id,'00000000-0000-0000-0000-000000000000'::uuid),related_claim_id,action,quantity,quantity_after,status_after,target_capacity,COALESCE(request_fingerprint,'')
		FROM claim_history WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&row.claimID, &row.relatedID, &row.action, &row.quantity, &row.quantityAfter, &row.statusAfter, &row.targetCapacity, &storedFP)
	if errors.Is(err, sql.ErrNoRows) {
		return registryRow{}, false, nil
	}
	if err != nil {
		return registryRow{}, false, err
	}
	if storedFP != fp {
		return registryRow{}, false, ErrIdempotency
	}
	return row, true, nil
}

func (p *Postgres) PlaceOperationalHold(ctx context.Context, org, slot uuid.UUID, qty int32, purpose, label, actor, reason, key string) (OperationalHold, bool, error) {
	if qty <= 0 {
		return OperationalHold{}, false, fmt.Errorf("quantity must be positive")
	}
	fp := opFingerprint("op-place", org, slot, qty, purpose, label)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationalHold{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	var target sql.NullInt32
	var lifecycle, closure, kind string
	// closure_status stays last before FROM (lock-handshake pattern; see CreateHold).
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationalHold{}, false, ErrNotFound
	}
	if err != nil {
		return OperationalHold{}, false, err
	}
	// A seated pool holds seat-by-seat only: a quantity-based operational hold would
	// consume capacity with no seat rows, selling fungible tickets over reserved seats
	// (TKT-80 AC2). Convert/release act on holds placed here, so they are transitively
	// covered — this Place entry point is the guard.
	if kind == "seated" {
		return OperationalHold{}, false, ErrPoolKindMismatch
	}
	// Staff holds are new demand too: a draining cut (TKT-76) bounds them by the target.
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	prior, found, err := registryLookup(ctx, tx, org, key, fp)
	if err != nil {
		return OperationalHold{}, false, err
	}
	if found {
		h := OperationalHold{ID: prior.claimID, OrganizerID: org, PoolID: slot, Quantity: prior.quantity, Purpose: purpose, Label: label, Status: prior.statusAfter, ServerTime: time.Now().UTC()}
		return h, true, p.commitAvailability(tx, slot)
	}
	// Same replay-then-guard order as CreateHold: a staff hold is a new hold too (TKT-75 AC2).
	if err = guardOffering(lifecycle, closure); err != nil {
		return OperationalHold{}, false, err
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return OperationalHold{}, false, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return OperationalHold{}, false, err
	}
	// int64 math: three valid int32 counters can wrap a 32-bit sum (ai-review finding 4).
	if int64(confirmed)+int64(held)+int64(qty) > int64(limit) {
		return OperationalHold{}, false, ErrUnavailable
	}
	h := OperationalHold{ID: uuid.New(), OrganizerID: org, PoolID: slot, Quantity: qty, Purpose: purpose, Label: label, Status: "held"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,operational_purpose,operational_label)
		VALUES($1,$2,$3,$4,'held',NULL,$5,$6,'operational',$7,$8) RETURNING now()`,
		h.ID, org, slot, qty, "op-place:"+key, fp, purpose, label).Scan(&h.ServerTime)
	if err != nil {
		return OperationalHold{}, false, err
	}
	if err = appendHistory(ctx, tx, org, h.ID, nil, "place", actor, reason, qty, qty, "held", &key, &fp); err != nil {
		return OperationalHold{}, false, err
	}
	return h, false, p.commitAvailability(tx, slot)
}

// lockOperational discovers the pool without locking, then locks pool → claim (ADR-010)
// and returns the claim, which must be an operational hold.
func lockOperational(ctx context.Context, tx *sql.Tx, org, id uuid.UUID) (Claim, error) {
	var c Claim
	err := tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,now(),claim_kind,COALESCE(operational_purpose,''),COALESCE(operational_label,'') FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &c.ServerTime, &c.Kind, &c.Purpose, &c.Label)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	if c.Kind != "operational" {
		return Claim{}, ErrNotFound
	}
	return c, nil
}

func (p *Postgres) poolOf(ctx context.Context, org, id uuid.UUID) (uuid.UUID, error) {
	var pool uuid.UUID
	err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1 AND organizer_id=$2`, id, org).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return pool, err
}

func (p *Postgres) ReleaseOperational(ctx context.Context, org, id uuid.UUID, qty int32, actor, reason, key string) (OperationalHold, bool, error) {
	if qty <= 0 {
		return OperationalHold{}, false, fmt.Errorf("quantity must be positive")
	}
	fp := opFingerprint("op-release", org, id, qty)
	pool, err := p.poolOf(ctx, org, id)
	if err != nil {
		return OperationalHold{}, false, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationalHold{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return OperationalHold{}, false, err
	}
	prior, found, err := registryLookup(ctx, tx, org, key, fp)
	if err != nil {
		return OperationalHold{}, false, err
	}
	if found {
		h := OperationalHold{ID: prior.claimID, OrganizerID: org, PoolID: pool, Quantity: prior.quantityAfter, Status: prior.statusAfter, ServerTime: time.Now().UTC()}
		return h, true, p.commitAvailability(tx, pool)
	}
	c, err := lockOperational(ctx, tx, org, id)
	if err != nil {
		return OperationalHold{}, false, err
	}
	if c.Status != "held" || qty > c.Quantity {
		return OperationalHold{}, false, ErrConflict
	}
	status, remaining := "held", c.Quantity-qty
	if qty == c.Quantity {
		// Whole release: the claim keeps its original quantity for the record and turns
		// terminal; nothing remains counted against capacity.
		status, remaining = "released", 0
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='released',updated_at=now() WHERE id=$1`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE claims SET quantity=quantity-$1,updated_at=now() WHERE id=$2`, qty, id)
	}
	if err != nil {
		return OperationalHold{}, false, err
	}
	if err = appendHistory(ctx, tx, org, id, nil, "release", actor, reason, qty, remaining, status, &key, &fp); err != nil {
		return OperationalHold{}, false, err
	}
	// Release lowers demand: settle a draining capacity cut (TKT-76).
	if err = reconcileCapacity(ctx, tx, pool); err != nil {
		return OperationalHold{}, false, err
	}
	h := OperationalHold{ID: id, OrganizerID: org, PoolID: pool, Quantity: remaining, Purpose: c.Purpose, Label: c.Label, Status: status, ServerTime: c.ServerTime}
	return h, false, p.commitAvailability(tx, pool)
}

// ConvertOperational atomically carves qty out of an operational hold into a normal buyer
// hold (TTL, ticket type, price) under the pool lock. The swap is quantity-neutral —
// child + remainder reserve exactly what the source reserved — so converted capacity is
// never observable as publicly available in between (AC2). The child's claims-table
// idempotency key is namespaced with the source id: a raw forward of the staff key could
// collide with an unrelated buyer hold's key.
//
// expectedSlot is the slot the caller priced the ticket type against. It is verified
// against the hold's pool BEFORE any write: a mismatch discovered after commit would have
// already carved operational quantity into a child destined to expire into the public
// pool (ai-review finding 1).
func (p *Postgres) ConvertOperational(ctx context.Context, org, id, ticketType, expectedSlot uuid.UUID, qty int32, unitAmount int64, currency, actor, reason, key string) (ConvertResult, bool, error) {
	if qty <= 0 {
		return ConvertResult{}, false, fmt.Errorf("quantity must be positive")
	}
	fp := opFingerprint("op-convert", org, id, qty, ticketType, expectedSlot, unitAmount, currency)
	pool, err := p.poolOf(ctx, org, id)
	if err != nil {
		return ConvertResult{}, false, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return ConvertResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
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
	c, err := lockOperational(ctx, tx, org, id)
	if err != nil {
		return ConvertResult{}, false, err
	}
	// Precondition, not post-check: nothing has been written yet, so a wrong ticket type
	// rejects with the source hold fully intact.
	if c.PoolID != expectedSlot {
		return ConvertResult{}, false, ErrConflict
	}
	if c.Status != "held" || qty > c.Quantity {
		return ConvertResult{}, false, ErrConflict
	}
	child := Claim{ID: uuid.New(), OrganizerID: org, PoolID: pool, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer') RETURNING expires_at,now()`,
		child.ID, org, pool, ticketType, qty, unitAmount, currency, p.ttl.String(), "convert:"+id.String()+":"+key, fp).Scan(&child.ExpiresAt, &child.ServerTime)
	if err != nil {
		return ConvertResult{}, false, err
	}
	status, remaining := "held", c.Quantity-qty
	if qty == c.Quantity {
		status, remaining = "released", 0
		_, err = tx.ExecContext(ctx, `UPDATE claims SET status='released',updated_at=now() WHERE id=$1`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE claims SET quantity=quantity-$1,updated_at=now() WHERE id=$2`, qty, id)
	}
	if err != nil {
		return ConvertResult{}, false, err
	}
	if err = appendHistory(ctx, tx, org, id, &child.ID, "convert", actor, reason, qty, remaining, status, &key, &fp); err != nil {
		return ConvertResult{}, false, err
	}
	res := ConvertResult{Child: child, SourceID: id, SourceRemaining: remaining, SourceStatus: status}
	return res, false, p.commitAvailability(tx, pool)
}

func (p *Postgres) History(ctx context.Context, org, id uuid.UUID) ([]HistoryEntry, error) {
	if _, err := p.poolOf(ctx, org, id); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT action,actor,reason,quantity,quantity_after,status_after,related_claim_id,occurred_at
		FROM claim_history WHERE organizer_id=$1 AND claim_id=$2
		ORDER BY occurred_at, append_order NULLS FIRST, id`, org, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		if err = rows.Scan(&e.Action, &e.Actor, &e.Reason, &e.Quantity, &e.QuantityAfter, &e.StatusAfter, &e.RelatedClaimID, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) StaffAvailability(ctx context.Context, org, slot uuid.UUID) (StaffAvailability, error) {
	a := StaffAvailability{SlotID: slot}
	var target sql.NullInt32
	var lifecycle, closure string
	err := p.db.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,closure_status,
			(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND claim_kind='buyer' AND `+liveClaims+`),
			(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND claim_kind='operational' AND `+liveClaims+`),
			(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND claim_kind='reservation' AND `+liveClaims+`)
		FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`, slot, org).
		Scan(&a.Capacity, &a.Confirmed, &target, &lifecycle, &closure, &a.BuyerHeld, &a.OperationalHeld, &a.ReservationHeld)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.OfferingStatus = offeringStatus(lifecycle, closure)
	// Staff see both sides of a draining cut (TKT-76): the effective clamp floor as
	// capacity, the eventual target separately.
	a.Capacity = effectiveCapacity(a.Capacity, target, a.Confirmed, a.BuyerHeld+a.OperationalHeld+a.ReservationHeld)
	if target.Valid {
		a.TargetCapacity = &target.Int32
	}
	a.Available = a.Capacity - a.Confirmed - a.BuyerHeld - a.OperationalHeld - a.ReservationHeld
	remaining := int64(a.Available)
	if a.OfferingStatus != "open" {
		a.Available, remaining = 0, 0
	}
	if a.Channels, err = channelAvailabilities(ctx, p.db, slot, remaining); err != nil {
		return a, err
	}
	var reserved int64
	if err = p.db.QueryRowContext(ctx, reservedForChannelsSQL, slot).Scan(&reserved); err != nil {
		return a, err
	}
	a.PublicAvailable = clampAvailable(remaining - reserved)
	return a, nil
}
