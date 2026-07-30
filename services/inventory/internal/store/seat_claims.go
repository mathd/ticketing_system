package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrPoolKindMismatch reports a quantity claim against a seated pool or a seat
	// claim against a GA pool — the two inventory kinds never cross (AC2).
	ErrPoolKindMismatch = errors.New("claim kind does not match pool kind")
	// ErrSeatTaken reports that one of the requested seats is already held/confirmed
	// by another live claim — the DB unique index is the arbiter (AC1).
	ErrSeatTaken = errors.New("seat already held by another live claim")
	// ErrSeatSetInvalid reports an empty, oversized, or malformed seat set.
	ErrSeatSetInvalid = errors.New("invalid seat set")
)

// MaxSeatsPerHold bounds a single seat-set claim (mirrors the GA 1..50 quantity band).
const MaxSeatsPerHold = 50

// PinRef is the catalog-pin coordinates for a claim's seats: the pool's seat map,
// the seat identities, and the stable pinned_by ("hold:<claim_id>"). The store never
// makes HTTP calls (ADR-010); it returns these so the post-commit API caller can
// pin/unpin against catalog.
type PinRef struct {
	SeatMapID uuid.UUID
	Seats     []string
	PinnedBy  string
}

// SeatHold is the result of CreateSeatHold: the buyer claim (quantity = len(Seats)),
// the map + seats to pin, whether this was an idempotent replay, and any seats that
// this transaction swept-expired (their pins are now stale — the caller unpins them
// best-effort). ExpiredPins is how a quiet seated pool's expired-hold pins get cleaned
// up on the next seat hold (ADR-031: no background worker).
type SeatHold struct {
	Claim       Claim
	SeatMapID   uuid.UUID
	Seats       []string
	PinnedBy    string // "hold:<claim_id>" — the exact key the caller pins/unpins with
	Replay      bool
	ExpiredPins []PinRef
}

func pinnedBy(claimID uuid.UUID) string { return "hold:" + claimID.String() }

// canonicalSeats sorts and de-duplicates a seat set. The idempotency fingerprint and
// the insert both use the canonical form so [A,B] and [B,A] replay as one claim under
// retry rather than conflicting (planReview finding).
func canonicalSeats(seats []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ErrSeatSetInvalid
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 || len(out) > MaxSeatsPerHold {
		return nil, ErrSeatSetInvalid
	}
	sort.Strings(out)
	return out, nil
}

// seatFingerprint binds the idempotency key to the canonical request (ADR-010): same
// key + same request replays; same key + different seats/price conflicts. The seat set
// is JSON-encoded, not comma-joined: a seat identity may itself contain a comma, and a
// comma-join is not injective (["A","B,C"] and ["A,B","C"] would collide and let one
// request replay the other's claim).
func seatFingerprint(org, slot, ticketType uuid.UUID, seats []string, unitAmount int64, currency string) string {
	enc, _ := json.Marshal(seats)
	s := fmt.Sprintf("seat:%s:%s:%s:%s:%d:%s", org, slot, ticketType, enc, unitAmount, currency)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// ProvisionSeated records a seated pool for a slot (catalog schema-4 seated
// publication). Idempotent on the event id, like Provision. capacity is the venue GA
// snapshot used as a coarse ceiling — the tight per-seat oversell boundary is the
// claim_seats unique index plus catalog PinSeat existence-validation (ADR-031).
func (p *Postgres) ProvisionSeated(ctx context.Context, eventID, slotID, organizerID, seatMapID uuid.UUID, capacity int32) error {
	if capacity <= 0 {
		return fmt.Errorf("capacity must be positive")
	}
	if seatMapID == uuid.Nil {
		return fmt.Errorf("seated pool requires a seat map")
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
	_, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id,inventory_kind,seat_map_id)
		VALUES($1,$2,$3,$4,'seated',$5) ON CONFLICT(slot_id) DO NOTHING`, slotID, organizerID, capacity, eventID, seatMapID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CreateSeatHold holds a specific set of seats on a seated slot, reusing the ADR-010
// pool-lock-first order, lazy expiry, and organizer-scoped idempotency. Each seat is
// one row in claim_seats under one buyer claim (quantity = len(seats)); the partial
// unique index makes a second live claim on any seat impossible (AC1). It never calls
// catalog — the caller pins the returned Seats after commit (hold-then-pin, ADR-031).
func (p *Postgres) CreateSeatHold(ctx context.Context, org, slot, ticketType uuid.UUID, seats []string, unitAmount int64, currency, key string) (SeatHold, error) {
	canon, err := canonicalSeats(seats)
	if err != nil {
		return SeatHold{}, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return SeatHold{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var capacity, confirmed int32
	var target sql.NullInt32
	var lifecycle, closure, kind string
	var seatMapID uuid.NullUUID
	// closure_status stays last before FROM (lock-handshake pattern; see CreateHold).
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,seat_map_id,closure_status
		FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).
		Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &seatMapID, &closure)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatHold{}, ErrNotFound
	}
	if err != nil {
		return SeatHold{}, err
	}
	if kind != "seated" || !seatMapID.Valid {
		return SeatHold{}, ErrPoolKindMismatch
	}
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	qty := int32(len(canon))
	fp := seatFingerprint(org, slot, ticketType, canon, unitAmount, currency)

	// Idempotency replay (ADR-010): same key replays the original outcome; the fingerprint
	// covers the canonical seat set so [A,B]/[B,A] retries do not conflict.
	var existing Claim
	var existingFP string
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),request_fingerprint,COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,'')
		FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&existing.ID, &existing.OrganizerID, &existing.PoolID, &existing.Quantity, &existing.Status, &existing.ExpiresAt, &existing.ServerTime, &existingFP, &existing.TicketTypeID, &existing.UnitAmount, &existing.Currency)
	if err == nil {
		if existingFP != fp {
			return SeatHold{}, ErrIdempotency
		}
		if existing.expired() {
			if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1 AND status='held'`, existing.ID); err != nil {
				return SeatHold{}, err
			}
			if err = appendHistory(ctx, tx, org, existing.ID, nil, "expire", "system", "ttl_elapsed", existing.Quantity, 0, "expired", nil, nil); err != nil {
				return SeatHold{}, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1 AND released_at IS NULL`, existing.ID); err != nil {
				return SeatHold{}, err
			}
			if err = tx.Commit(); err != nil {
				return SeatHold{}, err
			}
			return SeatHold{}, ErrConflict
		}
		// A terminal claim (released, or already-swept expired) cannot be revived by a
		// replay: returning it as a live hold would re-pin seats that are free again and
		// report a false success. The key is spent — the caller must hold anew (ErrConflict).
		if existing.Status == "released" || existing.Status == "expired" {
			return SeatHold{}, ErrConflict
		}
		existing.Kind = "buyer"
		return SeatHold{Claim: existing, SeatMapID: seatMapID.UUID, Seats: canon, PinnedBy: pinnedBy(existing.ID), Replay: true}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SeatHold{}, err
	}

	if err = guardOffering(lifecycle, closure); err != nil {
		return SeatHold{}, err
	}
	// Capture pins about to be swept-expired so the caller can clean them from catalog.
	expiredPins, err := seatsAboutToExpire(ctx, tx, slot, seatMapID.UUID)
	if err != nil {
		return SeatHold{}, err
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return SeatHold{}, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return SeatHold{}, err
	}
	if int64(confirmed)+int64(held)+int64(qty) > int64(limit) {
		return SeatHold{}, ErrUnavailable
	}

	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer') RETURNING expires_at,now()`,
		c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, fp).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return SeatHold{}, err
	}
	for _, seat := range canon {
		if _, err = tx.ExecContext(ctx, `INSERT INTO claim_seats(claim_id,pool_id,seat_identity) VALUES($1,$2,$3)`, c.ID, slot, seat); err != nil {
			if isUniqueViolation(err, "claim_seats_one_live_per_seat") {
				return SeatHold{}, ErrSeatTaken
			}
			return SeatHold{}, err
		}
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "seat_hold", qty, qty, "held", nil, nil); err != nil {
		return SeatHold{}, err
	}
	if err = tx.Commit(); err != nil {
		return SeatHold{}, err
	}
	return SeatHold{Claim: c, SeatMapID: seatMapID.UUID, Seats: canon, PinnedBy: pinnedBy(c.ID), ExpiredPins: expiredPins}, nil
}

// seatsAboutToExpire returns the pin refs for held seated claims in this pool whose TTL
// has elapsed but which the imminent sweep has not yet flipped — one PinRef per claim so
// each carries its own "hold:<claim_id>" pinned_by.
func seatsAboutToExpire(ctx context.Context, tx *sql.Tx, pool, seatMapID uuid.UUID) ([]PinRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT cs.claim_id, cs.seat_identity
		FROM claim_seats cs JOIN claims c ON c.id=cs.claim_id
		WHERE cs.pool_id=$1 AND cs.released_at IS NULL
		  AND c.status='held' AND c.expires_at IS NOT NULL AND c.expires_at<=now()
		ORDER BY cs.claim_id`, pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byClaim := map[uuid.UUID][]string{}
	order := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		var seat string
		if err = rows.Scan(&id, &seat); err != nil {
			return nil, err
		}
		if _, ok := byClaim[id]; !ok {
			order = append(order, id)
		}
		byClaim[id] = append(byClaim[id], seat)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	refs := make([]PinRef, 0, len(order))
	for _, id := range order {
		refs = append(refs, PinRef{SeatMapID: seatMapID, Seats: byClaim[id], PinnedBy: pinnedBy(id)})
	}
	return refs, nil
}

// SeatClaimState is the reconciliation verdict for one claim id a catalog pin refers to
// (TKT-112). Three states, not a bool: "unknown" is a distinct outcome that must NOT be
// treated as dead — a pin naming a claim this database has never seen is the shape an
// inventory database restored behind catalog presents, and `hold:` pins cover CONFIRMED
// sales as well as live holds (confirm/finalize keep the pin, ADR-031 §3), so unpinning
// one can strip the protection from a sold seat. Unknown is reported, never reclaimed.
type SeatClaimState string

const (
	// SeatClaimLive — the claim still consumes its seats: held and unexpired,
	// finalizing, or confirmed (all three keep claim_seats.released_at NULL).
	SeatClaimLive SeatClaimState = "live"
	// SeatClaimDead — the claim reached a terminal state and its seats are released,
	// so its catalog pins are stale and reclaimable.
	SeatClaimDead SeatClaimState = "dead"
	// SeatClaimUnknown — no such claim in this database. Fail safe: leave the pin.
	SeatClaimUnknown SeatClaimState = "unknown"
)

// ReconcileSeatClaimStates is the liveness verdict behind `reconcile-pins` (TKT-112): one
// state per requested claim id, so the caller can never read a missing answer as permission
// to delete.
//
// It does NOT read the claims and report what it saw. It groups the ids by pool, takes each
// pool's row lock in ADR-010's usual order, and runs the ordinary lazy `sweepExpired` before
// classifying — so a "dead" verdict is a fact this call CREATED (status flipped to expired,
// `claim_seats.released_at` set) rather than a prediction about a row someone else may still
// change. That is what closes the lock-queue race: a finalize that BEGAN before the TTL
// elapsed and is queued behind us re-reads the claim under READ COMMITTED when the lock is
// granted, sees the terminal status, and refuses. A time-comparison verdict would lose that
// race, because `now()` is frozen at transaction start
// (docs/learnings/2026-07-16-lock-queue-time-cutoffs.md, TKT-78) — and the same freeze is why
// the opposite order is safe: a reconciler that queues across the boundary judges with
// stale-early time and reports the claim live, which errs toward keeping the pin.
//
// "Live" is any non-terminal claim status — held, finalizing, or confirmed. Confirmed is in
// that list because confirm/finalize deliberately KEEP the catalog pin (the seat is sold,
// ADR-031 §3), so treating it as anything else would unpin every sold seat.
func (p *Postgres) ReconcileSeatClaimStates(ctx context.Context, claimIDs []uuid.UUID) (map[uuid.UUID]SeatClaimState, error) {
	out := make(map[uuid.UUID]SeatClaimState, len(claimIDs))
	if len(claimIDs) == 0 {
		return out, nil
	}
	// Every requested id starts unknown; a pool pass can only upgrade it. An id whose claim
	// does not exist therefore stays unknown, which the caller treats as "leave the pin".
	byPool := map[uuid.UUID][]uuid.UUID{}
	poolOrder := []uuid.UUID{}
	for _, id := range claimIDs {
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = SeatClaimUnknown
		var pool uuid.UUID
		err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1`, id).Scan(&pool)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve pool for claim %s: %w", id, err)
		}
		if _, ok := byPool[pool]; !ok {
			poolOrder = append(poolOrder, pool)
		}
		byPool[pool] = append(byPool[pool], id)
	}
	for _, pool := range poolOrder {
		if err := p.classifySeatClaimsInPool(ctx, pool, byPool[pool], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// classifySeatClaimsInPool settles one pool's expiry under its row lock and records a verdict
// for each of that pool's requested claims. One transaction per pool, in the same
// lock-the-pool-first order as every other write (ADR-010), so this can never deadlock
// against a hold, a transition, or a capacity adjustment.
func (p *Postgres) classifySeatClaimsInPool(ctx context.Context, pool uuid.UUID, ids []uuid.UUID, out map[uuid.UUID]SeatClaimState) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return fmt.Errorf("lock pool %s: %w", pool, err)
	}
	if err = sweepExpired(ctx, tx, pool); err != nil {
		return fmt.Errorf("sweep pool %s: %w", pool, err)
	}
	verdicts := map[uuid.UUID]SeatClaimState{}
	for _, id := range ids {
		var status string
		err = tx.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1 AND pool_id=$2`, id, pool).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			continue // stays unknown
		}
		if err != nil {
			return fmt.Errorf("classify claim %s: %w", id, err)
		}
		// Dead is established POSITIVELY, from a terminal status — never inferred from a
		// failure to prove liveness (ai-review F1). The first cut derived dead as the inverse
		// of "has a live claim_seats row AND has a live status", which classified any live
		// claim with missing or already-released seat rows as dead and deleted its pin.
		// Nothing in the schema couples claim status to claim_seats.released_at, so that shape
		// is representable — a `hold:` pin naming a GA claim has no seat rows at all — and
		// those are the same degraded-data cases the unknown verdict deliberately fails closed
		// for. Failing open here contradicted that rule.
		//
		// The terminal statuses are safe to call dead without re-checking the child rows
		// because sweepExpired ran above in THIS transaction and its releaseSeatsForTerminal
		// releases every terminal claim's seats pool-wide. A held claim past its expiry has
		// already been flipped by that same sweep, so it cannot still be sitting here as
		// "held"; if one ever did, calling it live keeps the pin, which is the safe direction.
		switch status {
		case "held", "finalizing", "confirmed":
			verdicts[id] = SeatClaimLive
		case "expired", "released":
			verdicts[id] = SeatClaimDead
		default:
			// A status this binary does not know is not permission to delete.
			verdicts[id] = SeatClaimUnknown
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit pool %s reconciliation: %w", pool, err)
	}
	// Published only after the commit: an uncommitted sweep is not yet the fact the "dead"
	// verdict claims it is, and a caller that unpinned on a rolled-back verdict would have
	// freed a seat whose claim is still live.
	for id, state := range verdicts {
		out[id] = state
	}
	return nil
}

// SeatPinRef returns the pin coordinates for a claim's seats (for the release handler to
// unpin after a terminal transition). Empty (ok=false) for a GA claim / non-seated pool.
func (p *Postgres) SeatPinRef(ctx context.Context, org, claimID uuid.UUID) (PinRef, bool, error) {
	var seatMapID uuid.NullUUID
	err := p.db.QueryRowContext(ctx, `SELECT ip.seat_map_id FROM claims c JOIN inventory_pools ip ON ip.slot_id=c.pool_id
		WHERE c.id=$1 AND c.organizer_id=$2`, claimID, org).Scan(&seatMapID)
	if errors.Is(err, sql.ErrNoRows) || !seatMapID.Valid {
		return PinRef{}, false, nil
	}
	if err != nil {
		return PinRef{}, false, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT seat_identity FROM claim_seats WHERE claim_id=$1 ORDER BY seat_identity`, claimID)
	if err != nil {
		return PinRef{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var seats []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return PinRef{}, false, err
		}
		seats = append(seats, s)
	}
	if err = rows.Err(); err != nil {
		return PinRef{}, false, err
	}
	if len(seats) == 0 {
		return PinRef{}, false, nil
	}
	return PinRef{SeatMapID: seatMapID.UUID, Seats: seats, PinnedBy: pinnedBy(claimID)}, true, nil
}
