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

// SeatTakenError is ErrSeatTaken plus the identities that actually lost (TKT-173).
// A buyer whose selection partly collided has to re-render the seats they must give
// up, and "a seat was taken" cannot tell them which — so the losing set travels with
// the error. It is knowable ONLY here: the arbiter is the partial unique index inside
// the claim transaction, and any answer computed afterwards (by re-reading occupancy,
// say) describes a different moment and can name seats this transaction never lost.
type SeatTakenError struct {
	// Seats are the requested identities another live claim already holds, sorted.
	// Seats the request could have had are NOT listed: telling a buyer to release a
	// seat that was free is its own defect.
	Seats []string
}

func (e *SeatTakenError) Error() string {
	return ErrSeatTaken.Error() + ": " + strings.Join(e.Seats, ", ")
}

// Unwrap keeps every existing `errors.Is(err, ErrSeatTaken)` call site working —
// the handler's 409 mapping, and the contention suite, both predate this type.
func (e *SeatTakenError) Unwrap() error { return ErrSeatTaken }

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

	// Arbitrate BEFORE writing anything (ai-review). The pool row is held FOR UPDATE
	// and claim_seats_one_live_per_seat is scoped to (pool_id, seat_identity), so
	// every writer for this pool is serialised behind us: what this read sees is what
	// the insert would have hit, and no concurrent claim can slip a seat in between.
	// The unique index remains the backstop — this is an optimisation and a better
	// error, not the correctness boundary.
	//
	// Reading the whole contended set rather than stopping at the first is the point:
	// a buyer whose selection partly collided has to re-render every seat they must
	// give up, and the per-seat loop this replaces returned on the first violation.
	contended, err := contendedSeats(ctx, tx, slot, canon)
	if err != nil {
		return SeatHold{}, err
	}
	if len(contended) > 0 {
		return SeatHold{}, &SeatTakenError{Seats: contended}
	}

	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: qty, UnitAmount: unitAmount, Currency: currency, Status: "held", Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer') RETURNING expires_at,now()`,
		c.ID, org, slot, ticketType, qty, unitAmount, currency, p.ttl.String(), key, fp).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return SeatHold{}, err
	}
	// Every free seat inserts and the claim row is already written, so a losing
	// request does N doomed inserts before rolling back. That matters here and
	// nowhere else: this runs under the pool row lock, so it serialises every claim
	// on the performance while doing aborted heap and index work — and a request
	// mixing one known-taken seat with 49 free ones is an easy way to make that
	// expensive on purpose during an on-sale (ai-review). The check below moves the
	// arbitration BEFORE any write.
	inserted := map[string]struct{}{}
	seatRows, err := tx.QueryContext(ctx, `INSERT INTO claim_seats(claim_id,pool_id,seat_identity)
		SELECT $1, $2, s FROM unnest($3::text[]) AS s
		RETURNING seat_identity`, c.ID, slot, canon)
	if err != nil {
		// Unreachable while the pre-check above holds: it runs under the same pool row
		// lock that serialises every writer for this pool, so nothing can take a seat
		// between the two. Kept because the index is the correctness boundary and a
		// boundary that answers 500 is not one — if the pre-check is ever weakened, or
		// a path appears that writes claim_seats without the pool lock, this keeps the
		// refusal a 409 instead of an opaque server error. The identities are not
		// recoverable from the violation, so this is the coarse sentinel.
		if isUniqueViolation(err, "claim_seats_one_live_per_seat") {
			return SeatHold{}, ErrSeatTaken
		}
		return SeatHold{}, err
	}
	for seatRows.Next() {
		var seat string
		if err = seatRows.Scan(&seat); err != nil {
			_ = seatRows.Close()
			return SeatHold{}, err
		}
		inserted[seat] = struct{}{}
	}
	if err = seatRows.Err(); err != nil {
		_ = seatRows.Close()
		return SeatHold{}, err
	}
	if err = seatRows.Close(); err != nil {
		return SeatHold{}, err
	}
	if len(inserted) != len(canon) {
		return SeatHold{}, fmt.Errorf("seat insert wrote %d of %d rows", len(inserted), len(canon))
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "seat_hold", qty, qty, "held", nil, nil); err != nil {
		return SeatHold{}, err
	}
	if err = tx.Commit(); err != nil {
		return SeatHold{}, err
	}
	return SeatHold{Claim: c, SeatMapID: seatMapID.UUID, Seats: canon, PinnedBy: pinnedBy(c.ID), ExpiredPins: expiredPins}, nil
}

// SeatOccupancy is the buyer-facing answer to "which seats can I not have" on a
// seated slot (TKT-172). Unavailable is sorted and never nil — an empty seated pool
// answers with [], which is a different thing from an unknown slot (ErrNotFound).
// OfferingStatus mirrors Availability's (TKT-75): the seat list stays factual on a
// closed or archived slot, and this field is how a caller tells "these seats are
// free" from "nothing here is claimable at all".
// Available is how many more seats the pool will actually let anyone claim. It is
// NOT derivable from the seat list, and that is the point: a seated pool carries a
// coarse aggregate ceiling as well as its per-seat rows, and CreateSeatHold refuses
// with ErrUnavailable when confirmed + live held + requested exceeds it. So a seat
// can be absent from Unavailable — genuinely unheld — and still be unbuyable,
// because a draining capacity cut (target_capacity, TKT-76) has taken the pool's
// headroom to zero, or the map was authored with more seats than the venue snapshot
// the pool was provisioned with. Without this field the response says "free" about
// a seat every claim will reject, which is the one thing AC2 forbids.
// RemainingCapacity is a CEILING, not a seat count, and the name says so on
// purpose. Inventory does not hold the seat universe — that is the seat map, in
// catalog — so this cannot be "how many seats are free". It is the pool's own
// aggregate headroom under exactly the test CreateSeatHold applies, and it can be
// wrong in both directions if read as a seat count: 90 on a ten-seat map backed by
// a hundred-seat venue snapshot with every seat sold, and 0 on a map with free
// seats whose pool has been drained by a capacity cut.
//
// A picker must gate on BOTH this and Unavailable. Neither is sufficient alone,
// which is the whole reason both are here.
type SeatOccupancy struct {
	SlotID            uuid.UUID `json:"slot_id"`
	SeatMapID         uuid.UUID `json:"seat_map_id"`
	OfferingStatus    string    `json:"offering_status"`
	RemainingCapacity int32     `json:"remaining_capacity"`
	Unavailable       []string  `json:"unavailable_seat_identities"`
}

// seatOccupancyPoolQuery reads the pool facts this response needs. The projection
// deliberately mirrors CreateSeatHold's own lock-handshake SELECT rather than the
// availability read's: `available` on the public availability endpoint additionally
// subtracts unsold channel reservations, and the SEATED claim path does not — it
// admits against target_capacity/capacity alone. Quoting availability here would
// report 0 while the seat claim succeeds, and would make the comment claiming the
// two agree a lie (ai-review pass 2).
const seatOccupancyPoolQuery = `SELECT inventory_kind,seat_map_id,capacity,target_capacity,
		confirmed_quantity,lifecycle_status,closure_status,
		(SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND ` + liveClaims + `)
	FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`

// seatOccupancySeatsQuery is the live-seat projection, scoped to one pool. It is a
// const because the ADR-019 plan proof EXPLAINs this exact statement rather than a
// retyped copy — a copy is how one of the two drifts away from the other.
//
// The predicate is a CONJUNCTION and both halves are load-bearing, for different
// failure modes. Deleting either ships a wrong answer that no other test would see:
//
//   - `released_at IS NULL` alone reports a due-but-unswept held claim as occupying
//     its seats. Expiry is swept lazily, on the next seat hold against the pool
//     (sweepExpired), so on a quiet pool those rows can sit live for hours while the
//     seat is in fact claimable — the next claim sweeps them before its own insert.
//   - consumingClaims alone reports a FULLY REFUNDED seat as occupied forever. A full
//     refund sets claim_seats.released_at but leaves claims.status = 'confirmed'
//     (refund_returns.go — releaseSeatsForTerminal cannot help, it only touches
//     claims already in ('expired','released')), so a status-only predicate strands
//     the seat permanently with nothing to notice it.
//
// consumingClaims is reused rather than retyped for the same reason: it is the
// definition the claim path enforces, and it already covers the finalizing window
// that a naive status='held' check drops.
const seatOccupancySeatsQuery = `SELECT cs.seat_identity
	FROM claim_seats cs JOIN claims c ON c.id=cs.claim_id
	WHERE cs.pool_id=$1 AND cs.released_at IS NULL AND ` + consumingClaims + `
	ORDER BY cs.seat_identity`

// SeatOccupancy reads which seats a seated slot cannot currently sell, and how much
// aggregate headroom the pool has left.
//
// It takes no row locks and writes nothing — deliberately. It backs a cacheable
// public GET, and sweeping the expired holds it filters out would turn the read into
// a mutation contending for the pool row the on-sale path needs (ADR-010). Filtering
// gives the same answer without it.
//
// It does run in a READ ONLY REPEATABLE READ transaction, which is not the same
// thing as taking a lock. Under READ COMMITTED each statement gets its own snapshot,
// so a claim committing between the pool read and the seat read yields a payload
// that contradicts itself — headroom for one more seat, next to a list that already
// contains the seat that consumed it (ai-review pass 2). The response is allowed to
// be stale (ADR-004 resolves that at the atomic claim); it is not allowed to be
// internally inconsistent, because a picker has no way to reconcile the two halves.
// A read-only snapshot costs no locks and cannot serialization-fail.
func (p *Postgres) SeatOccupancy(ctx context.Context, org, slot uuid.UUID) (SeatOccupancy, error) {
	occ := SeatOccupancy{SlotID: slot, Unavailable: []string{}}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return occ, err
	}
	defer func() { _ = tx.Rollback() }()

	var kind, lifecycle, closure string
	var seatMapID uuid.NullUUID
	var capacity, confirmed int32
	var target sql.NullInt32
	var held int64
	err = tx.QueryRowContext(ctx, seatOccupancyPoolQuery, slot, org).
		Scan(&kind, &seatMapID, &capacity, &target, &confirmed, &lifecycle, &closure, &held)
	if errors.Is(err, sql.ErrNoRows) {
		return occ, ErrNotFound
	}
	if err != nil {
		return occ, err
	}
	if kind != "seated" || !seatMapID.Valid {
		return occ, ErrPoolKindMismatch
	}
	occ.SeatMapID = seatMapID.UUID
	occ.OfferingStatus = offeringStatus(lifecycle, closure)

	// The admission test, verbatim from CreateSeatHold: limit is target_capacity when
	// a cut is pending, else capacity, and a claim is refused once confirmed + live
	// held + requested exceeds it. So the headroom is what is left of that limit.
	limit := capacity
	if target.Valid {
		limit = target.Int32
	}
	if remaining := int64(limit) - int64(confirmed) - held; remaining > 0 {
		occ.RemainingCapacity = int32(remaining)
	}
	// A dead slot grants nothing whatever the arithmetic says — guardOffering refuses
	// every claim before the limit is even consulted.
	if occ.OfferingStatus != "open" {
		occ.RemainingCapacity = 0
	}

	rows, err := tx.QueryContext(ctx, seatOccupancySeatsQuery, slot)
	if err != nil {
		return occ, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seat string
		if err = rows.Scan(&seat); err != nil {
			return occ, err
		}
		occ.Unavailable = append(occ.Unavailable, seat)
	}
	return occ, rows.Err()
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// contendedSeats returns which of the requested identities another live claim already
// holds, sorted, under the caller's pool lock. Empty means the claim can proceed.
//
// `canon` is sorted, so the result is too. It uses the same partial-index predicate
// the unique constraint enforces (released_at IS NULL) — not claim status — because
// that index is the arbiter, and a status-based read would disagree with it exactly
// in the finalizing window.
func contendedSeats(ctx context.Context, tx *sql.Tx, pool uuid.UUID, canon []string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s FROM unnest($2::text[]) AS s
		WHERE EXISTS (SELECT 1 FROM claim_seats cs
		              WHERE cs.pool_id=$1 AND cs.seat_identity=s AND cs.released_at IS NULL)
		ORDER BY s`, pool, canon)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var seat string
		if err = rows.Scan(&seat); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
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
		var quantity, returned int32
		err = tx.QueryRowContext(ctx, `SELECT status,quantity,returned_quantity FROM claims WHERE id=$1 AND pool_id=$2`, id, pool).Scan(&status, &quantity, &returned)
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
			// A FULLY returned confirmed claim is dead, even though its status stays
			// confirmed (TKT-161). A refund releases such a claim's seats inside the
			// inventory transaction and then unpins in catalog after the commit; if that
			// unpin fails, ADR-031's fail-safe leaves the pin, which blocks seat-map
			// edits. Without this branch `reconcile-pins` could never reclaim it, because
			// the claim never reaches a terminal status — the leak would be permanent.
			//
			// Positively established, like the terminal statuses beside it: fully
			// returned means the claim consumes nothing and its seats were released in
			// the same transaction that recorded the return. A PARTIALLY returned claim
			// stays live, because it still holds every one of its seats — a partial
			// seated return is refused precisely because no subset can be identified.
			if status == "confirmed" && quantity > 0 && returned == quantity {
				// Accounting alone is not proof. The rule this file already states —
				// dead is established POSITIVELY, never inferred — applies to the
				// return path too: `returned_quantity` and `claim_seats.released_at`
				// are not coupled by the schema, so a repair, a restore skew or a
				// future defect can leave the counter full while seat rows are still
				// live. Deleting the pin then lets a seat-map edit orphan seats
				// inventory still holds. Confirm the child rows agree; if they do not,
				// fall through to live, which keeps the pin (ai-review pass 2).
				var liveSeats int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, id).Scan(&liveSeats); err != nil {
					return fmt.Errorf("count live seats for claim %s: %w", id, err)
				}
				if liveSeats == 0 {
					verdicts[id] = SeatClaimDead
					continue
				}
				// Fully returned AND still holding seats is a CONTRADICTION, not a live
				// claim, and calling it live would assert something false about it
				// (ai-review pass 3). Unknown is the verdict this file already reserves
				// for degraded data: it keeps the pin — the safe direction, since
				// deleting one orphans a sold seat — and `reconcile-pins` counts and
				// reports it under "investigate before unpinning them by hand" rather
				// than filing it silently among healthy claims.
				//
				// Deliberately NOT repaired here. Which side is true is exactly what is
				// unknown: if the seat row is right and the counter is wrong, releasing
				// the seat would destroy the true fact. A classification read does not
				// get to mutate its way out of an ambiguity.
				verdicts[id] = SeatClaimUnknown
				continue
			}
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
