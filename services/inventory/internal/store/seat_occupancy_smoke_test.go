//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-172: the buyer-facing seat-occupancy read. Every assertion here is
// relational — partial-index liveness, lazy expiry, the refunded-but-confirmed
// state, cross-pool scoping, and PostgreSQL's actual plan — so this is a real
// Postgres suite. A mock-backed unit test could not fail for any of these reasons.

// occupancy is the assertion helper: it runs the read and returns the seat list.
func occupancy(t *testing.T, st *Postgres, org, slot uuid.UUID) SeatOccupancy {
	t.Helper()
	got, err := st.SeatOccupancy(t.Context(), org, slot)
	if err != nil {
		t.Fatalf("seat occupancy: %v", err)
	}
	return got
}

func seatsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSeatOccupancyCountsOnlyClaimableSeats is AC2. The read must agree with what
// the claim path would actually refuse, which is NOT the same as either conjunct
// of its predicate on its own:
//
//   - a `finalizing` claim still owns its seats, so raw status='held' loses it;
//   - a confirmed (sold) seat is occupied, so `liveClaims` alone loses it;
//   - a due-but-unswept held claim is free in fact (the next claim transaction
//     sweeps it before its unique-index insert), so `released_at IS NULL` alone
//     reports a free seat as taken.
func TestSeatOccupancyCountsOnlyClaimableSeats(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := provisionedSeated(t, ctx, st, 100)
	tt := uuid.New()

	held, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/1"}, 0, "EUR", "k-held")
	if err != nil {
		t.Fatalf("held hold: %v", err)
	}
	finalizing, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/2"}, 0, "EUR", "k-fin")
	if err != nil {
		t.Fatalf("finalizing hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, finalizing.Claim.ID, "finalizing"); err != nil {
		t.Fatalf("to finalizing: %v", err)
	}
	confirmed, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/3"}, 0, "EUR", "k-conf")
	if err != nil {
		t.Fatalf("confirmed hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, confirmed.Claim.ID, "finalizing"); err != nil {
		t.Fatalf("to finalizing: %v", err)
	}
	if _, err = st.Transition(ctx, org, confirmed.Claim.ID, "confirmed"); err != nil {
		t.Fatalf("to confirmed: %v", err)
	}
	released, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/4"}, 0, "EUR", "k-rel")
	if err != nil {
		t.Fatalf("released hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, released.Claim.ID, "released"); err != nil {
		t.Fatalf("to released: %v", err)
	}

	// A held claim past its TTL that nothing has swept yet: status is still 'held'
	// and its seat rows are still live, but the seat IS claimable — the next seat
	// hold sweeps it inside the same transaction that inserts (ADR-031).
	stale, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/5"}, 0, "EUR", "k-stale")
	if err != nil {
		t.Fatalf("stale hold: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE claims SET expires_at = now() - interval '1 minute' WHERE id=$1`, stale.Claim.ID); err != nil {
		t.Fatal(err)
	}

	got := occupancy(t, st, org, slot)

	if got.SlotID != slot || got.SeatMapID != seatMap {
		t.Fatalf("ids = %v/%v want %v/%v", got.SlotID, got.SeatMapID, slot, seatMap)
	}
	if got.OfferingStatus != "open" {
		t.Fatalf("offering_status = %q want open", got.OfferingStatus)
	}
	want := []string{"A/1/1", "A/1/2", "A/1/3"}
	if !seatsEqual(got.Unavailable, want) {
		t.Fatalf("unavailable = %v want %v (held + finalizing + confirmed; not released, not unswept-expired)",
			got.Unavailable, want)
	}
	_ = held

	// The read is a cacheable public GET: it must not have swept anything.
	var status string
	var releasedAt sql.NullTime
	if err = db.QueryRowContext(ctx,
		`SELECT c.status, cs.released_at FROM claims c JOIN claim_seats cs ON cs.claim_id=c.id WHERE c.id=$1`,
		stale.Claim.ID).Scan(&status, &releasedAt); err != nil {
		t.Fatal(err)
	}
	if status != "held" || releasedAt.Valid {
		t.Fatalf("the read mutated the stale claim: status=%q released_at=%v — a cacheable GET must not write",
			status, releasedAt)
	}
}

// TestSeatOccupancyFreesFullyRefundedSeats pins the OTHER reason the liveness
// predicate is a conjunction, and the one a reader is most likely to delete.
//
// A full refund of a seated claim sets claim_seats.released_at but leaves
// claims.status = 'confirmed' — refund_returns.go:113-137 says so explicitly, and
// releaseSeatsForTerminal cannot help because it only touches claims already in
// ('expired','released'). So `status='confirmed'` alone reports a refunded seat as
// occupied forever: an unsellable seat with no way for anyone to notice.
//
// If this test fails, do not relax it — one of the two conjuncts of the liveness
// predicate has been dropped.
func TestSeatOccupancyFreesFullyRefundedSeats(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sold, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2"}, 0, "EUR", "k-sold")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, sold.Claim.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sold.Claim.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if occ := occupancy(t, st, org, slot); len(occ.Unavailable) != 2 {
		t.Fatalf("sold seats must be occupied, got %v", occ.Unavailable)
	}

	// A FULL return — the only kind a seated claim accepts (ErrPartialSeatedReturn).
	if _, err = st.ReturnRefundedCapacity(ctx, org, sold.Claim.ID, uuid.New(), 2); err != nil {
		t.Fatalf("refund return: %v", err)
	}

	occ := occupancy(t, st, org, slot)
	if len(occ.Unavailable) != 0 {
		t.Fatalf("refunded seats must be claimable again, got %v — the claim is still 'confirmed', so "+
			"a status-only liveness predicate strands them permanently", occ.Unavailable)
	}
}

// TestSeatOccupancyIsPoolScoped is the first half of ADR-019's two-test rule: the
// RESULT is scoped. An identical seat identity on another pool must not leak.
func TestSeatOccupancyIsPoolScoped(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	orgA, slotA, _ := provisionedSeated(t, ctx, st, 100)
	orgB, slotB, _ := provisionedSeated(t, ctx, st, 100)

	if _, err := st.CreateSeatHold(ctx, orgA, slotA, uuid.New(), []string{"A/1/1"}, 0, "EUR", "ka"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSeatHold(ctx, orgB, slotB, uuid.New(), []string{"A/1/1", "A/1/2"}, 0, "EUR", "kb"); err != nil {
		t.Fatal(err)
	}

	if occ := occupancy(t, st, orgA, slotA); !seatsEqual(occ.Unavailable, []string{"A/1/1"}) {
		t.Fatalf("pool A = %v want [A/1/1] — pool B's identical seat identity leaked", occ.Unavailable)
	}
	if occ := occupancy(t, st, orgB, slotB); !seatsEqual(occ.Unavailable, []string{"A/1/1", "A/1/2"}) {
		t.Fatalf("pool B = %v want [A/1/1 A/1/2]", occ.Unavailable)
	}
}

// TestSeatOccupancyEmptySeatedPool: an empty seated pool is a 200 with an empty
// list, not a 404 — distinguishable from an unknown slot, and never nil.
func TestSeatOccupancyEmptySeatedPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := provisionedSeated(t, ctx, st, 100)

	occ := occupancy(t, st, org, slot)
	if occ.SeatMapID != seatMap {
		t.Fatalf("seat map = %v want %v", occ.SeatMapID, seatMap)
	}
	if occ.Unavailable == nil {
		t.Fatal("unavailable must be an empty slice, never nil — it serializes as [] not null")
	}
	if len(occ.Unavailable) != 0 {
		t.Fatalf("unavailable = %v want empty", occ.Unavailable)
	}
}

// TestSeatOccupancyDistinguishesUnknownAndGA is AC4: the two refusals must not
// collapse. Both map through the already-shipped problem() branches.
func TestSeatOccupancyDistinguishesUnknownAndGA(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	gaOrg, gaSlot := provisioned(t, ctx, st, 100)

	if _, err := st.SeatOccupancy(ctx, org, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slot: err = %v want ErrNotFound", err)
	}
	if _, err := st.SeatOccupancy(ctx, uuid.New(), slot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong organizer: err = %v want ErrNotFound", err)
	}
	if _, err := st.SeatOccupancy(ctx, gaOrg, gaSlot); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("GA slot: err = %v want ErrPoolKindMismatch", err)
	}
}

// TestSeatOccupancyReportsOfferingStatus: the counters stay factual on a dead slot,
// but the caller must be able to tell "these seats are free" from "nothing here is
// claimable" (TKT-75's rule, which Availability already carries).
func TestSeatOccupancyReportsOfferingStatus(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, lifecycle, closure, want string
	}{
		{"open", "published", "open", "open"},
		{"closed", "published", "closed", "closed"},
		{"archived wins over closed", "archived", "closed", "archived"},
	} {
		if _, err := db.ExecContext(ctx,
			`UPDATE inventory_pools SET lifecycle_status=$2, closure_status=$3 WHERE slot_id=$1`,
			slot, tc.lifecycle, tc.closure); err != nil {
			t.Fatal(err)
		}
		occ := occupancy(t, st, org, slot)
		if occ.OfferingStatus != tc.want {
			t.Fatalf("%s: offering_status = %q want %q", tc.name, occ.OfferingStatus, tc.want)
		}
		// Counters stay factual whatever the offering state.
		if !seatsEqual(occ.Unavailable, []string{"A/1/1"}) {
			t.Fatalf("%s: unavailable = %v want [A/1/1] — the seat list is factual, "+
				"offering_status is the sale signal", tc.name, occ.Unavailable)
		}
	}
}

// planProbeSeq names each PREPARE uniquely: a prepared statement outlives the
// rolled-back transaction that created it, so a fixed name dies with SQLSTATE
// 42P05 on the second call over the same pooled connection.
var planProbeSeq atomic.Uint64

// explainGenericPlan mirrors catalog's helper (services/catalog/internal/store/
// season_smoke_test.go) — it is unexported there and in another module's package,
// so this is a deliberate copy rather than an import.
//
// The PREPARE/EXECUTE round trip is the entire point. `EXPLAIN <query with $1>`
// sent through the driver plans the inner query with the value already bound and
// yields a CUSTOM plan whatever plan_cache_mode says: it looks like a passing
// assertion and proves nothing, because a value-bound plan uses the index whether
// the predicate is scoped or not. A real generic plan is recognisable by the
// parameter surviving as `$1` in the output, which is what the guard below checks.
// PREPARE, SET and EXECUTE share one transaction so the setting and the statement
// cannot land on different pooled connections.
func explainGenericPlan(t *testing.T, db *sql.DB, query string, scopes ...uuid.UUID) string {
	t.Helper()
	if len(scopes) == 0 {
		t.Fatal("explainGenericPlan: a query with no parameters has no generic plan to assert")
	}
	types := make([]string, 0, len(scopes))
	literals := make([]string, 0, len(scopes))
	for _, s := range scopes {
		types = append(types, "uuid")
		literals = append(literals, "'"+s.String()+"'::uuid")
	}
	stmt := "plan_probe_" + strconv.FormatUint(planProbeSeq.Add(1), 10)

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(t.Context(),
		"PREPARE "+stmt+"("+strings.Join(types, ", ")+") AS "+query); err != nil {
		t.Fatal(err)
	}
	// Set before the first EXECUTE: the cached plan is built then.
	if _, err = tx.ExecContext(t.Context(), `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(t.Context(),
		"EXPLAIN EXECUTE "+stmt+"("+strings.Join(literals, ", ")+")")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	got := plan.String()
	for i := range scopes {
		if marker := "$" + strconv.Itoa(i+1); !strings.Contains(got, marker) {
			t.Fatalf("not a generic plan — %s was substituted, so plan_cache_mode did not apply "+
				"and this assertion proves nothing.\nplan:\n%s", marker, got)
		}
	}
	return got
}

// TestSeatOccupancyIsIndexScoped is the second half of ADR-019's two-test rule:
// the SCAN is scoped. A correct result set can be produced by reading every seat
// row in the database and discarding the ones that belong to other pools — which
// is the defect, and it returns the right answer while doing it.
//
// It EXPLAINs seatOccupancySeatsQuery itself, the statement the shipped read
// executes, so a retyped copy cannot drift away from production. The claim is
// narrow: under the fixture's statistics and a blind plan, the read reaches
// claim_seats through its partial live index and does not sequentially scan it.
func TestSeatOccupancyIsIndexScoped(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1"); err != nil {
		t.Fatal(err)
	}

	// A plan assertion means nothing until a sequential scan is the WRONG choice:
	// on a handful of rows Postgres rightly ignores every index and the test fails
	// for a reason unrelated to scoping.
	//
	// Seeding one huge OTHER pool is not enough, and getting this wrong is the way
	// this test passes or fails for the wrong reason. A generic plan has no bound
	// value, so the planner estimates `pool_id = $1` at 1/n_distinct(pool_id) — with
	// two pools that is half the table, and a sequential scan is genuinely the right
	// choice. What makes the index the right choice under a BLIND plan is many
	// distinct pools, which is also what production looks like. So: 200 pools of 25
	// live seats each, seeded in SQL because 5000 rows through CreateSeatHold would
	// dominate the suite's runtime.
	if _, err := db.ExecContext(ctx, `
		WITH pools AS (
			INSERT INTO inventory_pools(slot_id, organizer_id, capacity, source_event_id,
			                            inventory_kind, seat_map_id)
			SELECT gen_random_uuid(), gen_random_uuid(), 1000, gen_random_uuid(),
			       'seated', gen_random_uuid()
			FROM generate_series(1, 200)
			RETURNING slot_id, organizer_id
		), bulk_claims AS (
			INSERT INTO claims(id, organizer_id, pool_id, quantity, status, expires_at,
			                   idempotency_key, request_fingerprint)
			SELECT gen_random_uuid(), p.organizer_id, p.slot_id, 25, 'held',
			       now() + interval '1 hour', 'bulk-' || p.slot_id, 'bulk'
			FROM pools p
			RETURNING id, pool_id
		)
		INSERT INTO claim_seats(claim_id, pool_id, seat_identity)
		SELECT bc.id, bc.pool_id, 'Y/1/' || g
		FROM bulk_claims bc, generate_series(1, 25) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE claim_seats, claims, inventory_pools`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(t, db, seatOccupancySeatsQuery, slot)

	if !strings.Contains(plan, "claim_seats_one_live_per_seat") {
		t.Fatalf("plan does not reach claim_seats through claim_seats_one_live_per_seat — it scans.\nplan:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on claim_seats") {
		t.Fatalf("plan sequentially scans claim_seats even though the index appears in it.\nplan:\n%s", plan)
	}
	// The two assertions above are jointly satisfiable by a plan that scans the WHOLE
	// partial index and filters afterwards: the index name appears, no sequential table
	// scan appears, and every live seat in the database is still read — which is exactly
	// the regression this test exists to prevent (ai-review finding). What rules that
	// out is the access condition: the scoping parameter must bind INSIDE the index
	// lookup, not in a filter applied to its output.
	if !strings.Contains(plan, "Index Cond: (pool_id = $1)") {
		t.Fatalf("the plan reads claim_seats through the index but does not BIND pool_id in the "+
			"index condition, so it scans every live seat and filters after — an unscoped read "+
			"wearing an index's name.\nplan:\n%s", plan)
	}
}

// TestSeatOccupancyReportsRemainingHeadroom is the ai-review finding: the seat list
// alone can say "free" about a seat that no claim will ever grant.
//
// A seated pool carries a coarse aggregate ceiling as well as its per-seat rows, and
// CreateSeatHold refuses with ErrUnavailable once confirmed + live held + requested
// exceeds it. A draining capacity cut (target_capacity, TKT-76) takes that headroom
// to zero without touching a single claim_seats row — so every unheld seat identity
// stays absent from Unavailable while every claim on it fails. offering_status does
// not move either: the slot is still open, it is just full.
//
// Available is the signal that closes it, and it is quoted from Availability rather
// than recomputed here so the two cannot drift.
func TestSeatOccupancyReportsRemainingHeadroom(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1"); err != nil {
		t.Fatal(err)
	}

	occ := occupancy(t, st, org, slot)
	if occ.RemainingCapacity != 99 {
		t.Fatalf("remaining_capacity = %d want 99 (100 capacity - 1 held)", occ.RemainingCapacity)
	}

	// Cut the pool to exactly what is already held. Nothing about the seats changes.
	if _, err := db.ExecContext(ctx,
		`UPDATE inventory_pools SET target_capacity=1 WHERE slot_id=$1`, slot); err != nil {
		t.Fatal(err)
	}

	occ = occupancy(t, st, org, slot)
	if !seatsEqual(occ.Unavailable, []string{"A/1/1"}) {
		t.Fatalf("the seat list is unchanged by a capacity cut, got %v", occ.Unavailable)
	}
	if occ.OfferingStatus != "open" {
		t.Fatalf("a capacity cut does not close the slot: offering_status = %q", occ.OfferingStatus)
	}
	if occ.RemainingCapacity != 0 {
		t.Fatalf("remaining_capacity = %d want 0 — without this the response says every other seat "+
			"is free while CreateSeatHold rejects them all with ErrUnavailable", occ.RemainingCapacity)
	}

	// The claim path agrees: this is the state the response now describes honestly.
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/2"}, 0, "EUR", "k2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("claim on an unheld seat with no headroom = %v, want ErrUnavailable", err)
	}
}

// TestSeatOccupancyCapacityMirrorsTheSeatedAdmissionRule pins WHICH rule
// remaining_capacity quotes, because there are two plausible ones and they disagree.
//
// The public availability read subtracts unsold channel reservations from its
// `available`. The seated claim path does NOT: CreateSeatHold admits against
// target_capacity/capacity alone and never consults channel_allocations. Sourcing
// this field from Availability therefore reported 0 while the very seat claim a
// picker would issue succeeded — suppressing genuinely claimable inventory, and
// making the comment that claimed the two agree false (ai-review pass 2).
//
// If this test fails because someone routed the field back through Availability,
// the fix is not to relax it.
func TestSeatOccupancyCapacityMirrorsTheSeatedAdmissionRule(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 10)

	// Reserve the pool's whole capacity for a non-public channel. The public
	// availability read would now answer 0.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_allocations(pool_id, channel_code, cap)
		VALUES ($1, 'reseller', 10)`, slot); err != nil {
		t.Fatal(err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}

	occ := occupancy(t, st, org, slot)
	if occ.RemainingCapacity != 10 {
		t.Fatalf("remaining_capacity = %d want 10: it quotes the SEATED admission rule "+
			"(target_capacity/capacity), not availability's channel-adjusted %d",
			occ.RemainingCapacity, a.Available)
	}
	// And the claim path agrees — which is the whole point of quoting its rule.
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1"); err != nil {
		t.Fatalf("seat claim under a full channel reservation = %v; if this now fails, the "+
			"seated channel semantics changed and this field must follow them", err)
	}
}

// TestSeatOccupancyCapacityIsACeilingNotASeatCount pins the honest reading of the
// field, so nobody mistakes it for "seats you can still pick" (ai-review pass 2).
//
// Inventory does not hold the seat universe — that is the seat map, in catalog —
// and a seated pool is provisioned from the venue's GA snapshot, which can exceed
// the map's seat count. So with every mapped seat occupied the pool still reports
// headroom. That is not a bug to clamp away here (inventory cannot see the map
// without a cross-service call the claim path deliberately avoids); it is why a
// picker must gate on the seat list AS WELL, and why the field is not called
// "available".
func TestSeatOccupancyCapacityIsACeilingNotASeatCount(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	// The "map" here is two seats; the pool's ceiling is a hundred.
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2"}, 0, "EUR", "k1"); err != nil {
		t.Fatal(err)
	}

	occ := occupancy(t, st, org, slot)
	if !seatsEqual(occ.Unavailable, []string{"A/1/1", "A/1/2"}) {
		t.Fatalf("unavailable = %v want both mapped seats", occ.Unavailable)
	}
	if occ.RemainingCapacity != 98 {
		t.Fatalf("remaining_capacity = %d want 98 — the field is the POOL's ceiling, not a "+
			"count of free seats, and this test exists to keep that distinction visible",
			occ.RemainingCapacity)
	}
}

// TestSeatOccupancyCapacityAcrossTheClaimLifecycle is the mutation-sensitive
// coverage the third review pass asked for: it walks one seated claim through
// held -> finalizing -> confirmed -> fully refunded and pins the headroom at each
// step. The aggregate is assembled from TWO sources that must not overlap — the
// pool's confirmed_quantity column, and a live-claims sum over `liveClaims`, which
// deliberately EXCLUDES confirmed. Nothing else in the suite would catch either
// half being wrong:
//
//   - if liveClaims were widened to consumingClaims, the confirmed step would
//     count the claim twice and headroom would read 8 instead of 9;
//   - if finalizing were dropped from the predicate, that step would read 10 and
//     the pool would appear to have room it has already promised;
//   - if the refund did not return confirmed capacity, the last step would stay 9.
func TestSeatOccupancyCapacityAcrossTheClaimLifecycle(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 10)

	if got := occupancy(t, st, org, slot).RemainingCapacity; got != 10 {
		t.Fatalf("empty pool: remaining_capacity = %d want 10", got)
	}

	hold, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		to   string
		want int32
	}{
		{"", 9},           // held
		{"finalizing", 9}, // still owns the seat and the capacity
		{"confirmed", 9},  // now counted by confirmed_quantity, NOT by liveClaims
	} {
		if step.to != "" {
			if _, err = st.Transition(ctx, org, hold.Claim.ID, step.to); err != nil {
				t.Fatalf("to %s: %v", step.to, err)
			}
		}
		occ := occupancy(t, st, org, slot)
		if occ.RemainingCapacity != step.want {
			t.Fatalf("after %q: remaining_capacity = %d want %d — the claim is counted %s",
				step.to, occ.RemainingCapacity, step.want,
				map[bool]string{true: "twice or not at all", false: "wrongly"}[occ.RemainingCapacity != step.want])
		}
		if !seatsEqual(occ.Unavailable, []string{"A/1/1"}) {
			t.Fatalf("after %q: unavailable = %v want [A/1/1]", step.to, occ.Unavailable)
		}
	}

	if _, err = st.ReturnRefundedCapacity(ctx, org, hold.Claim.ID, uuid.New(), 1); err != nil {
		t.Fatalf("refund return: %v", err)
	}
	occ := occupancy(t, st, org, slot)
	if occ.RemainingCapacity != 10 {
		t.Fatalf("after a full refund: remaining_capacity = %d want 10 — the capacity came back "+
			"but the aggregate did not notice", occ.RemainingCapacity)
	}
	if len(occ.Unavailable) != 0 {
		t.Fatalf("after a full refund: unavailable = %v want empty", occ.Unavailable)
	}
}

// TestSeatOccupancyCapacityIsSlotScoped closes the other mutation the third pass
// named: the live-claims aggregate is a correlated subquery, and if its predicate
// were scoped by organizer instead of by slot — or if the two $1 bindings drifted
// apart — a second seated pool under the SAME organizer would eat this pool's
// headroom. Every other test in this file uses one pool per organizer and would
// stay green through that bug.
func TestSeatOccupancyCapacityIsSlotScoped(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slotA, _ := provisionedSeated(t, ctx, st, 10)

	// A second seated pool for the SAME organizer, holding most of its own capacity.
	slotB, seatMapB := uuid.New(), uuid.New()
	if err := st.ProvisionSeated(ctx, uuid.New(), slotB, org, seatMapB, 10, false, nil); err != nil {
		t.Fatal(err)
	}
	seats := []string{"B/1/1", "B/1/2", "B/1/3", "B/1/4", "B/1/5", "B/1/6", "B/1/7"}
	if _, err := st.CreateSeatHold(ctx, org, slotB, uuid.New(), seats, 0, "EUR", "kb"); err != nil {
		t.Fatal(err)
	}
	var poolsForOrg int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM inventory_pools WHERE organizer_id=$1`, org).Scan(&poolsForOrg); err != nil {
		t.Fatal(err)
	}
	if poolsForOrg != 2 {
		t.Fatalf("fixture must give the organizer two seated pools, got %d", poolsForOrg)
	}

	if occ := occupancy(t, st, org, slotA); occ.RemainingCapacity != 10 || len(occ.Unavailable) != 0 {
		t.Fatalf("slot A = capacity %d / seats %v; the other pool's 7 held seats leaked into "+
			"this pool's aggregate", occ.RemainingCapacity, occ.Unavailable)
	}
	if occ := occupancy(t, st, org, slotB); occ.RemainingCapacity != 3 {
		t.Fatalf("slot B: remaining_capacity = %d want 3", occ.RemainingCapacity)
	}
}

// TestProvisionSeatedCommitsAdjacencyAtomically is TKT-181's central property: a
// rule-enabled pool and its adjacency projection are committed together, or not at all.
//
// Not a style preference. ProvisionSeated writes consumed_events, so a pool that
// commits WITHOUT its projection can never be repaired — a later binary redelivered
// the same event short-circuits on the consumed row (ADR-041). "Pool exists, rule says
// on, projection missing" has to be unrepresentable rather than merely unlikely.
func TestProvisionSeatedCommitsAdjacencyAtomically(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	left, mid := "A/1/1", "A/1/2"
	adjacency := []SeatAdjacencyRow{
		{SeatIdentity: left, Right: &mid},
		{SeatIdentity: mid, Left: &left},
	}

	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, true, adjacency); err != nil {
		t.Fatal(err)
	}

	var enabled bool
	if err := db.QueryRowContext(ctx,
		`SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("the pool must record that the rule is on")
	}
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1`, slot).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("adjacency rows = %d want 2 — the projection commits with the pool", rows)
	}
	// A row end is NULL, and that is an answer rather than missing data: an end seat
	// has one neighbour, so the rule must be able to tell "no neighbour" from
	// "unknown".
	var leftOfFirst sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT left_identity FROM seat_claim_adjacency WHERE pool_id=$1 AND seat_identity=$2`,
		slot, left).Scan(&leftOfFirst); err != nil {
		t.Fatal(err)
	}
	if leftOfFirst.Valid {
		t.Fatalf("the first seat in a row has no left neighbour, got %q", leftOfFirst.String)
	}
}

// TestProvisionSeatedRefusesEnabledPoolWithoutProjection: fail closed rather than
// commit a pool whose rule cannot be enforced. Because the write is idempotent on the
// event id, committing here would be FINAL.
func TestProvisionSeatedRefusesEnabledPoolWithoutProjection(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	slot := uuid.New()

	err := st.ProvisionSeated(ctx, uuid.New(), slot, uuid.New(), uuid.New(), 100, true, nil)
	if err == nil {
		t.Fatal("a rule-enabled pool with no adjacency must be refused, not committed")
	}
	var pools int
	if qerr := db.QueryRowContext(ctx,
		`SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&pools); qerr != nil {
		t.Fatal(qerr)
	}
	if pools != 0 {
		t.Fatal("nothing may be committed when the projection is missing")
	}
}

// TestProvisionSeatedRuleOffWritesNoProjection is AC4's absence, proven by construction:
// a rule-off pool provisions exactly as it did before this ticket and touches the new
// table not at all.
func TestProvisionSeatedRuleOffWritesNoProjection(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	slot := uuid.New()

	if err := st.ProvisionSeated(ctx, uuid.New(), slot, uuid.New(), uuid.New(), 100, false, nil); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1`, slot).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if enabled || rows != 0 {
		t.Fatalf("rule-off pool: enabled=%v adjacency rows=%d, want false/0", enabled, rows)
	}
}

// TestProvisionSeatedUpgradesAnExistingPool is ADR-041's correction wave, which is the
// only reason performances published before the transport existed are not permanently
// rule-less.
//
// Those pools were provisioned at schema 4 and are rule-off. The wave re-emits them
// under a FRESH event id, so consumed_events accepts it — and an `ON CONFLICT(slot_id)
// DO NOTHING` insert would then leave the pool rule-off, insert the adjacency beside
// it, and mark the event consumed. The organizer's rule silently disabled for ever,
// by the very mechanism meant to repair it (ai-review).
func TestProvisionSeatedUpgradesAnExistingPool(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()

	// As it was before the transport existed: seated, rule off, no projection.
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, false, nil); err != nil {
		t.Fatal(err)
	}

	left, mid := "A/1/1", "A/1/2"
	adjacency := []SeatAdjacencyRow{{SeatIdentity: left, Right: &mid}, {SeatIdentity: mid, Left: &left}}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, true, adjacency); err != nil {
		t.Fatalf("correction wave: %v", err)
	}

	var enabled bool
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1`, slot).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if !enabled || rows != 2 {
		t.Fatalf("upgrade left enabled=%v adjacency=%d, want true/2 — the wave must turn the rule ON, "+
			"not insert a projection beside a pool that still says it is off", enabled, rows)
	}
}

// A stale schema-4 replay must never turn the rule back OFF: the flag is monotonic.
func TestProvisionSeatedRuleFlagIsMonotonic(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := "A/1/1"
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, true,
		[]SeatAdjacencyRow{{SeatIdentity: seat}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, false, nil); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := db.QueryRowContext(ctx,
		`SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("a stale schema-4 replay must not disable a rule the correction wave turned on")
	}
}

// Adopting a pool that names a different organizer or a different seat map would
// attach one map's adjacency to another map's seats. Refuse instead.
func TestProvisionSeatedRefusesToAdoptAMismatchedPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100, false, nil); err != nil {
		t.Fatal(err)
	}
	seat := "A/1/1"
	adj := []SeatAdjacencyRow{{SeatIdentity: seat}}

	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, uuid.New(), 100, true, adj); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("different seat map: err = %v want ErrPoolKindMismatch", err)
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, uuid.New(), seatMap, 100, true, adj); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("different organizer: err = %v want ErrPoolKindMismatch", err)
	}
}

// seededOrphanPool provisions a rule-enabled pool with one row of `n` seats named
// A/1/1..A/1/n, and returns (org, slot, seat namer).
func seededOrphanPool(t *testing.T, ctx context.Context, st *Postgres, n int) (uuid.UUID, uuid.UUID, func(int) string) {
	t.Helper()
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := func(i int) string { return "A/1/" + strconv.Itoa(i) }
	adjacency := make([]SeatAdjacencyRow, 0, n)
	for i := 1; i <= n; i++ {
		row := SeatAdjacencyRow{SeatIdentity: seat(i)}
		if i > 1 {
			left := seat(i - 1)
			row.Left = &left
		}
		if i < n {
			right := seat(i + 1)
			row.Right = &right
		}
		adjacency = append(adjacency, row)
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, adjacency); err != nil {
		t.Fatal(err)
	}
	return org, slot, seat
}

// TestSeatHoldRejectsNewlyOrphanedSeats is TKT-182: a selection that would strand a
// lone free seat is refused inside the deciding transaction.
func TestSeatHoldRejectsNewlyOrphanedSeats(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)

	t.Run("stranding the middle of three", func(t *testing.T) {
		org, slot, seat := seededOrphanPool(t, ctx, st, 3)
		// Taking 1 and 3 leaves 2 with no free neighbour.
		_, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(1), seat(3)}, 0, "EUR", "k1")
		var orphan *SeatOrphanedError
		if !errors.As(err, &orphan) {
			t.Fatalf("err = %v (%T), want *SeatOrphanedError", err, err)
		}
		if len(orphan.Seats) != 1 || orphan.Seats[0] != seat(2) {
			t.Fatalf("stranded = %v want [%s]", orphan.Seats, seat(2))
		}
	})

	t.Run("a run of two is fine", func(t *testing.T) {
		org, slot, seat := seededOrphanPool(t, ctx, st, 4)
		// Taking 1 leaves 2,3,4 — every one still has a free neighbour.
		if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(1)}, 0, "EUR", "k2"); err != nil {
			t.Fatalf("a selection that strands nothing must succeed: %v", err)
		}
	})

	t.Run("row ends have one neighbour", func(t *testing.T) {
		org, slot, seat := seededOrphanPool(t, ctx, st, 3)
		// Taking 1 and 2 leaves 3, whose only neighbour (2) is now gone.
		_, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(1), seat(2)}, 0, "EUR", "k3")
		var orphan *SeatOrphanedError
		if !errors.As(err, &orphan) || len(orphan.Seats) != 1 || orphan.Seats[0] != seat(3) {
			t.Fatalf("err = %v — an END seat has ONE neighbour, so losing it strands the end", err)
		}
	})

	t.Run("a one-seat row is always selectable", func(t *testing.T) {
		org, slot, seat := seededOrphanPool(t, ctx, st, 1)
		if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(1)}, 0, "EUR", "k4"); err != nil {
			t.Fatalf("a seat with no neighbours strands nobody: %v", err)
		}
	})

	t.Run("only NEWLY orphaned seats are reported", func(t *testing.T) {
		// Ten seats, so a claim can be genuinely unrelated to the stranded one. In a
		// six-seat row nothing is far enough away: every remaining choice strands
		// something of its own, which is a fact about small rows, not about the rule.
		org, slot, seat := seededOrphanPool(t, ctx, st, 10)
		// Strand seat 2 the way reality does — a refund, an admin action, a claim made
		// before the rule was enabled — by writing the rows directly rather than going
		// through the rule that would now refuse them. The earlier version of this test
		// tried to seed through CreateSeatHold and SKIPPED when that was refused, so it
		// asserted nothing at all on the path it existed to protect (ai-review).
		db := dbOf(t, st)
		for _, s := range []string{seat(1), seat(3)} {
			claimID := uuid.New()
			if _, err := db.ExecContext(ctx,
				`INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint)
				 VALUES($1,$2,$3,1,'confirmed',now()+interval '1 hour',$4,'seed')`,
				claimID, org, slot, "seed-"+s); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO claim_seats(claim_id,pool_id,seat_identity) VALUES($1,$2,$3)`,
				claimID, slot, s); err != nil {
				t.Fatal(err)
			}
		}
		// Seat 2 is isolated and nothing this claim does causes it: 7 and 8 leave 6
		// beside 5 and 9 beside 10.
		if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(7), seat(8)}, 0, "EUR", "k5"); err != nil {
			t.Fatalf("a pre-existing orphan must not poison an unrelated claim: %v", err)
		}
		// And the rule still fires where THIS claim does the stranding: taking 5 leaves
		// 4 between an already-taken 3 and a now-taken 5.
		_, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat(5)}, 0, "EUR", "k6")
		var orphan *SeatOrphanedError
		if !errors.As(err, &orphan) {
			t.Fatalf("err = %v — the rule must still fire for seats THIS claim strands", err)
		}
		// It may legitimately name several seats — taking 5 isolates 4 (3 is gone) and
		// 6 (7 is gone). What it must NEVER name is seat 2: that one was stranded long
		// before this claim existed, and blaming this buyer for it is the bug.
		for _, s := range orphan.Seats {
			if s == seat(2) {
				t.Fatalf("stranded = %v — seat 2 was already isolated; a pre-existing orphan "+
					"must never be re-reported against a later claim", orphan.Seats)
			}
		}
		if len(orphan.Seats) == 0 {
			t.Fatal("taking seat 5 does strand seats; the rule must say so")
		}
	})
}

// TestSeatHoldRuleOffIgnoresOrphans is AC4 proven by construction: with the rule off,
// the identical selection succeeds and the projection is never consulted.
func TestSeatHoldRuleOffIgnoresOrphans(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 1000)

	// Drop the table entirely: a rule-off claim must not read it at all.
	if _, err := db.ExecContext(ctx, `DROP TABLE seat_claim_adjacency`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/3"}, 0, "EUR", "k1"); err != nil {
		t.Fatalf("rule-off claim must not touch the projection: %v", err)
	}
}

// TestOrphanRuleHoldsUnderContention is AC7, and it is the reason the check lives
// inside the deciding transaction rather than anywhere more convenient.
//
// Row of five. A takes seat 1; B takes seat 3. Each selection is legal ALONE — 1
// leaves 2,3,4,5 and 3 leaves 1,2,4,5, both fully paired. Together they isolate seat 2.
// A check that ran before the transaction — in the browser, in commerce, in a
// pre-flight read — passes both, and the row ends up with a hole nobody can sell.
//
// Under the pool lock exactly one commits and the loser is told which seat it would
// have stranded.
func TestOrphanRuleHoldsUnderContention(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededOrphanPool(t, ctx, st, 5)

	// The barrier is a real transaction holding the pool row, not a goroutine start
	// signal. Closing a channel only makes both goroutines RUNNABLE; normal scheduling
	// could let one finish before the other begins, and the test would pass even with
	// the lock removed (ai-review). Holding the row makes the overlap certain: B cannot
	// reach its orphan check until A commits.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(ctx,
		`SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	picks := [][]string{{seat(1)}, {seat(3)}}
	started := make(chan struct{}, 2)
	for i := range picks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			_, results[i] = st.CreateSeatHold(ctx, org, slot, uuid.New(), picks[i], 0, "EUR",
				"race-"+strconv.Itoa(i))
		}(i)
	}
	// Both are inside CreateSeatHold and blocked on the pool row before either can
	// decide anything.
	<-started
	<-started
	time.Sleep(150 * time.Millisecond)
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	var ok, orphaned int
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrSeatOrphaned):
			orphaned++
			var o *SeatOrphanedError
			if errors.As(err, &o) && (len(o.Seats) != 1 || o.Seats[0] != seat(2)) {
				t.Fatalf("claim %d: stranded = %v want [%s]", i, o.Seats, seat(2))
			}
		default:
			t.Fatalf("claim %d: unexpected %v", i, err)
		}
	}
	if ok != 1 || orphaned != 1 {
		t.Fatalf("succeeded=%d orphan-refused=%d, want 1/1 — two individually legal claims "+
			"must not be able to jointly strand a seat", ok, orphaned)
	}

	// And the loser wrote nothing.
	var live int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claim_seats WHERE pool_id=$1 AND released_at IS NULL`, slot).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live seat rows = %d want 1 — the refused claim must roll back entirely", live)
	}
}
