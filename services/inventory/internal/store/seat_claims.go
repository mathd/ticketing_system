package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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

// ErrSeatOrphaned reports a selection that would strand a lone free seat (ADR-041).
var ErrSeatOrphaned = errors.New("selection would strand a seat with no free neighbour")

// SeatOrphanedError names the seats that WOULD be stranded — free seats the buyer did
// not ask for, which is why this cannot share `seat_taken`'s code on the wire: a caller
// validating that the identities are a subset of the request (TKT-173) would reject
// every valid orphan refusal as malformed. The two carry opposite relationships to the
// request.
type SeatOrphanedError struct {
	// Seats are the free seats this selection would isolate, sorted. They stay
	// SELECTABLE in a picker: adding one is the buyer's repair.
	Seats []string
}

func (e *SeatOrphanedError) Error() string {
	return ErrSeatOrphaned.Error() + ": " + strings.Join(e.Seats, ", ")
}

func (e *SeatOrphanedError) Unwrap() error { return ErrSeatOrphaned }

// orphanedSeatsQuery finds the seats a selection would strand, entirely in SQL and
// entirely within the caller's transaction and pool lock.
//
// The candidate set is the NEIGHBOURS OF THE REQUESTED SEATS, and that single choice
// carries two properties an earlier version got wrong (ai-review):
//
//   - **Only newly orphaned seats.** A seat can only become isolated by this claim if
//     one of its own neighbours is being taken now, so restricting candidates to
//     N(requested) makes "newly" structural rather than a filter. The earlier version
//     scanned every seat and tried to exclude pre-existing orphans with a predicate
//     that was tautological — it re-reported the same stranded seat on every later
//     claim in that row, for ever.
//   - **Bounded work under the lock.** At most two candidates per requested seat, each
//     reached through seat_claim_adjacency's (pool_id, seat_identity) primary key. The
//     earlier version scanned the whole pool while holding the row lock that serialises
//     every claimant on the performance — the worst place in the system to do O(map).
//
// `occupied(x)` is what will be taken once this claim commits: the requested set, plus
// anything already live under the same predicate the claim path enforces (see
// seatOccupancySeatsQuery). A seat is stranded when it is not occupied and every
// neighbour it HAS is — where "every neighbour it has" is load-bearing: NULL means no
// neighbour rather than unknown, so a row end has one and a one-seat row has none and
// is never strandable.
const orphanedSeatsQuery = `
WITH requested AS (SELECT DISTINCT unnest($2::text[]) AS id),
candidate AS (
	SELECT DISTINCT v.n AS id
	  FROM seat_claim_adjacency a
	  JOIN requested r ON r.id = a.seat_identity
	  CROSS JOIN LATERAL (VALUES (a.left_identity), (a.right_identity)) AS v(n)
	 WHERE a.pool_id = $1
	   AND v.n IS NOT NULL
	   AND NOT EXISTS (SELECT 1 FROM requested r2 WHERE r2.id = v.n)
),
-- Everything whose occupancy the answer can depend on: the candidates and their own
-- neighbours. Bounding this is what keeps the whole statement proportional to the
-- REQUEST rather than to the map.
scope AS (
	SELECT c.id FROM candidate c
	UNION
	SELECT n.v FROM candidate c
	  JOIN seat_claim_adjacency a ON a.pool_id = $1 AND a.seat_identity = c.id
	  CROSS JOIN LATERAL (VALUES (a.left_identity), (a.right_identity)) AS n(v)
	 WHERE n.v IS NOT NULL
),
occupied AS (
	SELECT id FROM requested
	UNION
	SELECT cs.seat_identity
	  FROM claim_seats cs JOIN claims cl ON cl.id = cs.claim_id
	 WHERE cs.pool_id = $1 AND cs.released_at IS NULL AND ` + consumingClaims + `
	   AND cs.seat_identity IN (SELECT id FROM scope)
)
SELECT c.id
  FROM candidate c
  JOIN seat_claim_adjacency a ON a.pool_id = $1 AND a.seat_identity = c.id
 WHERE NOT (a.left_identity IS NULL AND a.right_identity IS NULL)
   AND c.id NOT IN (SELECT id FROM occupied)
   AND (a.left_identity  IS NULL OR a.left_identity  IN (SELECT id FROM occupied))
   AND (a.right_identity IS NULL OR a.right_identity IN (SELECT id FROM occupied))
 ORDER BY c.id`

// projectionGapsQuery audits the slice of the projection this claim's answer rests on:
// the requested seats' rows, the rows of every seat they name as a neighbour, and
// whether those edges point back. It returns one identity per defect, or nothing.
//
// Bounded on purpose, and the bound is the honest limit. An edge pointing IN from a row
// the request never reaches — B names A while A does not name B — is invisible here,
// and finding it means scanning the pool, which is exactly the cost the bounded rewrite
// removed. That case is closed at provisioning instead (validateAdjacency), where the
// projection is built and the only place it legitimately changes.
const projectionGapsQuery = `
WITH requested AS (SELECT DISTINCT unnest($2::text[]) AS id),
needed AS (
	SELECT id FROM requested
	UNION
	SELECT v.n
	  FROM seat_claim_adjacency a
	  JOIN requested r ON r.id = a.seat_identity
	  CROSS JOIN LATERAL (VALUES (a.left_identity), (a.right_identity)) AS v(n)
	 WHERE a.pool_id = $1 AND v.n IS NOT NULL
)
SELECT n.id FROM needed n
 WHERE NOT EXISTS (SELECT 1 FROM seat_claim_adjacency a
                    WHERE a.pool_id = $1 AND a.seat_identity = n.id)
UNION
SELECT a.seat_identity
  FROM seat_claim_adjacency a
  JOIN needed n ON n.id = a.seat_identity
  CROSS JOIN LATERAL (VALUES (a.left_identity), (a.right_identity)) AS v(x)
  JOIN seat_claim_adjacency b ON b.pool_id = $1 AND b.seat_identity = v.x
 WHERE a.pool_id = $1 AND v.x IS NOT NULL
   AND a.seat_identity IS DISTINCT FROM b.left_identity
   AND a.seat_identity IS DISTINCT FROM b.right_identity
 LIMIT 5`

// ErrSeatProjectionIncomplete reports a rule-enabled pool whose adjacency projection
// cannot support a sound answer for this request.
//
// It is fail-CLOSED on purpose. The rule discovers candidates by reading adjacency rows,
// so a missing row yields no candidates, finds no orphans, and lets the claim commit —
// silently stranding a neighbour. A non-reciprocal edge is worse than incomplete: it can
// blame an unrelated claim for a seat that was already isolated. The bounded query
// assumes a complete, reciprocal projection, and the honest way to depend on that is to
// check it rather than hope (ai-review).
var ErrSeatProjectionIncomplete = errors.New("seat adjacency projection cannot answer this claim")

// validateAdjacency rejects a projection that is not internally consistent, at the one
// place it is written. Claim time can only audit what the request reaches; this sees the
// whole set, once, off the hot path.
//
// It proves INTERNAL CONSISTENCY, not fidelity to the seat map. A projection whose seats
// all name no neighbours is perfectly reciprocal, and nothing in this package can tell it
// apart from a map of genuine one-seat rows — the geometry that would settle it is not
// here. Fidelity is established where the adjacency is derived from that geometry
// (consumer.SeatMapAdjacency) and pinned by its tests; re-checking it here would mean
// re-deriving it from data the store does not have (ai-review).
func validateAdjacency(rows []SeatAdjacencyRow) error {
	byID := make(map[string]SeatAdjacencyRow, len(rows))
	for _, r := range rows {
		byID[r.SeatIdentity] = r
	}
	names := func(r SeatAdjacencyRow, id string) bool {
		return (r.Left != nil && *r.Left == id) || (r.Right != nil && *r.Right == id)
	}
	for _, r := range rows {
		for _, n := range []*string{r.Left, r.Right} {
			if n == nil {
				continue
			}
			other, ok := byID[*n]
			if !ok {
				return fmt.Errorf("%w: seat %q names neighbour %q, which has no row",
					ErrSeatProjectionIncomplete, r.SeatIdentity, *n)
			}
			if !names(other, r.SeatIdentity) {
				return fmt.Errorf("%w: seat %q names %q but %q does not name it back",
					ErrSeatProjectionIncomplete, r.SeatIdentity, *n, *n)
			}
		}
	}
	return validateAdjacencyOrder(rows)
}

// validateAdjacencyOrder checks the ordering half of a projection (TKT-81), on the same
// terms as the edges above: internal consistency at the moment it is written, which is the
// only thing this layer can establish. Fidelity to catalog's geometry is settled where the
// ordering is DERIVED (SeatMapAdjacency), because that is the only place the geometry
// exists -- re-checking it here would mean re-deriving it from data the store does not
// have (ADR-041's own statement of this limit, applied to the new columns).
//
// Three properties, each unsound rather than merely untidy if violated:
//
//   - All or none. A projection where some seats carry ordering and others do not makes
//     selection silently partial: the seats with no metadata are invisible to it, so a row
//     reads as shorter than it is and runs that exist are never offered.
//   - Both halves together. A position with no row does not say what it is a position in.
//     (The database CHECK says this too; a projection is rejected before it reaches SQL so
//     the error names the seat rather than a constraint.)
//   - Unique position within a row. The ordering is only deterministic if it is total, and
//     a selection that returns an arbitrary order among ties passes every test written on
//     a fixture that has none.
func validateAdjacencyOrder(rows []SeatAdjacencyRow) error {
	ordered := 0
	seen := map[string]map[int32]string{}
	rankOf := map[string]int32{}
	keyOf := map[int32]string{}
	for _, r := range rows {
		if r.RowKey == nil && r.Position == nil && r.RowRank == nil {
			continue
		}
		if r.RowKey == nil || r.Position == nil || r.RowRank == nil {
			return fmt.Errorf("%w: seat %q carries a partial ordering (row=%v rank=%v position=%v)",
				ErrSeatProjectionIncomplete, r.SeatIdentity, r.RowKey, r.RowRank, r.Position)
		}
		if strings.TrimSpace(*r.RowKey) == "" || *r.Position <= 0 || *r.RowRank <= 0 {
			return fmt.Errorf("%w: seat %q has an unusable ordering (row=%q rank=%d position=%d)",
				ErrSeatProjectionIncomplete, r.SeatIdentity, *r.RowKey, *r.RowRank, *r.Position)
		}
		ordered++
		if seen[*r.RowKey] == nil {
			seen[*r.RowKey] = map[int32]string{}
		}
		if other, dup := seen[*r.RowKey][*r.Position]; dup {
			return fmt.Errorf("%w: seats %q and %q share position %d in row %q",
				ErrSeatProjectionIncomplete, other, r.SeatIdentity, *r.Position, *r.RowKey)
		}
		seen[*r.RowKey][*r.Position] = r.SeatIdentity
		if prior, ok := rankOf[*r.RowKey]; ok && prior != *r.RowRank {
			return fmt.Errorf("%w: row %q carries ranks %d and %d — a row has one place in the order",
				ErrSeatProjectionIncomplete, *r.RowKey, prior, *r.RowRank)
		}
		rankOf[*r.RowKey] = *r.RowRank
		if prior, ok := keyOf[*r.RowRank]; ok && prior != *r.RowKey {
			return fmt.Errorf("%w: rank %d is claimed by rows %q and %q — two rows cannot share a place",
				ErrSeatProjectionIncomplete, *r.RowRank, prior, *r.RowKey)
		}
		keyOf[*r.RowRank] = *r.RowKey
	}
	if ordered != 0 && ordered != len(rows) {
		return fmt.Errorf("%w: %d of %d seats carry ordering metadata — a partly ordered projection hides the rows it omits",
			ErrSeatProjectionIncomplete, ordered, len(rows))
	}
	if ordered == 0 {
		return nil
	}
	// The ordering must DESCRIBE THE EDGES, not merely be well formed beside them (ai-review
	// pass 3). Everything above proves the ordering is internally tidy — unique positions, one
	// rank per row — and none of it makes the two descriptions of the same geometry agree.
	//
	// They are two descriptions: the edges say who sits next to whom, the positions say where
	// each seat sits, and selection reads only the positions while arbitration reads only the
	// edges. A projection with chain A-B-C-D-E-F-G and positions B=1, E=2, C=3 ... passes every
	// other check, and best-available then offers B and E as a two-seat run — they hold
	// consecutive positions and are four seats apart. The orphan filter cannot save it either:
	// it reasons in the same positional space, so it agrees they are neighbours. Executed, not
	// argued: that exact permutation sold B+E as "together" before this check existed.
	//
	// So: within each row, positions form 1..N, and the seat at position i names position i-1
	// as its left and i+1 as its right. That makes the two descriptions the same statement,
	// and it is checked here because this is where a projection is built — the claim path
	// cannot re-derive it from data it does not hold (ADR-041's own division of labour).
	byRowPos := map[string]map[int32]SeatAdjacencyRow{}
	for _, r := range rows {
		if byRowPos[*r.RowKey] == nil {
			byRowPos[*r.RowKey] = map[int32]SeatAdjacencyRow{}
		}
		byRowPos[*r.RowKey][*r.Position] = r
	}
	for rowKey, seats := range byRowPos {
		n := int32(len(seats))
		for pos := int32(1); pos <= n; pos++ {
			seat, ok := seats[pos]
			if !ok {
				return fmt.Errorf("%w: row %q has %d seats but no seat at position %d — positions must run 1..N",
					ErrSeatProjectionIncomplete, rowKey, n, pos)
			}
			names := func(edge *string, neighbourPos int32, side string) error {
				want, exists := seats[neighbourPos]
				switch {
				case !exists && edge != nil:
					return fmt.Errorf("%w: seat %q is at the %s end of row %q but names %q as its %s neighbour",
						ErrSeatProjectionIncomplete, seat.SeatIdentity, side, rowKey, *edge, side)
				case exists && edge == nil:
					return fmt.Errorf("%w: seat %q sits beside %q in row %q but names no %s neighbour",
						ErrSeatProjectionIncomplete, seat.SeatIdentity, want.SeatIdentity, rowKey, side)
				case exists && *edge != want.SeatIdentity:
					return fmt.Errorf("%w: seat %q names %q as its %s neighbour but position %d holds %q — the ordering and the adjacency describe different geometries",
						ErrSeatProjectionIncomplete, seat.SeatIdentity, *edge, side, neighbourPos, want.SeatIdentity)
				}
				return nil
			}
			if err := names(seat.Left, pos-1, "left"); err != nil {
				return err
			}
			if err := names(seat.Right, pos+1, "right"); err != nil {
				return err
			}
		}
	}
	return nil
}

// orphanedSeats runs the rule under the caller's pool lock. Empty means the selection
// strands nothing.
func orphanedSeats(ctx context.Context, tx *sql.Tx, pool uuid.UUID, canon []string) ([]string, error) {
	// The answer below is unsound, not merely incomplete, if the rows it reads are
	// missing or point the wrong way.
	gaps, err := tx.QueryContext(ctx, projectionGapsQuery, pool, canon)
	if err != nil {
		return nil, err
	}
	var bad []string
	for gaps.Next() {
		var id string
		if err = gaps.Scan(&id); err != nil {
			_ = gaps.Close()
			return nil, err
		}
		bad = append(bad, id)
	}
	if err = gaps.Err(); err != nil {
		_ = gaps.Close()
		return nil, err
	}
	if err = gaps.Close(); err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrSeatProjectionIncomplete, bad)
	}
	rows, err := tx.QueryContext(ctx, orphanedSeatsQuery, pool, canon)
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

// SeatAdjacencyRow is one seat's immediate neighbours in its row, as projected from the
// pool's exact published seat-map version (ADR-041). A nil neighbour is a row end.
//
// RowKey and Position are the ORDERING half, added for best-available selection
// (TKT-81 / ADR-061). The neighbour edges answer "given these seats, is anything
// stranded?"; they cannot answer "find me four seats together", because a linked list has
// no head to index and no order to sort by. Catalog's geometry carries both facts and
// inventory already reads them -- SeatMapAdjacency sorts each row by position and then
// discards the row and the position, keeping only the edges. These two fields keep them.
//
// Both nil means this projection predates ADR-061. That is a distinguishable state rather
// than a gap, in the same way orphan_prevention_enabled distinguishes a rule-off pool from
// one whose projection failed to load: best-available refuses such a pool with its own
// code, and the repair is a re-provision, not a smaller party.
type SeatAdjacencyRow struct {
	SeatIdentity string
	Left         *string
	Right        *string
	RowKey       *string
	Position     *int32
	// RowRank orders the ROWS, and it is separate from RowKey because identity and order
	// are two different facts. RowKey is the row's catalog uuid, which sorts arbitrarily;
	// ordering by it made "the first run in projected order" mean "the first run in a
	// random row". Derived from (section position, row position).
	RowRank *int32
}

// ProvisionSeated records a seated pool for a slot (catalog schema-4/5 seated
// publication). Idempotent on the event id, like Provision. capacity is the venue GA
// snapshot used as a coarse ceiling — the tight per-seat oversell boundary is the
// claim_seats unique index plus catalog PinSeat existence-validation (ADR-031).
//
// adjacency is non-empty only for a schema-5, rule-enabled publication (TKT-181). It
// is written in the SAME transaction as the pool and the consumed-event row, which is
// the property the whole design rests on: a pool that says the rule is on and has no
// projection is unrepresentable. Anything less would leave a rule-enabled pool that
// silently enforces nothing, and the consumed-event row would stop any later binary
// fixing it (ADR-041's rollout section).
func (p *Postgres) ProvisionSeated(ctx context.Context, eventID, slotID, organizerID, seatMapID uuid.UUID, capacity int32, orphanPrevention bool, adjacency []SeatAdjacencyRow) error {
	if err := validateAdjacency(adjacency); err != nil {
		return err
	}
	if capacity <= 0 {
		return fmt.Errorf("capacity must be positive")
	}
	if seatMapID == uuid.Nil {
		return fmt.Errorf("seated pool requires a seat map")
	}
	// Fail closed rather than provision a pool whose rule cannot be enforced.
	if orphanPrevention && len(adjacency) == 0 {
		return fmt.Errorf("orphan-prevention pool requires a seat adjacency projection")
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
	// ON CONFLICT DO NOTHING is right for a REPLAY and wrong for an UPGRADE, and the
	// difference is invisible from here — both arrive as "the pool already exists".
	//
	// ADR-041's correction wave re-emits, under a FRESH event id, performances that were
	// published against a rule-enabled map before the transport existed. Those pools were
	// provisioned at schema 4 and are rule-off. DO NOTHING would leave them rule-off,
	// insert the adjacency beside them, and mark the fresh event consumed — the organizer's
	// rule silently disabled for ever, which is precisely the failure the correction wave
	// exists to repair (ai-review).
	//
	// So: lock the row, verify it is the same seated pool, and turn the flag ON. Identity
	// is checked rather than assumed — adopting a pool that names a different organizer or
	// a different seat map would attach one map's adjacency to another map's seats.
	var existingKind string
	var existingOrg uuid.UUID
	var existingMap uuid.NullUUID
	err = tx.QueryRowContext(ctx, `SELECT inventory_kind, organizer_id, seat_map_id
		FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slotID).
		Scan(&existingKind, &existingOrg, &existingMap)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id,inventory_kind,seat_map_id,orphan_prevention_enabled)
			VALUES($1,$2,$3,$4,'seated',$5,$6)`, slotID, organizerID, capacity, eventID, seatMapID, orphanPrevention); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if existingKind != "seated" || existingOrg != organizerID || !existingMap.Valid || existingMap.UUID != seatMapID {
			return fmt.Errorf("%w: pool %s is not the seated pool this publication describes", ErrPoolKindMismatch, slotID)
		}
		// Monotonic: a correction wave turns the rule on, and a stale schema-4 replay
		// must never turn it back off.
		if orphanPrevention {
			if _, err = tx.ExecContext(ctx,
				`UPDATE inventory_pools SET orphan_prevention_enabled=true WHERE slot_id=$1`, slotID); err != nil {
				return err
			}
		}
	}
	// The conflict clause is ASYMMETRIC, and the asymmetry is the point (TKT-81).
	//
	// DO NOTHING is right for a replay and wrong for an upgrade -- the same distinction
	// the pool row above has to make, and invisible here for the same reason: both arrive
	// as "the row already exists". A pool provisioned before ADR-061 carries adjacency
	// edges and no ordering metadata, and the only thing that can supply that metadata is
	// a re-provision carrying real catalog geometry. Under DO NOTHING that re-provision is
	// a silent no-op on every row, so the pool stays unselectable for ever while the
	// correction wave reports success.
	//
	// So the ordering columns take EXCLUDED, and the neighbour edges deliberately do not.
	// The edges are the substrate ADR-041 arbitrates on and fail-closes against; a live
	// claim was decided against the edges as they stood, and a later publication that
	// rewrote them would retroactively change what that decision meant. The ordering
	// columns are additive, read only by selection, and carry no such history. Changing
	// this line to update the edges too is not a cleanup -- it is a different decision
	// about immutability, and belongs in an ADR.
	// Refuse a projection that is not the SAME SET as the one already stored (ai-review).
	//
	// The upsert above fills ordering metadata on an existing pool and leaves the arbitration
	// edges alone, which is right when both publications describe the same geometry. If they
	// do not — a seat added, a seat dropped, an edge changed — a column-wise merge would
	// splice one generation's ordering onto another's edges and leave a projection neither
	// input describes: rows omitted by the new set survive and stay selectable, and rows it
	// adds name neighbours that were never updated to name them back.
	//
	// ADR-029 already makes that unreachable through the ordinary path: a published seat-map
	// version is immutable, a version IS a seat_maps row, and the identity check above refuses
	// any publication naming a different seat_map_id. So this is defence in depth against a
	// catalog integrity violation rather than a live defect — but the failure it prevents is
	// silent projection poisoning on the correction-wave path, which per ADR-021 is exactly
	// where an honest-writer guarantee should be checked rather than assumed.
	//
	// Scoped to publications that CARRY a projection. A schema-4 replay arrives with none
	// and is not describing a geometry at all, so comparing it against the stored set would
	// read "I said nothing" as "I said something different" and refuse a legal replay.
	var storedSeats int
	if err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1`, slotID).Scan(&storedSeats); err != nil {
		return err
	}
	if storedSeats > 0 && len(adjacency) > 0 {
		if storedSeats != len(adjacency) {
			return fmt.Errorf("%w: pool %s has a %d-seat projection and this publication describes %d",
				ErrSeatProjectionIncomplete, slotID, storedSeats, len(adjacency))
		}
		for _, a := range adjacency {
			var l, r, storedKey sql.NullString
			var storedPos, storedRank sql.NullInt32
			err = tx.QueryRowContext(ctx,
				`SELECT left_identity, right_identity, row_key, position, row_rank FROM seat_claim_adjacency WHERE pool_id=$1 AND seat_identity=$2`,
				slotID, a.SeatIdentity).Scan(&l, &r, &storedKey, &storedPos, &storedRank)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: pool %s has no projection row for seat %q, which this publication describes",
					ErrSeatProjectionIncomplete, slotID, a.SeatIdentity)
			}
			if err != nil {
				return err
			}
			same := func(stored sql.NullString, incoming *string) bool {
				if incoming == nil {
					return !stored.Valid
				}
				return stored.Valid && stored.String == *incoming
			}
			if !same(l, a.Left) || !same(r, a.Right) {
				return fmt.Errorf("%w: pool %s already holds different adjacency for seat %q — this publication describes another geometry",
					ErrSeatProjectionIncomplete, slotID, a.SeatIdentity)
			}
			// And the ORDERING, where the pool already has some (ai-review pass 2).
			// Comparing only identities and edges left the door the guard exists to close:
			// a publication carrying the same chain with permuted row/position values passed
			// every check and then overwrote the ordering, so two seats that are not
			// neighbours could be returned as a contiguous run. Where the stored ordering is
			// NULL this is the upgrade path and any incoming ordering is accepted, which is
			// the whole point of the upsert.
			if storedKey.Valid {
				if a.RowKey == nil || a.Position == nil || a.RowRank == nil ||
					storedKey.String != *a.RowKey || storedPos.Int32 != *a.Position || storedRank.Int32 != *a.RowRank {
					return fmt.Errorf("%w: pool %s already holds a different ordering for seat %q — this publication describes another geometry",
						ErrSeatProjectionIncomplete, slotID, a.SeatIdentity)
				}
			}
		}
	}
	for _, a := range adjacency {
		if _, err = tx.ExecContext(ctx, `INSERT INTO seat_claim_adjacency(pool_id,seat_identity,left_identity,right_identity,row_key,position,row_rank)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(pool_id,seat_identity) DO UPDATE
			SET row_key = EXCLUDED.row_key, position = EXCLUDED.position, row_rank = EXCLUDED.row_rank`,
			slotID, a.SeatIdentity, a.Left, a.Right, a.RowKey, a.Position, a.RowRank); err != nil {
			return err
		}
	}
	return p.commitAvailability(tx, slotID)
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
	var orphanPrevention bool
	// closure_status stays last before FROM (lock-handshake pattern; see CreateHold).
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,seat_map_id,orphan_prevention_enabled,closure_status
		FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).
		Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &seatMapID, &orphanPrevention, &closure)
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
	// Scoped to the PUBLIC namespace explicitly (TKT-246, ai-review pass 3).
	//
	// Migration 0016 deliberately made (organizer_id, idempotency_key) non-unique across
	// reseller scopes, so an unscoped lookup here can match a RESELLER's GA claim: a
	// public seated request reusing that key would be refused ErrIdempotency by a row it
	// has nothing to do with, and with several such rows QueryRow picks an unspecified
	// one. That is the cross-namespace denial 0016 exists to prevent, surviving in the
	// reader that was not audited when the constraint changed.
	//
	// Seated holds are public-only today — there is no seated partner surface (TKT-176
	// owns that seam) — so the scope is a literal NULL rather than a parameter. When a
	// seated partner sale arrives, this becomes the same IS NOT DISTINCT FROM predicate
	// CreateHold uses, and the compiler will not remind anyone: that is what this
	// comment is for.
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),request_fingerprint,COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,'')
		FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 AND reseller_scope IS NULL`, org, key).
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
			if err = p.commitAvailability(tx, slot); err != nil {
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
		return SeatHold{Claim: existing, SeatMapID: seatMapID.UUID, Seats: canon, PinnedBy: pinnedBy(existing.ID), Replay: true}, p.commitAvailability(tx, slot)
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

	// The orphan rule, and ONLY when the pool carries it: a rule-off pool must run no
	// extra statement at all (ADR-041's AC4), which is why this is inside the branch
	// rather than a query that returns nothing.
	//
	// It runs here — after the expiry sweep, after contention, under the pool lock,
	// before any write. All four matter. Before the sweep it would count seats whose
	// holds have already lapsed; before contention it would compute against a selection
	// that is about to be refused anyway; outside the lock it would be advisory, and
	// two claimants could each take a legal seat and jointly strand a third.
	if orphanPrevention {
		stranded, oerr := orphanedSeats(ctx, tx, slot, canon)
		if oerr != nil {
			return SeatHold{}, oerr
		}
		if len(stranded) > 0 {
			return SeatHold{}, &SeatOrphanedError{Seats: stranded}
		}
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
	if err = p.commitAvailability(tx, slot); err != nil {
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
	if err = p.commitAvailability(tx, pool); err != nil {
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

// MaxBestAvailableScan bounds how many projected seats one best-available request reads
// before giving up (TKT-81 / ADR-061).
//
// The cap exists because the scan runs under the pool row lock that serialises every
// claimant on the performance, and the expensive case is not the exotic one: once a show
// is nearly sold out, EVERY request is a request that finds no run, and that is exactly
// when contention peaks. An uncapped scan is O(map) on the platform's hottest lock, which
// is the cost ADR-041 rejected when it rewrote orphanedSeatsQuery for the same reason.
//
// 400 = 8x MaxSeatsPerHold. Wide enough that the largest legal party still has real room
// to find a run, and wide enough that a request is never capped before exhausting the row
// it started in on any realistic house. It is a constant rather than a tuning knob because
// a per-pool value would be a promise about latency this system cannot keep.
//
// What it buys is bounded work; what it costs is honesty about the semantics. Best
// available means "the first eligible run within the first MaxBestAvailableScan seats in
// projected order", not "the best run in the house". That is stated here, in ADR-061, and
// in the OpenAPI description, because a caller who believes the second will read a refusal
// as a sellout.
const MaxBestAvailableScan = 400

// maxBestAvailableScanSQL is MaxBestAvailableScan as it appears INSIDE bestAvailableRunQuery.
// The two are pinned equal by TestBestAvailableScanCapConstantsAgree — a Go const cannot be
// interpolated into another const, and a silent divergence would make the shipped bound
// differ from the documented one with nothing to notice.
const maxBestAvailableScanSQL = "LIMIT 400"

var (
	// ErrBestAvailableUnavailable reports that this pool cannot seat a party of N right
	// now: no free run that long within the scanned window. RETRYABLE, and a smaller
	// party may well succeed — the caller should offer fewer seats, not report a sellout.
	ErrBestAvailableUnavailable = errors.New("no contiguous run of that size is available")
	// ErrBestAvailableUnsupported reports that this pool has no ordering projection, so
	// best-available cannot be answered here AT ALL. Deterministic: it will never succeed,
	// for any N, until the pool is re-provisioned with ADR-061 geometry.
	//
	// Distinct from ErrBestAvailableUnavailable on purpose, and the distinction is the same
	// one ADR-041 drew between a rule-off pool and a pool whose projection failed to load:
	// one is a property of the request, the other is an operational defect, and collapsing
	// them makes a broken pool look like a sold-out show to everyone who could fix it.
	ErrBestAvailableUnsupported = errors.New("this slot does not support best-available selection")
)

// bestAvailableRunQuery finds the first free contiguous run of $3 seats in the pool's
// projected order, and returns its seat identities in that order.
//
// The shape, and why each part is the way it is:
//
//   - **`ordered`** reads the projection through (pool_id, row_key, position) — the index
//     migration 0018 adds — ordered, and LIMITed to $4. The ORDER BY is served by the
//     index, so the LIMIT terminates the scan rather than sorting the pool and then
//     discarding: that is the difference between a bounded read and an O(map) read wearing
//     a bound, and it is why the plan test asserts the absence of a Sort node rather than
//     merely the presence of an index.
//   - **`free`** marks each scanned seat as takeable or not under exactly the claim path's
//     own liveness predicate (released_at IS NULL AND consumingClaims), evaluated after the
//     caller's sweepExpired — so a seat whose hold has lapsed counts as free here, as it
//     does everywhere else.
//   - **`grp`** is the standard gaps-and-islands trick: `position - row_number()` over the
//     free seats of a row is constant exactly across a maximal run of consecutive
//     positions. It groups runs without a recursive term, which matters because the
//     recursive alternative is the linked-list walk this whole design exists to avoid.
//   - **`run`** keeps only runs long enough, and `windows` slides a window of exactly $3
//     seats along each of them. A run of 6 offers four windows for a party of 3; the extra
//     windows are what let the orphan filter reject one without refusing the request.
//   - **`legal`** drops any window whose placement would strand a seat. A seat is stranded
//     when it is free, is not in the window, and every neighbour it HAS is either taken or
//     inside the window. Only the two seats flanking a window can be newly stranded by it —
//     the same "candidates are the neighbours of the selection" bound ADR-041 established,
//     which is what keeps this proportional to the window rather than the row. NULL
//     neighbour means no neighbour, so a row end has one and a one-seat row has none.
//     Pre-existing orphans are untouched: a seat already isolated fails the "free" test on
//     at least one side regardless of this window, and refusing on its account would poison
//     every later claim in the row for ever (ADR-041's "only NEWLY orphaned" rule).
//   - The final ORDER BY picks the earliest legal window in projected order, and re-emits
//     its seats in position order so the caller receives a run, not a set.
//
// $1 pool, $2 orphan rule on, $3 party size. The scan cap is INTERPOLATED, not bound:
// it is a compile-time constant, and Postgres folds a LIMIT parameter into the generic
// plan, which would defeat the ADR-019 plan probe that has to EXPLAIN this exact statement.
const bestAvailableRunQuery = `
WITH ordered AS (
	SELECT a.seat_identity, a.row_key, a.row_rank, a.position, a.left_identity, a.right_identity
	  FROM seat_claim_adjacency a
	 WHERE a.pool_id = $1 AND a.row_key IS NOT NULL
	 ORDER BY a.row_rank, a.row_key, a.position
	 LIMIT 400
),
-- CONTEXT, not candidates. The orphan predicate asks about seats one and two positions
-- outside a window, so a window sitting at the very edge of the scan has flanks whose own
-- neighbours fall past the cap. Judging those with only the scanned set in hand meant treating
-- "I cannot see it" as "it is taken", which refused legal runs at the boundary -- a bounded
-- scan is allowed to stop looking for candidates, but it must not start inventing
-- occupancy (ai-review).
--
-- Two extra positions per row is exactly the predicate's reach: it inspects position lo-1
-- and hi+1 (the flanks) and lo-2 / hi+2 (each flank's other side), and lo-1/hi+1 are inside
-- the scan whenever the window is. Bounded by the same argument as the scan itself -- at
-- most two rows' worth of extra rows, each reached through the same index.
context AS (
	SELECT o.seat_identity, o.row_key, o.row_rank, o.position, o.left_identity, o.right_identity FROM ordered o
	UNION
	SELECT a.seat_identity, a.row_key, a.row_rank, a.position, a.left_identity, a.right_identity
	  FROM seat_claim_adjacency a
	  JOIN (SELECT row_key, min(position) AS lo, max(position) AS hi FROM ordered GROUP BY row_key) b
	    ON b.row_key = a.row_key
	 WHERE a.pool_id = $1 AND a.row_key IS NOT NULL
	   AND (a.position IN (b.lo - 2, b.lo - 1, b.hi + 1, b.hi + 2))
),
taken AS (
	SELECT cs.seat_identity
	  FROM claim_seats cs JOIN claims cl ON cl.id = cs.claim_id
	 WHERE cs.pool_id = $1 AND cs.released_at IS NULL AND ` + consumingClaims + `
),
free AS (
	SELECT o.* FROM ordered o
	 WHERE NOT EXISTS (SELECT 1 FROM taken t WHERE t.seat_identity = o.seat_identity)
),
-- The same freeness test over the wider set. Used ONLY by the orphan predicate: a seat
-- outside the scan may be someone's neighbour but is never itself selectable, which is what
-- keeps the cap meaningful.
free_context AS (
	SELECT c.* FROM context c
	 WHERE NOT EXISTS (SELECT 1 FROM taken t WHERE t.seat_identity = c.seat_identity)
),
grp AS (
	SELECT f.*, f.position - row_number() OVER (PARTITION BY f.row_key ORDER BY f.position) AS island
	  FROM free f
),
windows AS (
	SELECT g.row_key, g.row_rank, g.position AS start_position,
	       array_agg(g2.seat_identity ORDER BY g2.position) AS seats,
	       min(g2.position) AS lo, max(g2.position) AS hi
	  FROM grp g
	  JOIN grp g2 ON g2.row_key = g.row_key AND g2.island = g.island
	              AND g2.position >= g.position AND g2.position < g.position + $3
	 GROUP BY g.row_key, g.row_rank, g.island, g.position
	HAVING count(*) = $3
),
legal AS (
	SELECT w.* FROM windows w
	 WHERE NOT $2 OR NOT EXISTS (
		-- The only seats this window can newly strand are the two flanking it: a seat
		-- becomes isolated only when one of its own neighbours is taken now (ADR-041).
		-- n is such a flank (it comes from grp, so it is FREE by construction), and it
		-- is stranded when its OTHER side is not free either -- position lo-2 for the left
		-- flank, hi+2 for the right. That side is unavailable when no FREE seat sits there,
		-- which covers both "taken" and "row ends here". A seat with no edges at all is a
		-- one-seat row and is never strandable, which is the NOT(both NULL) guard.
		SELECT 1 FROM free_context n
		 WHERE n.row_key = w.row_key
		   AND n.position IN (w.lo - 1, w.hi + 1)
		   AND NOT (n.left_identity IS NULL AND n.right_identity IS NULL)
		   AND NOT EXISTS (
			SELECT 1 FROM free_context o
			 WHERE o.row_key = n.row_key
			   AND o.position = CASE WHEN n.position < w.lo THEN n.position - 1 ELSE n.position + 1 END
		   )
	   )
),
chosen AS (
	SELECT row_key, row_rank, start_position, seats FROM legal ORDER BY row_rank, row_key, start_position LIMIT 1
)
-- Unnested back into rows rather than returned as text[]: pgx's database/sql path has no
-- []string scanner, and adding an array driver for one column is a dependency this does not
-- need. WITH ORDINALITY preserves the array's order, which IS the run's order.
SELECT s.seat FROM chosen c, unnest(c.seats) WITH ORDINALITY AS s(seat, ord) ORDER BY s.ord`

// bestAvailableFingerprint binds an idempotency key to the INTENT of a best-available
// request rather than to its outcome (TKT-81).
//
// This is the one structural difference from seatFingerprint, and it is forced: a
// best-available request does not carry seats, so there is nothing outcome-shaped to hash
// at the moment the key must be resolved. Hashing the party size instead means a retry
// under the same key is recognised as the same request, replays the ORIGINAL claim, and
// re-reads the seats it already selected from claim_seats -- rather than running selection
// a second time and handing out a second run under a key the caller believes names one
// hold.
//
// The "best:" prefix is not decoration. Without a mode discriminator a named-seat request
// and a best-available request under one key hash over different tuples that could
// coincide, and one would replay the other's claim; with it, they collide as
// ErrIdempotency, which is the correct answer to reusing a spent key for a different kind
// of request.
func bestAvailableFingerprint(org, slot, ticketType uuid.UUID, count int32, unitAmount int64, currency string) string {
	s := fmt.Sprintf("best:%s:%s:%s:%d:%d:%s", org, slot, ticketType, count, unitAmount, currency)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// claimedSeats reads back the seats a claim holds, in projected order where the projection
// has one and by identity otherwise, and separately reports how many are still LIVE. It
// exists for the best-available replay: the request cannot reconstruct the seat set, so the
// row is the only source of truth for what a retry is a retry OF.
//
// It returns every row, released or not, and lets the caller decide — for the reason the
// refund path states where it releases them: `claims.status` and `claim_seats.released_at`
// are not coupled by the schema, so "the claim is confirmed" and "its seats are still
// consumed" are two different questions. A reader that answers the second by filtering the
// first silently reports an empty seat set for a fully returned claim.
func claimedSeats(ctx context.Context, tx *sql.Tx, pool, claimID uuid.UUID) ([]string, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT cs.seat_identity, (cs.released_at IS NULL) AS live
		FROM claim_seats cs
		LEFT JOIN seat_claim_adjacency a
		       ON a.pool_id = cs.pool_id AND a.seat_identity = cs.seat_identity
		WHERE cs.claim_id = $1 AND cs.pool_id = $2
		ORDER BY a.row_rank NULLS LAST, a.row_key NULLS LAST, a.position NULLS LAST, cs.seat_identity`, claimID, pool)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	live := 0
	for rows.Next() {
		var seat string
		var isLive bool
		if err = rows.Scan(&seat, &isLive); err != nil {
			return nil, 0, err
		}
		out = append(out, seat)
		if isLive {
			live++
		}
	}
	return out, live, rows.Err()
}

// CreateBestAvailableSeatHold selects and claims a contiguous run of `count` seats in ONE
// transaction under the pool row lock (TKT-81 / ADR-061, AC1).
//
// The single-transaction property is the whole ticket. Selection outside the lock is
// advisory: two claimants would each pick the same free run, and one of them would lose at
// the insert -- or, worse, they would pick overlapping runs and jointly strand what was
// between them. Here the pool row is held FOR UPDATE before anything is read, so what
// selection sees is what the insert gets, and every other claimant on this performance is
// behind us.
//
// The order of operations is CreateSeatHold's, deliberately and in the same sequence,
// because each position was argued for there: lock, then replay, then guard the offering,
// then sweep expiry, then the aggregate ceiling, then -- where the named-seat path
// arbitrates a given set -- CHOOSE a set, then write. Two consequences of that placement
// are worth naming. Selection runs after sweepExpired, so a seat whose hold lapsed is
// selectable rather than invisible. And the ceiling runs before selection, so a drained
// pool refuses on headroom without paying for a scan, and never reports "no run" about a
// map that is full of runs.
//
// There is no retry loop, and its absence is a design statement rather than an omission
// (AC3's "bounded retries"). Serialisation happens in the lock queue, so by the time this
// transaction reads the projection there is nothing left to race with; a retry could only
// fire on a conflict that cannot occur, and an unbounded one would turn a sold-out pool
// into a lock convoy.
func (p *Postgres) CreateBestAvailableSeatHold(ctx context.Context, org, slot, ticketType uuid.UUID, count int32, unitAmount int64, currency, key string) (SeatHold, error) {
	if count <= 0 || count > MaxSeatsPerHold {
		return SeatHold{}, ErrSeatSetInvalid
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
	var orphanPrevention bool
	// closure_status stays last before FROM (lock-handshake pattern; see CreateHold).
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity,lifecycle_status,inventory_kind,seat_map_id,orphan_prevention_enabled,closure_status
		FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).
		Scan(&capacity, &confirmed, &target, &lifecycle, &kind, &seatMapID, &orphanPrevention, &closure)
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
	fp := bestAvailableFingerprint(org, slot, ticketType, count, unitAmount, currency)

	// Replay BEFORE selection, and this ordering is the safety property, not a shortcut.
	// Selecting first and then discovering the key was spent would already have read a run
	// as available that the original claim holds -- harmless here only because the
	// transaction rolls back, and a trap for anyone who later moves a write above it.
	//
	// Scoped to the PUBLIC namespace explicitly, for the reason CreateSeatHold's comment
	// gives: 0016 made (organizer_id, idempotency_key) non-unique across reseller scopes.
	var existing Claim
	var existingFP string
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,expires_at,now(),request_fingerprint,COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,'')
		FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 AND reseller_scope IS NULL`, org, key).
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
			if err = p.commitAvailability(tx, slot); err != nil {
				return SeatHold{}, err
			}
			return SeatHold{}, ErrConflict
		}
		if existing.Status == "released" || existing.Status == "expired" {
			return SeatHold{}, ErrConflict
		}
		// The seats come from the ROW, never from a second selection. This is the
		// difference that makes a best-available retry safe: the request carries a party
		// size, so re-deriving a set from it would produce a DIFFERENT run and hand the
		// caller two holds under one key.
		//
		// Read RELEASED rows too, and then decide (ai-review). `claims.status` and
		// `claim_seats.released_at` are not coupled by the schema — the refund path says so
		// in as many words — so a FULLY RETURNED seated claim sits at status 'confirmed'
		// with every seat row released. Filtering on live rows made that replay answer with
		// the original quantity and an EMPTY seat set, which the caller then tried to pin.
		seats, live, serr := claimedSeats(ctx, tx, slot, existing.ID)
		if serr != nil {
			return SeatHold{}, serr
		}
		// EVERY seat the claim named must still be live, not merely one of them (ai-review
		// pass 2). The first version of this guard checked `live == 0`, which is the fully
		// returned case and only that; a claim with one seat released and two still held
		// would replay all three, and the caller would pin a seat that has since been
		// reallocated -- reporting an allocation the claim does not hold, or provoking a
		// deterministic pin rejection that releases the seats it does.
		//
		// The schema permits that skew deliberately (the refund path says so where it
		// releases rows), so partial liveness is a state to REFUSE, not one to interpret.
		// A replay must answer with the original allocation or not at all.
		if live != len(seats) || int32(live) != existing.Quantity {
			return SeatHold{}, ErrConflict
		}
		existing.Kind = "buyer"
		return SeatHold{Claim: existing, SeatMapID: seatMapID.UUID, Seats: seats, PinnedBy: pinnedBy(existing.ID), Replay: true}, p.commitAvailability(tx, slot)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SeatHold{}, err
	}

	if err = guardOffering(lifecycle, closure); err != nil {
		return SeatHold{}, err
	}
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
	if int64(confirmed)+int64(held)+int64(count) > int64(limit) {
		return SeatHold{}, ErrUnavailable
	}

	// A pool with no ordering projection cannot be selected over, and saying so is a
	// different answer from "cannot seat your party" -- one is repaired by re-provisioning,
	// the other by asking for fewer seats. Checked BEFORE the scan so an unsupported pool
	// is answered without touching the projection at all, and checked as its own predicate
	// rather than inferred from an empty result, which is what would make the two
	// indistinguishable again.
	var ordered bool
	if err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM seat_claim_adjacency WHERE pool_id=$1 AND row_key IS NOT NULL)`, slot).
		Scan(&ordered); err != nil {
		return SeatHold{}, err
	}
	if !ordered {
		return SeatHold{}, ErrBestAvailableUnsupported
	}

	canon, err := bestAvailableRun(ctx, tx, slot, orphanPrevention, count, p.bestAvailableScan())
	if err != nil {
		return SeatHold{}, err
	}

	// The named-seat path's own guards, run on the set this transaction chose. Neither can
	// fire while the lock is held and the selection query is correct -- which is precisely
	// why they are here. contendedSeats re-asserts against claim_seats that nothing chosen
	// is live, and orphanedSeats re-runs projectionGapsQuery, the fail-closed audit that
	// refuses an unsound projection. A selection built on a projection with a missing or
	// one-way edge is wrong in a way no amount of correct SQL downstream can detect, and
	// the cost of both checks is proportional to the party, not the map.
	contended, err := contendedSeats(ctx, tx, slot, canon)
	if err != nil {
		return SeatHold{}, err
	}
	if len(contended) > 0 {
		return SeatHold{}, &SeatTakenError{Seats: contended}
	}
	if orphanPrevention {
		stranded, oerr := orphanedSeats(ctx, tx, slot, canon)
		if oerr != nil {
			return SeatHold{}, oerr
		}
		if len(stranded) > 0 {
			// Unreachable while `legal` filters correctly: the query excludes exactly the
			// windows this would refuse. Kept as the boundary, not the belt -- if the two
			// ever disagree the honest answer is a refusal, not a stranded seat, and a
			// caller reading `orphaned_seats` learns which seats to add.
			return SeatHold{}, &SeatOrphanedError{Seats: stranded}
		}
	}

	c := Claim{ID: uuid.New(), OrganizerID: org, PoolID: slot, TicketTypeID: ticketType, Quantity: count, UnitAmount: unitAmount, Currency: currency, Status: "held", Kind: "buyer"}
	err = tx.QueryRowContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
		VALUES($1,$2,$3,$4,$5,$6,$7,'held',now()+$8::interval,$9,$10,'buyer') RETURNING expires_at,now()`,
		c.ID, org, slot, ticketType, count, unitAmount, currency, p.ttl.String(), key, fp).Scan(&c.ExpiresAt, &c.ServerTime)
	if err != nil {
		return SeatHold{}, err
	}
	inserted := 0
	seatRows, err := tx.QueryContext(ctx, `INSERT INTO claim_seats(claim_id,pool_id,seat_identity)
		SELECT $1, $2, s FROM unnest($3::text[]) AS s
		RETURNING seat_identity`, c.ID, slot, canon)
	if err != nil {
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
		inserted++
	}
	if err = seatRows.Err(); err != nil {
		_ = seatRows.Close()
		return SeatHold{}, err
	}
	if err = seatRows.Close(); err != nil {
		return SeatHold{}, err
	}
	if inserted != len(canon) {
		return SeatHold{}, fmt.Errorf("seat insert wrote %d of %d rows", inserted, len(canon))
	}
	if err = appendHistory(ctx, tx, org, c.ID, nil, "create", "buyer", "seat_hold", count, count, "held", nil, nil); err != nil {
		return SeatHold{}, err
	}
	if err = p.commitAvailability(tx, slot); err != nil {
		return SeatHold{}, err
	}
	// Seats are returned in PROJECTED order, which is the order the query emitted them in
	// and the order a buyer reads a row -- not canonicalSeats' lexical sort. A run is a
	// sequence, and "seats 9, 10, 11" sorted lexically reads as 10, 11, 9.
	return SeatHold{Claim: c, SeatMapID: seatMapID.UUID, Seats: canon, PinnedBy: pinnedBy(c.ID), ExpiredPins: expiredPins}, nil
}

// bestAvailableScan is the scan cap this store applies, MaxBestAvailableScan unless a test
// has narrowed it. The seam exists because the cap governs exactly one behaviour -- which
// seats the bounded scan ADMITS, and therefore what the projected scan order is actually
// load-bearing for -- and that behaviour is unobservable on any fixture smaller than the
// cap. A test that cannot reach the cap cannot show the ordering matters, and a 400-seat
// fixture in a suite that runs per-schema is a poor trade for the same proof.
func (p *Postgres) bestAvailableScan() int32 {
	if p.baScan > 0 {
		return p.baScan
	}
	return MaxBestAvailableScan
}

// bestAvailableRun runs the selection query and returns the chosen run in projected order,
// or ErrBestAvailableUnavailable when no legal window exists within the scan cap.
//
// An empty result and a short result are both refusals rather than partial successes: the
// query emits a window only when it has exactly `count` seats, so a different length means
// the query and this function disagree about what was asked, and selling a partial run
// would be worse than refusing.
func bestAvailableRun(ctx context.Context, tx *sql.Tx, pool uuid.UUID, orphanPrevention bool, count, scan int32) ([]string, error) {
	q := bestAvailableRunQuery
	if scan != MaxBestAvailableScan {
		// Only a test narrows the cap (see bestAvailableScan). Rebuilding the statement
		// rather than binding the limit keeps the SHIPPED query a single const the plan
		// probe can EXPLAIN verbatim, which is the whole reason it is a const.
		q = strings.Replace(q, maxBestAvailableScanSQL, "LIMIT "+strconv.Itoa(int(scan)), 1)
	}
	rows, err := tx.QueryContext(ctx, q, pool, orphanPrevention, count)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0, count)
	for rows.Next() {
		var seat string
		if err = rows.Scan(&seat); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrBestAvailableUnavailable
	}
	if int32(len(out)) != count {
		return nil, fmt.Errorf("selection returned %d seats for a party of %d", len(out), count)
	}
	return out, nil
}
