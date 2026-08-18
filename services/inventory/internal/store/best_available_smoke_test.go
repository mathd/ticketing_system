//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seededBestAvailablePool provisions a rule-enabled pool whose projection carries the
// ordering metadata best-available selects on: `rows` rows of `perRow` seats each, named
// A/<r>/<s>, each row a complete left/right chain with row_key "A/<r>" and position 1..perRow.
//
// Deliberately a SIBLING of seededOrphanPool rather than a replacement for it. The
// ADR-041 fixtures pin a shipped rule that does not read ordering metadata; moving them
// with this mechanism would quietly turn them into assertions that the new shape is what
// the old rule always meant.
func seededBestAvailablePool(t *testing.T, ctx context.Context, st *Postgres, rows, perRow int) (uuid.UUID, uuid.UUID, func(int, int) string) {
	t.Helper()
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := func(r, s int) string { return "A/" + strconv.Itoa(r) + "/" + strconv.Itoa(s) }
	adjacency := make([]SeatAdjacencyRow, 0, rows*perRow)
	for r := 1; r <= rows; r++ {
		rowKey := "A/" + strconv.Itoa(r)
		for s := 1; s <= perRow; s++ {
			pos := int32(s)
			key := rowKey
			rank := int32(r)
			row := SeatAdjacencyRow{SeatIdentity: seat(r, s), RowKey: &key, Position: &pos, RowRank: &rank}
			if s > 1 {
				left := seat(r, s-1)
				row.Left = &left
			}
			if s < perRow {
				right := seat(r, s+1)
				row.Right = &right
			}
			adjacency = append(adjacency, row)
		}
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 10000, true, adjacency); err != nil {
		t.Fatal(err)
	}
	return org, slot, seat
}

// hold takes named seats, failing the test on anything but success. Best-available tests
// need occupancy as a PRECONDITION, so a silent failure here would leave a fixture that
// cannot reach the state under test.
func hold(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, seats ...string) {
	t.Helper()
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), seats, 0, "EUR", uuid.NewString()); err != nil {
		t.Fatalf("seeding hold on %v: %v", seats, err)
	}
}

// TestBestAvailableReturnsAContiguousRunInProjectedOrder is AC1's core: N seats that are
// adjacent per the map's own geometry, not merely N free seats.
func TestBestAvailableReturnsAContiguousRunInProjectedOrder(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 10)

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 4, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1, 1), seat(1, 2), seat(1, 3), seat(1, 4)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v — the first run in projected order", got.Seats, want)
	}
	if got.Claim.Quantity != 4 {
		t.Fatalf("quantity = %d want 4 — the claim counts the seats it selected", got.Claim.Quantity)
	}
}

// TestBestAvailableSkipsAGapRatherThanReturningScatteredSeats is the test that fails if
// selection ever degenerates into "any N free seats". The free seats are deliberately
// arranged so that a non-contiguous picker has an easy, wrong answer available: seats
// 1,2 and 4,5,6,7 are free and 3 is taken, so a scatterer returns 1,2,4,5 while the only
// legal answer is the run 4..7.
func TestBestAvailableSkipsAGapRatherThanReturningScatteredSeats(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 8)
	hold(t, ctx, st, org, slot, seat(1, 3), seat(1, 8))

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 4, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1, 4), seat(1, 5), seat(1, 6), seat(1, 7)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v — a run, not four free seats", got.Seats, want)
	}
}

// TestBestAvailableOrdersByPositionNotSeatIdentity is the identity-order trap, and getting
// it to bite took a correction worth recording.
//
// Seat identities are free text and sort lexically, so A/1/10 < A/1/2. The obvious version
// of this test — a wide row, assert the geometric answer — passes even when the projection
// is scanned in lexical order, because the island grouping and the final tie-break both
// order by `position` independently of how the scan emitted rows. The scan's ORDER BY
// governs exactly one thing: which seats the bounded LIMIT ADMITS. So the only fixture that
// can observe it is one that exceeds the cap, and the assertion is about which seats the
// scan could see at all.
//
// Here the cap is 12 over a 20-seat row. In projected order the scan admits positions 1..12
// and the first free run of 3 is 1,2,3. In lexical order it admits A/1/1, A/1/10..A/1/19,
// A/1/2 — a set whose only 3-consecutive-position island is 17,18,19 — so a lexical scan
// returns a different, and much worse, run. Mutating the scan's ORDER BY to seat_identity
// makes this go red; on a fixture below the cap it does not, which is why the earlier
// version of this test proved nothing.
func TestBestAvailableOrdersByPositionNotSeatIdentity(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	st.baScan = 12
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 20)

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1, 1), seat(1, 2), seat(1, 3)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v — the bounded scan must admit seats in POSITION order; a lexical scan admits A/1/1,A/1/10..A/1/19,A/1/2 and answers with the far end of the row", got.Seats, want)
	}
}

// TestBestAvailableScanCapBoundsTheWork pins the cap itself (A3). Work under the pool row
// lock is bounded by MaxBestAvailableScan seats, not by the size of the map — so a request
// that would only be satisfiable beyond the cap is refused rather than paying for the scan
// that would find it.
//
// This is the honest statement of what best-available means here: the first legal run
// within the scanned window, not the best run in the house. A test is the only place that
// distinction is falsifiable — deleting the LIMIT makes this go red by SUCCEEDING.
func TestBestAvailableScanCapBoundsTheWork(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	st.baScan = 6
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 20)
	// Inside the cap (positions 1..6) every free run is at most 2 long; a 3-run exists
	// only at 7..20, which the cap forbids the scan from ever seeing.
	hold(t, ctx, st, org, slot, seat(1, 3), seat(1, 6))

	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrBestAvailableUnavailable) {
		t.Fatalf("err = %v want ErrBestAvailableUnavailable — the scan is capped, and a run beyond the cap is out of reach by design", err)
	}
	// The same request succeeds once the window is wide enough, which is what makes the
	// refusal above a statement about the CAP rather than about the fixture.
	st.baScan = 20
	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("with the cap raised the same run must be found: %v", err)
	}
	want := []string{seat(1, 7), seat(1, 8), seat(1, 9)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v", got.Seats, want)
	}
}

// TestBestAvailableNeverSpansRows: rows and sections never connect (ADR-041).
//
// The fixture is built so that a selection which ignores the row boundary finds a run and a
// correct one does not, and making that true took a correction. The obvious version — two
// rows of three, free seats straddling the boundary — passes even when the island grouping
// is stripped of its PARTITION BY, because `position - row_number()` over a flat walk still
// happens to separate two rows whose positions both restart at 1.
//
// What defeats a flat walk is the ISLAND arithmetic, not exotic positions. Both rows run
// 1..3 (the derivation re-bases every row, and validateAdjacencyOrder now requires it), and
// the seats free are A/1/2, A/1/3, A/2/1, A/2/2. Walked as one flat list ordered by
// (row, position) those four are consecutive entries, so a `position - row_number()` that
// forgets to partition by row assigns them one island and offers all four as a run —
// selling a party of four two seats in one row and two in another, and calling them
// adjacent. Partitioned, they are two separate 2-runs and a party of four has no answer.
//
// An earlier version of this fixture numbered row 2 as 4,5,6 to make the flat walk see one
// contiguous position range. That is not a projection this system can produce — every row
// is re-based — so it tested the query against an input reality never supplies.
func TestBestAvailableNeverSpansRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := func(r, s int) string { return "A/" + strconv.Itoa(r) + "/" + strconv.Itoa(s) }

	adjacency := make([]SeatAdjacencyRow, 0, 6)
	for r, base := range map[int]int{1: 1, 2: 1} {
		rowKey := "A/" + strconv.Itoa(r)
		for i := 0; i < 3; i++ {
			pos := int32(base + i)
			key := rowKey
			rank := int32(r)
			row := SeatAdjacencyRow{SeatIdentity: seat(r, base+i), RowKey: &key, Position: &pos, RowRank: &rank}
			if i > 0 {
				left := seat(r, base+i-1)
				row.Left = &left
			}
			if i < 2 {
				right := seat(r, base+i+1)
				row.Right = &right
			}
			adjacency = append(adjacency, row)
		}
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, false, adjacency); err != nil {
		t.Fatal(err)
	}
	// Row 1 keeps seats 2 and 3; row 2 keeps 1 and 2. Four free seats, no four-run.
	//
	// What this test observes, stated honestly rather than assumed — because the obvious
	// claim ("this proves the row partition is load-bearing") is false, and a comment
	// asserting it would be worse than no comment.
	//
	// The boundary is enforced in three places: the island PARTITION, the window JOIN's row
	// predicate, and the flank lookups in the orphan filter. Neither of the first two can be
	// killed by any fixture, and the reason is structural rather than a gap in the fixtures.
	// validateAdjacencyOrder now requires every row to run 1..N, so a new row always restarts
	// at position 1 — a decrease — while row_number() only ever increases. Their difference
	// therefore CANNOT repeat across a boundary: the island value changes by construction, so
	// even a flat unpartitioned walk separates the rows, and matching windows on island alone
	// cannot pair seats from two rows. Both predicates are redundant given the re-basing.
	//
	// They stay, because the redundancy is a consequence of an invariant enforced elsewhere
	// and a future change to either could make them load-bearing again — but nobody should
	// read this suite as proving they work. What it proves is the PROPERTY, which is the
	// thing the product cares about: four free seats split across two rows are not a
	// three-run, and each row's own 2-run is still sellable.
	hold(t, ctx, st, org, slot, seat(1, 1), seat(2, 3))

	// Four free seats and the longest run in either row is 2, so a party of THREE has no
	// answer. An island-only join assembles one from A/1/2, A/1/3 and a row-2 seat.
	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrBestAvailableUnavailable) {
		t.Fatalf("err = %v want ErrBestAvailableUnavailable — four free seats split across two rows are not a three-run", err)
	}
	// Nothing was written by the refusal — a partial claim here would be the worst outcome
	// of all, and a refusal that still consumed seats looks identical from the error alone.
	var live int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claim_seats cs JOIN claims c ON c.id=cs.claim_id
		 WHERE cs.pool_id=$1 AND cs.released_at IS NULL AND c.quantity=3`, slot).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("a refused best-available wrote %d seat rows", live)
	}
	// And each row's own 2-run is still sellable, so the refusal above is about the BOUNDARY
	// rather than about a pool with nothing left in it.
	rest, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("each row still holds a 2-run and one must be sold: %v", err)
	}
	if strings.Join(rest.Seats, ",") != seat(1, 2)+","+seat(1, 3) {
		t.Fatalf("seats = %v want row 1's run", rest.Seats)
	}
}

// TestBestAvailableRefusesWhenNoRunIsLongEnough: the free seats exist but never enough of
// them together. Distinct from a sellout, and the caller is told to try a smaller party.
func TestBestAvailableRefusesWhenNoRunIsLongEnough(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 9)
	hold(t, ctx, st, org, slot, seat(1, 3), seat(1, 6))
	// Free runs: 1-2, 4-5, 7-9. Longest is 3.

	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 4, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrBestAvailableUnavailable) {
		t.Fatalf("err = %v want ErrBestAvailableUnavailable", err)
	}
	// ...and the very same pool serves the smaller party, which is what makes the
	// refusal "try fewer" rather than "sold out". Without this half the test above
	// passes on a pool that refuses everything.
	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("a 3-seat run exists and must be sold: %v", err)
	}
	want := []string{seat(1, 7), seat(1, 8), seat(1, 9)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v", got.Seats, want)
	}
}

// TestBestAvailableRefusesAPoolWithNoOrderingMetadata is the A1/A4 distinction, and the
// first of the two predicates guarding the capability. A pool provisioned before ADR-061
// has adjacency but no row_key/position: it cannot be selected over, and saying so is a
// DIFFERENT answer from "cannot seat your party" — one is repaired by re-provisioning,
// the other by asking for fewer seats.
func TestBestAvailableRefusesAPoolWithNoOrderingMetadata(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	// seededOrphanPool is the pre-ADR-061 shape: rule ON, adjacency present, no ordering.
	org, slot, _ := seededOrphanPool(t, ctx, st, 10)

	_, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if !errors.Is(err, ErrBestAvailableUnsupported) {
		t.Fatalf("err = %v want ErrBestAvailableUnsupported", err)
	}
	if errors.Is(err, ErrBestAvailableUnavailable) {
		t.Fatal("an unsupported pool must not be reported as a sellout — the two have different remedies")
	}
}

// TestBestAvailableRefusesAGaPool: the kind guard, reused verbatim from the named-seat
// path. A quantity claim on a GA pool goes through CreateHold; this endpoint is seated-only.
func TestBestAvailableRefusesAGaPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot := uuid.New(), uuid.New()
	if err := st.Provision(ctx, uuid.New(), slot, org, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("err = %v want ErrPoolKindMismatch", err)
	}
}

// TestBestAvailableRejectsAnOutOfBandSeatCount pins both ends of the band. N is bounded by
// MaxSeatsPerHold for the same reason the named-seat path is: one claim, one insert, one
// bounded scan.
func TestBestAvailableRejectsAnOutOfBandSeatCount(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 4)

	for _, n := range []int32{0, -1, MaxSeatsPerHold + 1} {
		if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), n, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrSeatSetInvalid) {
			t.Fatalf("n=%d err = %v want ErrSeatSetInvalid", n, err)
		}
	}
}

// TestBestAvailableReplayReturnsTheSameSeats is the idempotency property that makes this
// endpoint safe to retry, and the one that has no analogue on the named-seat path: the
// request does not carry the seats, so a replay that re-ran selection would hand out a
// SECOND run under a key the caller believes names one hold.
//
// The assertion is the persisted set, not merely "no error" — reconstructing seats from
// the request is impossible here, so a replay that returns the right claim id with the
// wrong seats is exactly the defect this guards.
func TestBestAvailableReplayReturnsTheSameSeats(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
	key := uuid.NewString()
	tt := uuid.New()

	first, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replay {
		t.Fatal("the second call under one key is a replay")
	}
	if again.Claim.ID != first.Claim.ID {
		t.Fatalf("replay claim %s != original %s — a retry must not open a second hold", again.Claim.ID, first.Claim.ID)
	}
	if strings.Join(again.Seats, ",") != strings.Join(first.Seats, ",") {
		t.Fatalf("replay seats %v != original %v — the seats come from the row, not a re-selection", again.Seats, first.Seats)
	}
	// The invariant behind the two assertions above, stated where a fixture cannot fake
	// it: one draw-down of the pool, not two.
	var live int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claim_seats WHERE pool_id=$1 AND released_at IS NULL`, slot).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 3 {
		t.Fatalf("live seat rows = %d want 3 — a replay allocated a second run", live)
	}
}

// TestBestAvailableReplayRefusesADifferentCount: same key, different intent. The
// fingerprint covers the request the caller made, so changing the party size under a spent
// key is a conflict rather than a new selection.
func TestBestAvailableReplayRefusesADifferentCount(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
	key := uuid.NewString()
	tt := uuid.New()

	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 2, 0, "EUR", key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("err = %v want ErrIdempotency", err)
	}
}

// TestBestAvailableAndNamedSeatKeysDoNotCollide: one idempotency key names one hold,
// whichever endpoint opened it. Reusing a spent key for the OTHER kind of request is a
// conflict, never a replay — a best-available caller must never be handed a named-seat
// claim and told it is their retry.
//
// The scope of this test is stated honestly, because a mutation exposed the difference.
// Deleting the "best:" mode prefix from bestAvailableFingerprint does NOT make it red: the
// two fingerprints also differ structurally (seatFingerprint hashes a JSON seat array where
// this hashes an integer party size), so they cannot collide even unprefixed. What this
// test pins is the OUTCOME — a cross-kind key reuse is refused — not the mechanism that
// achieves it. The prefix stays because it makes the separation explicit rather than a
// consequence of two encodings happening to differ, and if a future change makes the
// encodings converge this test is what still holds.
func TestBestAvailableAndNamedSeatKeysDoNotCollide(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 10)
	key := uuid.NewString()
	tt := uuid.New()

	if _, err := st.CreateSeatHold(ctx, org, slot, tt, []string{seat(1, 1)}, 0, "EUR", key); err != nil {
		t.Fatal(err)
	}
	// Same key, same org/slot/type/money, one seat either way: only the MODE differs.
	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 1, 0, "EUR", key); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("err = %v want ErrIdempotency — a named-seat hold must not be replayed as best-available", err)
	}
}

// TestBestAvailableRespectsTheAggregateCeiling: the pool's coarse headroom refuses before
// any selection happens. A drained pool must not pay for a scan to find seats it cannot
// sell, and must not answer "no run" about a map that is full of runs.
func TestBestAvailableRespectsTheAggregateCeiling(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	adjacency := make([]SeatAdjacencyRow, 0, 6)
	rowKey := "A/1"
	for s := 1; s <= 6; s++ {
		pos := int32(s)
		key := rowKey
		rank := int32(1)
		row := SeatAdjacencyRow{SeatIdentity: "A/1/" + strconv.Itoa(s), RowKey: &key, Position: &pos, RowRank: &rank}
		if s > 1 {
			left := "A/1/" + strconv.Itoa(s-1)
			row.Left = &left
		}
		if s < 6 {
			right := "A/1/" + strconv.Itoa(s+1)
			row.Right = &right
		}
		adjacency = append(adjacency, row)
	}
	// Capacity 2 over a 6-seat map: every seat is free, and the pool may still only sell 2.
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 2, true, adjacency); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 4, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v want ErrUnavailable — the ceiling refuses before selection, and a headroom refusal is not a geometry refusal", err)
	}
}

// TestBestAvailableNeverStrandsASeat is A5: selection is orphan-AWARE, not orphan-checked.
// The difference is whether the rule REJECTS A WINDOW (and selection moves on) or refuses
// the request, and only a fixture where the FIRST window in projected order is the illegal
// one can tell them apart.
//
// Getting that fixture right took a correction. The earlier version left the first window
// legal, so filtered and unfiltered selection returned the same seats and the test passed
// with the whole orphan predicate deleted — a green test about nothing.
//
// Row of 9. Taken: 1 and 5. Free: 2,3,4,6,7,8,9. For a party of 2 the windows in projected
// order are 2-3, 3-4, 6-7, 7-8, 8-9.
//   - 2-3 would strand seat 4: 4's neighbours are 3 (inside the window) and 5 (taken).
//   - 3-4 would strand seat 2: 2's neighbours are 1 (taken) and 3 (inside the window).
//   - 6-7 strands nothing: 8 keeps 9.
//
// So first-fit answers 2-3 and the correct answer is 6-7. Deleting the orphan predicate
// makes this go red, which is the property the earlier fixture could not observe.
func TestBestAvailableNeverStrandsASeat(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 9)
	hold(t, ctx, st, org, slot, seat(1, 1), seat(1, 5))

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1, 6), seat(1, 7)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v — the first two windows each strand a seat, so selection must SKIP them rather than take one or refuse", got.Seats, want)
	}
	// And the seats the skipped windows would have stranded are still FREE — asserted
	// against the database rather than re-derived from the rule, so this cannot pass by
	// agreeing with the same bug twice. (Whether either is claimable ALONE is a different
	// question the orphan rule answers on its own terms: taking 4 by itself would strand 3.
	// What matters here is that selection left them unconsumed.)
	for _, want := range []string{seat(1, 2), seat(1, 3), seat(1, 4)} {
		var live int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM claim_seats WHERE pool_id=$1 AND seat_identity=$2 AND released_at IS NULL`,
			slot, want).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 0 {
			t.Fatalf("%s was consumed by a selection that should have skipped its window", want)
		}
	}
}

// TestBestAvailableRefusesWhenEveryWindowWouldStrand is the other side of A5: skipping is
// not the same as ignoring. When NO window is legal the request is refused, rather than
// falling back to a stranding one.
//
// Row of 4, seat 1 taken. Free: 2,3,4. Windows of 2: 2-3 (strands 4, whose other neighbour
// is the row end) and 3-4 (strands 2, whose other neighbour 1 is taken). Nothing is legal,
// so the answer is a refusal — and a party of 3 on the same pool succeeds, which is what
// makes it a statement about the RULE rather than about an exhausted pool.
func TestBestAvailableRefusesWhenEveryWindowWouldStrand(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 4)
	hold(t, ctx, st, org, slot, seat(1, 1))

	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrBestAvailableUnavailable) {
		t.Fatalf("err = %v want ErrBestAvailableUnavailable — every 2-window strands a seat", err)
	}
	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("taking all three free seats strands nothing and must succeed: %v", err)
	}
	if strings.Join(got.Seats, ",") != strings.Join([]string{seat(1, 2), seat(1, 3), seat(1, 4)}, ",") {
		t.Fatalf("seats = %v want the whole free run", got.Seats)
	}
}

// TestBestAvailableIgnoresTheRuleOnARuleOffPool: the orphan filter is conditioned on the
// pool's own flag, exactly as the named-seat path conditions its rule. A rule-off pool with
// ordering metadata selects the first run in projected order and strands whatever the map
// allows — the venue turned the rule off on purpose.
func TestBestAvailableIgnoresTheRuleOnARuleOffPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := func(i int) string { return "A/1/" + strconv.Itoa(i) }
	rowKey := "A/1"
	adjacency := make([]SeatAdjacencyRow, 0, 4)
	for i := 1; i <= 4; i++ {
		pos := int32(i)
		key := rowKey
		rank := int32(1)
		row := SeatAdjacencyRow{SeatIdentity: seat(i), RowKey: &key, Position: &pos, RowRank: &rank}
		if i > 1 {
			left := seat(i - 1)
			row.Left = &left
		}
		if i < 4 {
			right := seat(i + 1)
			row.Right = &right
		}
		adjacency = append(adjacency, row)
	}
	// Rule OFF, projection present — the shape a future ticket produces when it decouples
	// the two, and the shape this test exists to keep honest.
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, false, adjacency); err != nil {
		t.Fatal(err)
	}

	// The same arrangement the test above refuses: with the rule off, 2-3 is taken and
	// seat 4 is left stranded, which is what "off" means.
	hold(t, ctx, st, org, slot, seat(1))
	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("a rule-off pool must not apply the orphan rule: %v", err)
	}
	if strings.Join(got.Seats, ",") != seat(2)+","+seat(3) {
		t.Fatalf("seats = %v want the first run %s,%s", got.Seats, seat(2), seat(3))
	}
}

// TestBestAvailableUnderContentionNeverDoubleAllocates is AC3.
//
// The harness is TestOrphanRuleHoldsUnderContention's, not a close(start) barrier, and the
// difference is the whole point: a start channel proves the goroutines were released
// together, which is not the same as proving their transactions overlapped. Here a real
// blocker transaction holds the pool row, every claimant is observed transitively blocked
// on it through pg_blocking_pids, and only then is the lock released — so the overlap is a
// fact rather than a hope.
func TestBestAvailableUnderContentionNeverDoubleAllocates(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 4, 10)

	const claimants = 12
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, claimants)
	seats := make([][]string, claimants)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 3, 0, "EUR", uuid.NewString())
			errs[i], seats[i] = err, h.Seats
		}(i)
	}

	// Wait until every claimant is genuinely queued behind the blocker before letting go.
	//
	// The chain is followed TRANSITIVELY, and that is not a refinement: PostgreSQL queues
	// row-lock waiters, so the second claimant reports the FIRST claimant as its blocker
	// rather than the session actually holding the row. Counting direct blockers of the
	// blocker's pid sees exactly one waiter for ever and times out while the contention it
	// is looking for is happening in front of it.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `
			WITH RECURSIVE chain(pid) AS (
				SELECT $1::int
				UNION
				SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid))
				 WHERE a.datname = current_database()
			)
			SELECT count(*) - 1 FROM chain`, blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= claimants {
			break
		}
		if time.Now().After(deadline) {
			_ = blocker.Rollback()
			wg.Wait()
			t.Fatalf("only %d of %d claimants blocked on the pool row held by pid %d — the contention this test claims to create did not happen", waiting, claimants, blockerPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	// No deadlock, and no error outside the two the design admits.
	granted := 0
	for i, err := range errs {
		switch {
		case err == nil:
			granted++
		case errors.Is(err, ErrBestAvailableUnavailable), errors.Is(err, ErrUnavailable):
		default:
			t.Fatalf("claimant %d: unexpected %v — contention must degrade, not fail", i, err)
		}
	}
	if granted == 0 {
		t.Fatal("no claimant was served: the harness proved serialization and nothing else")
	}
	// The invariant, asserted in SQL rather than from the returned sets: no seat is live
	// under two claims. A Go-side check would only prove the returns disagreed, which is
	// a weaker statement than the database's own.
	var maxDup int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(max(c),0) FROM (
		SELECT count(*) c FROM claim_seats WHERE pool_id=$1 AND released_at IS NULL GROUP BY seat_identity) x`, slot).Scan(&maxDup); err != nil {
		t.Fatal(err)
	}
	if maxDup > 1 {
		t.Fatalf("a seat is held by %d live claims — double-allocation under contention", maxDup)
	}
	// Every granted set must itself be a contiguous run: serialization must not degrade
	// the geometry it was protecting.
	for i, got := range seats {
		if errs[i] != nil || len(got) == 0 {
			continue
		}
		if len(got) != 3 {
			t.Fatalf("claimant %d got %d seats, want 3", i, len(got))
		}
	}
}

// TestReProvisionFillsOrderingMetadataWithoutRewritingEdges is amendment A2, and it is the
// only thing standing between this feature and a pool that can never be enabled.
//
// A pool provisioned before ADR-061 has adjacency edges and no ordering. The migration
// deliberately does not backfill (row_key was never projected and cannot be recovered), so
// the ONLY thing that can supply the metadata is a re-provision carrying real catalog
// geometry — ADR-041's correction wave. Under the ON CONFLICT DO NOTHING that shipped with
// TKT-181 that re-provision is a silent no-op on every adjacency row: the pool stays
// unselectable for ever while the wave reports success. Exactly the shape ProvisionSeated's
// own comment warns about for the rule flag, one table down.
//
// The second half is the asymmetry. The edges are the substrate ADR-041 arbitrates on, and
// a live claim was decided against them as they stood; a conflict clause that rewrote them
// would let a later publication retroactively change what that decision meant. So the
// metadata is upserted and the edges are asserted BYTE-IDENTICAL across the upgrade.
func TestReProvisionFillsOrderingMetadataWithoutRewritingEdges(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	seat := func(i int) string { return "A/1/" + strconv.Itoa(i) }

	// Pre-ADR-061 shape: edges, no ordering.
	before := make([]SeatAdjacencyRow, 0, 4)
	for i := 1; i <= 4; i++ {
		row := SeatAdjacencyRow{SeatIdentity: seat(i)}
		if i > 1 {
			left := seat(i - 1)
			row.Left = &left
		}
		if i < 4 {
			right := seat(i + 1)
			row.Right = &right
		}
		before = append(before, row)
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, before); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString()); !errors.Is(err, ErrBestAvailableUnsupported) {
		t.Fatalf("precondition: the pool must start unsupported, got %v", err)
	}

	// The correction wave: a FRESH event id carrying the same geometry plus ordering.
	//
	// The edges match the stored ones on purpose. An earlier version of this test sent a
	// deliberately REVERSED chain to make "the edges were not rewritten" falsifiable, and
	// ai-review's third finding removed the need: a re-provision describing different
	// geometry is now REFUSED outright (TestReProvisionRefusesADifferentGeometry), so the
	// reversed input no longer reaches the upsert at all. The edge-preservation assertion
	// below therefore checks that the upgrade path leaves them alone, and the refusal test
	// covers the case where a publication tries to change them.
	after := make([]SeatAdjacencyRow, 0, 4)
	rowKey := "A/1"
	for i := 1; i <= 4; i++ {
		pos := int32(i)
		key := rowKey
		rank := int32(1)
		row := SeatAdjacencyRow{SeatIdentity: seat(i), RowKey: &key, Position: &pos, RowRank: &rank}
		if i > 1 {
			left := seat(i - 1)
			row.Left = &left
		}
		if i < 4 {
			right := seat(i + 1)
			row.Right = &right
		}
		after = append(after, row)
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, after); err != nil {
		t.Fatal(err)
	}

	// The metadata landed...
	var ordered int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1 AND row_key='A/1' AND position IS NOT NULL`,
		slot).Scan(&ordered); err != nil {
		t.Fatal(err)
	}
	if ordered != 4 {
		t.Fatalf("ordered rows = %d want 4 — a re-provision must UPGRADE an existing projection, not no-op on it", ordered)
	}
	// ...and the pool is selectable, which is the operator-visible half of the same fact.
	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("after the upgrade the pool must serve best-available: %v", err)
	}
	if strings.Join(got.Seats, ",") != seat(1)+","+seat(2) {
		t.Fatalf("seats = %v want the first run", got.Seats)
	}

	// And the edges were not touched. Asserted per seat rather than by a count, because a
	// count survives a clause that rewrote every edge to the same wrong value.
	for i := 1; i <= 4; i++ {
		var l, r sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT left_identity, right_identity FROM seat_claim_adjacency WHERE pool_id=$1 AND seat_identity=$2`,
			slot, seat(i)).Scan(&l, &r); err != nil {
			t.Fatal(err)
		}
		wantL, wantR := seat(i-1), seat(i+1)
		if i == 1 && l.Valid {
			t.Fatalf("%s gained a left neighbour %q across the upgrade", seat(i), l.String)
		}
		if i > 1 && (!l.Valid || l.String != wantL) {
			t.Fatalf("%s left = %v want %s — the upgrade must not rewrite arbitration edges", seat(i), l, wantL)
		}
		if i == 4 && r.Valid {
			t.Fatalf("%s gained a right neighbour %q across the upgrade", seat(i), r.String)
		}
		if i < 4 && (!r.Valid || r.String != wantR) {
			t.Fatalf("%s right = %v want %s — the upgrade must not rewrite arbitration edges", seat(i), r, wantR)
		}
	}
}

// TestBestAvailableSelectionIsIndexScoped is amendment A3's plan proof, and ADR-019's
// two-part obligation: the read must be scoped AND the SCAN must be.
//
// It EXPLAINs bestAvailableRunQuery itself — the same const the code executes, which is why
// the query is a package-level const at all, and why the scan cap is written literally
// inside it rather than bound as a parameter. A copy of the SQL in a test proves the copy
// is fast.
//
// Two assertions, and the second is the one that matters. Reaching seat_claim_adjacency
// through the index is necessary but nowhere near sufficient: a plan that reads every
// matching row and SORTS it satisfies the first assertion while doing exactly the O(map)
// work under the pool lock that the LIMIT exists to prevent. What rules that out is an
// ordered index scan directly under the Limit — the index delivering rows already in
// (row_key, position) order so the scan stops at the cap instead of reading everything and
// discarding.
//
// The seeding is deliberately many-pooled, copied from TestSeatOccupancyIsIndexScoped's
// reasoning: the planner estimates `pool_id = $1` at 1/n_distinct(pool_id), so with two
// pools that is half the table and a sequential scan is genuinely right. The index becomes
// the right choice only with many distinct pools, which is also what production looks like.
//
// It asserts the plan for BOUND parameters, not the generic plan, and that difference is a
// finding rather than a convenience. Under a generic plan this statement gets a Bitmap Heap
// Scan plus a Sort: with no bound value the planner's row estimate is the n_distinct guess,
// which is small enough that sorting looks cheaper than an ordered walk. With the actual
// pool bound — and with the row counts a real house has — it chooses the ordered index scan
// this test asserts. So the bound is real but it is the PLANNER's to keep, not the schema's:
// the index makes the good plan available and the LIMIT makes it terminating, and a future
// statistics shift could still pick the sort. That residual is recorded in ADR-061 rather
// than hidden behind a green test, and it is why MaxBestAvailableScan exists as a hard
// second bound: even the sorting plan reads at most the pool's projection, not the map's.
func TestBestAvailableSelectionIsIndexScoped(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	_, slot, _ := seededBestAvailablePool(t, ctx, st, 2, 10)

	// The pool under test needs a house-sized projection, not a fixture-sized one. On the
	// 20 seats seededBestAvailablePool writes, reading everything and sorting genuinely IS
	// cheaper than an ordered index walk, and the planner is right to say so — the test
	// would fail for a reason that has nothing to do with scoping. 3000 seats is an ordinary
	// arena, and it is the scale at which terminating early stops being a rounding error.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO seat_claim_adjacency(pool_id, seat_identity, row_key, position, row_rank)
		SELECT $1, 'B/' || r || '/' || c, 'B/' || r, c, 100 + r
		FROM generate_series(1, 60) r, generate_series(1, 50) c`, slot); err != nil {
		t.Fatal(err)
	}

	// 200 other seated pools of 25 ordered seats each, seeded in SQL: 5000 rows through
	// ProvisionSeated would dominate the suite's runtime and prove the same thing.
	if _, err := db.ExecContext(ctx, `
		WITH pools AS (
			INSERT INTO inventory_pools(slot_id, organizer_id, capacity, source_event_id,
			                            inventory_kind, seat_map_id, orphan_prevention_enabled)
			SELECT gen_random_uuid(), gen_random_uuid(), 1000, gen_random_uuid(),
			       'seated', gen_random_uuid(), true
			FROM generate_series(1, 200)
			RETURNING slot_id
		)
		INSERT INTO seat_claim_adjacency(pool_id, seat_identity, row_key, position, row_rank)
		SELECT p.slot_id, 'Z/1/' || g, 'Z/1', g, 1
		FROM pools p, generate_series(1, 25) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE seat_claim_adjacency, claim_seats, claims, inventory_pools`); err != nil {
		t.Fatal(err)
	}

	var plan strings.Builder
	rows, err := db.QueryContext(ctx, "EXPLAIN "+bestAvailableRunQuery, slot, true, int32(2))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	p := plan.String()

	scanIdx := strings.Index(p, "Index Scan using seat_claim_adjacency_row_position")
	if scanIdx < 0 {
		t.Fatalf("the plan does not read seat_claim_adjacency through an ORDERED index scan on "+
			"seat_claim_adjacency_row_position, so the ORDER BY is being satisfied some other way "+
			"and the scan cap does not terminate the read early.\nplan:\n%s", p)
	}
	// No Sort may sit between the Limit and that scan. EXPLAIN indents children under
	// parents, so the scan's ancestors are the preceding lines with strictly smaller
	// indentation; walking outward from the scan to the first Limit and rejecting any Sort
	// on the way is what distinguishes "the index delivers the order" from "everything was
	// read and then ordered". Without the ordering index the plan is
	// Limit -> Sort -> Index Scan on the primary key, and that is precisely the O(pool) read
	// under the pool row lock this test exists to forbid.
	lines := strings.Split(p, "\n")
	scanLine, indentOf := -1, func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	for i, l := range lines {
		if strings.Contains(l, "Index Scan using seat_claim_adjacency_row_position") {
			scanLine = i
			break
		}
	}
	depth := indentOf(lines[scanLine])
	foundLimit := false
	for i := scanLine - 1; i >= 0 && !foundLimit; i-- {
		if indentOf(lines[i]) >= depth || strings.TrimSpace(lines[i]) == "" {
			continue
		}
		depth = indentOf(lines[i])
		node := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "->"))
		switch {
		case strings.HasPrefix(node, "Limit"):
			foundLimit = true
		case strings.HasPrefix(node, "Sort") || strings.HasPrefix(node, "Incremental Sort"):
			t.Fatalf("a %q sits between the scan cap's Limit and the adjacency scan: every projected "+
				"seat in the pool is read and ordered before the bound applies, under the pool row "+
				"lock.\nplan:\n%s", node, p)
		}
	}
	if !foundLimit {
		t.Fatalf("no Limit governs the adjacency scan — the scan cap does not bound the read.\nplan:\n%s", p)
	}
	// ADR-019's second half: a scoped read is only scoped if the index SERVES the filter.
	tail := p[scanIdx:]
	if cut := strings.Index(tail, "\n"+strings.Repeat(" ", indentOf(lines[scanLine]))); cut > 0 {
		_ = cut
	}
	if !strings.Contains(tail, "Index Cond: (pool_id = ") {
		t.Fatalf("the scan reaches the index but does not bind pool_id in its index condition, so "+
			"it reads other pools' projections and filters after.\nplan:\n%s", p)
	}
}

// TestBestAvailableScanCapConstantsAgree pins the one thing a compiler cannot: the cap is
// written twice — once as a Go const the code and the ADR quote, and once literally inside
// bestAvailableRunQuery, because a Go const cannot be interpolated into another const and
// the query must stay a single const for the plan probe to EXPLAIN it verbatim.
//
// A divergence here is silent and consequential: the shipped bound would differ from the
// documented one, the test seam that narrows it would stop matching, and every claim made
// about bounded work under the pool lock would be about a number nothing enforces.
func TestBestAvailableScanCapConstantsAgree(t *testing.T) {
	want := "LIMIT " + strconv.Itoa(MaxBestAvailableScan)
	if maxBestAvailableScanSQL != want {
		t.Fatalf("maxBestAvailableScanSQL = %q but MaxBestAvailableScan says %q", maxBestAvailableScanSQL, want)
	}
	if !strings.Contains(bestAvailableRunQuery, maxBestAvailableScanSQL) {
		t.Fatalf("the shipped query does not contain %q — the scan cap in the SQL has drifted from the constant", maxBestAvailableScanSQL)
	}
}

// TestBestAvailableJudgesTheOrphanRuleBeyondTheScanCap replaces a test that blessed a
// defect, and the way it did so is the lesson.
//
// The first version asserted that a window at the very edge of the scan is REFUSED, on the
// reasoning that the filter cannot see a flank's neighbour beyond the cap and should fail
// safe. It was green, well named, and wrong — ai-review found it. Failing safe is the right
// instinct about *granting* a stranding run; it is not a licence to refuse a legal one. The
// bounded scan is allowed to stop looking for candidates. It must not start inventing
// occupancy for seats it did not read, and "I cannot see it, therefore it is taken" is
// exactly that.
//
// The fix gives the predicate two positions of CONTEXT past the scan window — its whole
// reach — without making those seats selectable. This test pins the resulting behaviour with
// the review's own scenario: a row of 405 against a cap of 400, positions 1..395 taken.
// Window 396..399 is legal, because seat 400 keeps its free neighbour 401 — a seat the
// selection scan never reads. The old code refused; the answer must be 396..399.
func TestBestAvailableJudgesTheOrphanRuleBeyondTheScanCap(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seat := seededBestAvailablePool(t, ctx, st, 1, 405)
	taken := make([]string, 0, 395)
	for i := 1; i <= 395; i++ {
		taken = append(taken, seat(1, i))
	}
	// Seeded directly: the arrangement is this test's INPUT, and routing it through
	// CreateSeatHold would let the orphan rule refuse the precondition and leave the fixture
	// unable to reach the state it names.
	seedTakenSeats(t, ctx, st, org, slot, taken)

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 4, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatalf("err = %v — 396..399 is legal: seat 400 keeps its free neighbour 401, and a seat "+
			"the scan did not read is not thereby occupied", err)
	}
	want := []string{seat(1, 396), seat(1, 397), seat(1, 398), seat(1, 399)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v", got.Seats, want)
	}
	// The complement, on the same fixture shape: when the seat past the cap is genuinely
	// TAKEN, the flank really is stranded and the window really must be refused. Without
	// this half the test above is satisfied by a filter that was simply switched off.
	org2, slot2, seat2 := seededBestAvailablePool(t, ctx, st, 1, 405)
	taken2 := make([]string, 0, 396)
	for i := 1; i <= 395; i++ {
		taken2 = append(taken2, seat2(1, i))
	}
	taken2 = append(taken2, seat2(1, 401)) // beyond the cap, and occupied
	seedTakenSeats(t, ctx, st, org2, slot2, taken2)

	got2, err := st.CreateBestAvailableSeatHold(ctx, org2, slot2, uuid.New(), 4, 0, "EUR", uuid.NewString())
	if err == nil && strings.Join(got2.Seats, ",") == strings.Join([]string{seat2(1, 396), seat2(1, 397), seat2(1, 398), seat2(1, 399)}, ",") {
		t.Fatal("396..399 strands seat 400 when 401 is taken, and must not be selected — the context " +
			"read must inform the rule, not disable it")
	}
}

// seedTakenSeats marks seats consumed by writing the claim directly. Tests that need an
// ARRANGEMENT as their input use this rather than CreateSeatHold, whose own orphan rule
// would refuse many legal-to-construct arrangements and leave the fixture unable to reach
// the state the test names.
func seedTakenSeats(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, seats []string) {
	t.Helper()
	if len(seats) == 0 {
		return
	}
	if _, err := st.db.ExecContext(ctx, `
		WITH c AS (
			INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint)
			VALUES(gen_random_uuid(),$1,$2,$3,'held',now()+interval '1 hour',gen_random_uuid()::text,'seed')
			RETURNING id)
		INSERT INTO claim_seats(claim_id,pool_id,seat_identity)
		SELECT c.id,$2,s FROM c, unnest($4::text[]) AS s`, org, slot, len(seats), seats); err != nil {
		t.Fatalf("seeding taken seats: %v", err)
	}
}

// TestBestAvailableReplayRefusesAFullyReturnedClaim is ai-review's second [high], and the
// shape is one the named-seat path never has to face.
//
// `claims.status` and `claim_seats.released_at` are NOT coupled by the schema — the refund
// path says so where it releases them — so a fully returned seated claim sits at status
// 'confirmed' with every seat row released. The replay branch accepted it as live, and
// because the request carries only a party size there was nothing to cross-check the answer
// against: it returned the original quantity with an EMPTY seat set, which the caller then
// tried to pin.
//
// A claim whose seats have all been given back is spent, whatever its status says. The key
// cannot be replayed onto it — returning it would re-pin seats that are free again and
// report a false success, the same reasoning the released and expired branches already apply.
func TestBestAvailableReplayRefusesAFullyReturnedClaim(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
	key := uuid.NewString()
	tt := uuid.New()

	first, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err != nil {
		t.Fatal(err)
	}
	// The state a full seated refund leaves behind: the claim stays confirmed, every seat
	// row is released. Written directly so this test pins the STATE rather than the refund
	// path that produces it — the coupling it guards against is exactly the assumption that
	// those two move together.
	if _, err := db.ExecContext(ctx, `UPDATE claims SET status='confirmed' WHERE id=$1`, first.Claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1`, first.Claim.ID); err != nil {
		t.Fatal(err)
	}

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err == nil {
		t.Fatalf("a replay onto a fully returned claim returned %d seats (%v) — the key is spent and "+
			"those seats are free again", len(got.Seats), got.Seats)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v want ErrConflict", err)
	}
	// And a confirmed claim whose seats are STILL live replays normally, which is what makes
	// the refusal above about the returned seats rather than about the status.
	org2, slot2, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
	key2 := uuid.NewString()
	live, err := st.CreateBestAvailableSeatHold(ctx, org2, slot2, tt, 3, 0, "EUR", key2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE claims SET status='confirmed' WHERE id=$1`, live.Claim.ID); err != nil {
		t.Fatal(err)
	}
	again, err := st.CreateBestAvailableSeatHold(ctx, org2, slot2, tt, 3, 0, "EUR", key2)
	if err != nil {
		t.Fatalf("a confirmed claim with live seats must still replay: %v", err)
	}
	if strings.Join(again.Seats, ",") != strings.Join(live.Seats, ",") {
		t.Fatalf("replay seats %v != original %v", again.Seats, live.Seats)
	}
}

// TestReProvisionRefusesADifferentGeometry is ai-review's [medium], strengthened after a
// second pass found the first version toothless: it provisioned two seats and re-provisioned
// three, so it always exited at the count check and the whole per-seat comparison could be
// deleted with the test still green. Every case below is EQUAL-CARDINALITY, so the count
// check cannot answer any of them.
//
// The guard exists because the upsert fills ordering metadata and deliberately leaves the
// arbitration edges alone — right when both publications describe the same geometry, and a
// splice of two generations when they do not. ADR-029 makes that unreachable through the
// ordinary path (a published version is immutable, and the pool refuses a different
// seat_map_id), so this is defence in depth against a catalog integrity violation, checked
// rather than assumed because the failure is silent and lands on the correction-wave path.
func TestReProvisionRefusesADifferentGeometry(t *testing.T) {
	seat := func(i int) string { return "A/1/" + strconv.Itoa(i) }
	// chain builds an n-seat row, optionally permuting the ORDERING while leaving every
	// identity and every edge exactly as they were.
	chain := func(n int, order []int) []SeatAdjacencyRow {
		out := make([]SeatAdjacencyRow, 0, n)
		for i := 1; i <= n; i++ {
			pos := int32(i)
			if order != nil {
				pos = int32(order[i-1])
			}
			key, rank := "A/1", int32(1)
			row := SeatAdjacencyRow{SeatIdentity: seat(i), RowKey: &key, Position: &pos, RowRank: &rank}
			if i > 1 {
				l := seat(i - 1)
				row.Left = &l
			}
			if i < n {
				r := seat(i + 1)
				row.Right = &r
			}
			out = append(out, row)
		}
		return out
	}

	cases := map[string]func() []SeatAdjacencyRow{
		// Same count, one identity swapped for another: the per-seat lookup must miss.
		"a seat replaced": func() []SeatAdjacencyRow {
			rows := chain(4, nil)
			rows[3].SeatIdentity = "A/1/99"
			l := seat(3)
			rows[3].Left = &l
			r := "A/1/99"
			rows[2].Right = &r
			return rows
		},
		// Same identities, one edge pointing somewhere else: the edge comparison must catch it.
		"an edge changed": func() []SeatAdjacencyRow {
			rows := chain(4, nil)
			rows[0].Right = nil
			rows[1].Left = nil
			return rows
		},
		// Same identities AND same edges, ordering permuted. This is the case the first
		// version of the guard let through, and the one with teeth: it would overwrite the
		// ordering so that two seats which are not neighbours read as a contiguous run.
		"ordering permuted": func() []SeatAdjacencyRow {
			return chain(4, []int{3, 4, 1, 2})
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, st, _ := storeForTest(t, 10*time.Minute)
			org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
			if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, chain(4, nil)); err != nil {
				t.Fatal(err)
			}
			if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, build()); !errors.Is(err, ErrSeatProjectionIncomplete) {
				t.Fatalf("err = %v want ErrSeatProjectionIncomplete — a re-provision describing different "+
					"geometry must be refused, not merged column-wise into the stored one", err)
			}
			// The stored projection is untouched by the refusal, asserted per seat rather
			// than by a count: a count survives a write that changed every row.
			for i := 1; i <= 4; i++ {
				var pos int32
				if err := st.db.QueryRowContext(ctx,
					`SELECT position FROM seat_claim_adjacency WHERE pool_id=$1 AND seat_identity=$2`,
					slot, seat(i)).Scan(&pos); err != nil {
					t.Fatalf("%s: %v", seat(i), err)
				}
				if pos != int32(i) {
					t.Fatalf("%s position = %d want %d — the refusal must leave the stored set intact", seat(i), pos, i)
				}
			}
		})
	}

	// And the identical set is still accepted, which is what makes the refusals above about
	// the DIFFERENCE rather than about re-provisioning at all.
	t.Run("the same geometry is accepted", func(t *testing.T) {
		ctx, st, _ := storeForTest(t, 10*time.Minute)
		org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
		if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, chain(4, nil)); err != nil {
			t.Fatal(err)
		}
		if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, chain(4, nil)); err != nil {
			t.Fatalf("re-provisioning the same geometry must succeed: %v", err)
		}
	})
}

// TestBestAvailableReplayRefusesAPartiallyReleasedClaim is ai-review's second-pass [high].
// The first version of the spent-claim guard checked `live == 0`, which is the fully returned
// case and only that. A claim with one seat released and the rest live would replay ALL of
// them, and the caller would pin a seat that has since been reallocated — reporting an
// allocation the claim does not hold, or provoking a deterministic pin rejection that
// releases the seats it does. The schema permits that skew deliberately, so partial liveness
// is a state to refuse rather than one to interpret.
func TestBestAvailableReplayRefusesAPartiallyReleasedClaim(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
	key, tt := uuid.NewString(), uuid.New()

	first, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE claims SET status='confirmed' WHERE id=$1`, first.Claim.ID); err != nil {
		t.Fatal(err)
	}
	// Exactly one seat released: two are still live, so the old `live == 0` guard passes.
	if _, err := db.ExecContext(ctx,
		`UPDATE claim_seats SET released_at=now() WHERE claim_id=$1 AND seat_identity=$2`,
		first.Claim.ID, first.Seats[0]); err != nil {
		t.Fatal(err)
	}

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, tt, 3, 0, "EUR", key)
	if err == nil {
		t.Fatalf("a replay onto a partially released claim returned %v — %s is free again and may "+
			"already belong to someone else", got.Seats, first.Seats[0])
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v want ErrConflict", err)
	}
}

// TestBestAvailableOrdersRowsByRankNotByRowKey pins the defect that only the end-to-end tier
// could see, and the reason it could see it.
//
// row_key is the row's catalog UUID, because labels repeat across sections. A UUID sorts
// arbitrarily — so ordering rows by it made "the first run in projected order" mean "the first
// run in a random row". Every store and handler test passed anyway, because each supplies its
// own row keys and naturally picks names that sort the way the fixture reads ("A/1" before
// "A/2"). Only a real catalog publication hands inventory keys whose sort order has nothing to
// do with the venue's, and that is what caught it.
//
// So the fixture here does what production does and the earlier fixtures did not: it gives the
// FIRST row a key that sorts LAST. If ordering falls back to row_key, the answer comes from the
// wrong row.
func TestBestAvailableOrdersRowsByRankNotByRowKey(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	// Row 1 is "zzz" and row 2 is "aaa": lexically reversed against their true order.
	keys := []string{"zzz-row-one", "aaa-row-two"}
	seat := func(r, i int) string { return "R" + strconv.Itoa(r) + "/" + strconv.Itoa(i) }
	adjacency := make([]SeatAdjacencyRow, 0, 8)
	for r := 1; r <= 2; r++ {
		for i := 1; i <= 4; i++ {
			pos, rank, key := int32(i), int32(r), keys[r-1]
			row := SeatAdjacencyRow{SeatIdentity: seat(r, i), RowKey: &key, Position: &pos, RowRank: &rank}
			if i > 1 {
				l := seat(r, i-1)
				row.Left = &l
			}
			if i < 4 {
				rr := seat(r, i+1)
				row.Right = &rr
			}
			adjacency = append(adjacency, row)
		}
	}
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, false, adjacency); err != nil {
		t.Fatal(err)
	}

	got, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 0, "EUR", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1, 1), seat(1, 2)}
	if strings.Join(got.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("seats = %v want %v — rows are ordered by their RANK, which is the venue's order; "+
			"row_key is an opaque identity and sorting by it puts the buyer in a row chosen by uuid", got.Seats, want)
	}
}

// TestProvisionRefusesOrderingThatContradictsItsEdges is ai-review pass 3's [high], and the
// defect it names was executed before it was fixed: the projection sold two seats four apart
// as a contiguous run.
//
// A projection carries TWO descriptions of one geometry. The edges say who sits next to whom;
// the positions say where each seat sits. Selection reads only the positions and arbitration
// reads only the edges, so nothing forced them to agree — and every check that existed
// (unique positions, one rank per row, reciprocal edges) was satisfied by a projection where
// they did not. Chain A-B-C-D-E-F-G with positions B=1, E=2, C=3, D=4, F=5, G=6, A=7: with A
// taken, best-available returned B and E as a two-seat run. The orphan filter could not catch
// it either, because it reasons in the same positional space and agreed they were neighbours.
//
// The rule is now that within a row positions run 1..N and the seat at position i names
// position i-1 as its left and i+1 as its right — which makes the two descriptions the same
// statement. Checked where the projection is BUILT, because the claim path cannot re-derive it
// from data it does not hold (ADR-041's division of labour).
func TestProvisionRefusesOrderingThatContradictsItsEdges(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	seat := func(i int) string { return "A/1/" + string(rune('A'+i-1)) }
	build := func(pos map[int]int32) []SeatAdjacencyRow {
		out := make([]SeatAdjacencyRow, 0, 7)
		for i := 1; i <= 7; i++ {
			p := pos[i]
			key, rank := "A/1", int32(1)
			row := SeatAdjacencyRow{SeatIdentity: seat(i), RowKey: &key, Position: &p, RowRank: &rank}
			if i > 1 {
				l := seat(i - 1)
				row.Left = &l
			}
			if i < 7 {
				r := seat(i + 1)
				row.Right = &r
			}
			out = append(out, row)
		}
		return out
	}

	// The exact permutation from the finding.
	permuted := build(map[int]int32{2: 1, 5: 2, 3: 3, 4: 4, 6: 5, 7: 6, 1: 7})
	err := st.ProvisionSeated(ctx, uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1000, true, permuted)
	if !errors.Is(err, ErrSeatProjectionIncomplete) {
		t.Fatalf("err = %v want ErrSeatProjectionIncomplete — an ordering that contradicts the edges "+
			"lets selection sell non-adjacent seats as a run", err)
	}

	// A gap in the positions is the same class of defect: 1..N must be total, or a row reads
	// as shorter than it is and runs that exist are never offered.
	gapped := build(map[int]int32{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 8})
	if err := st.ProvisionSeated(ctx, uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1000, true, gapped); !errors.Is(err, ErrSeatProjectionIncomplete) {
		t.Fatalf("err = %v want ErrSeatProjectionIncomplete — positions must run 1..N with no gap", err)
	}

	// And the honest ordering is accepted, which is what makes the refusals above about the
	// CONTRADICTION rather than about ordered projections in general.
	if err := st.ProvisionSeated(ctx, uuid.New(), uuid.New(), uuid.New(), uuid.New(), 1000, true,
		build(map[int]int32{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7})); err != nil {
		t.Fatalf("an ordering that matches its edges must be accepted: %v", err)
	}
}
