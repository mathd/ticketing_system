//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func storeForTest(t *testing.T, ttl time.Duration) (context.Context, *Postgres, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INVENTORY_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "inventory_op_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return ctx, New(db, ttl), db
}

func provisioned(t *testing.T, ctx context.Context, st *Postgres, capacity int32) (uuid.UUID, uuid.UUID) {
	t.Helper()
	org, slot := uuid.New(), uuid.New()
	if err := st.Provision(ctx, uuid.New(), slot, org, capacity); err != nil {
		t.Fatal(err)
	}
	return org, slot
}

func TestOperationalHoldsNeverExpireAndCountAgainstCapacity(t *testing.T) {
	// Negative TTL: every buyer hold is born expired, so any sweep the code runs fires
	// immediately. An operational hold must survive all of them.
	ctx, st, _ := storeForTest(t, -time.Second)
	org, slot := provisioned(t, ctx, st, 10)

	op, replay, err := st.PlaceOperationalHold(ctx, org, slot, 6, "house", "front-of-house", "staff:amy", "opening allotment", "k-place")
	if err != nil || replay {
		t.Fatalf("place: %v replay=%v", err, replay)
	}
	if op.Status != "held" || op.Quantity != 6 {
		t.Fatalf("place returned %+v", op)
	}
	// A buyer hold born expired, then a second buyer hold to trigger the sweep.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "buyer-1"); err != nil {
		t.Fatalf("buyer hold: %v", err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "buyer-2"); err != nil {
		t.Fatalf("buyer hold 2: %v", err)
	}
	a, err := st.Availability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	// Buyer holds all expired (negative TTL); only the operational 6 stays counted.
	if a.Held != 6 || a.Available != 4 {
		t.Fatalf("availability after sweeps = %+v, want held=6 available=4", a)
	}
	staff, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if staff.OperationalHeld != 6 || staff.BuyerHeld != 0 || staff.Available != 4 {
		t.Fatalf("staff availability = %+v", staff)
	}
	// Capacity invariant includes operational quantity.
	if _, _, err = st.PlaceOperationalHold(ctx, org, slot, 5, "kill", "sightline kills", "staff:amy", "production", "k-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("overplace = %v, want ErrUnavailable", err)
	}
}

func TestOperationalPartialAndWholeRelease(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 10, "artist", "band allotment", "staff:amy", "contract", "k-place")
	if err != nil {
		t.Fatal(err)
	}
	rel, replay, err := st.ReleaseOperational(ctx, org, op.ID, 3, "staff:amy", "unused", "k-rel-1")
	if err != nil || replay {
		t.Fatalf("partial release: %v replay=%v", err, replay)
	}
	if rel.Status != "held" || rel.Quantity != 7 {
		t.Fatalf("partial release => %+v, want held/7", rel)
	}
	a, _ := st.Availability(ctx, org, slot)
	if a.Held != 7 || a.Available != 3 {
		t.Fatalf("availability after partial release = %+v", a)
	}
	rel, _, err = st.ReleaseOperational(ctx, org, op.ID, 7, "staff:amy", "show cancelled", "k-rel-2")
	if err != nil || rel.Status != "released" || rel.Quantity != 0 {
		t.Fatalf("whole release => %+v err=%v", rel, err)
	}
	a, _ = st.Availability(ctx, org, slot)
	if a.Held != 0 || a.Available != 10 {
		t.Fatalf("availability after whole release = %+v", a)
	}
	// Terminal: further mutations conflict.
	if _, _, err = st.ReleaseOperational(ctx, org, op.ID, 1, "staff:amy", "again", "k-rel-3"); !errors.Is(err, ErrConflict) {
		t.Fatalf("release after terminal = %v, want ErrConflict", err)
	}
	// Over-quantity conflicts.
	op2, _, _ := st.PlaceOperationalHold(ctx, org, slot, 2, "house", "late holds", "staff:amy", "ops", "k-place-2")
	if _, _, err = st.ReleaseOperational(ctx, org, op2.ID, 3, "staff:amy", "too many", "k-rel-4"); !errors.Is(err, ErrConflict) {
		t.Fatalf("over-quantity release = %v, want ErrConflict", err)
	}
}

func TestOperationalConvertIsQuantityNeutralAndAudited(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	tt := uuid.New()
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 10, "house", "front-of-house", "staff:amy", "opening allotment", "k-place")
	if err != nil {
		t.Fatal(err)
	}
	res, replay, err := st.ConvertOperational(ctx, org, op.ID, tt, slot, 2, 2500, "EUR", "staff:amy", "walk-up sale", "k-conv")
	if err != nil || replay {
		t.Fatalf("convert: %v replay=%v", err, replay)
	}
	if res.SourceRemaining != 8 || res.SourceStatus != "held" {
		t.Fatalf("source after convert = %+v", res)
	}
	c := res.Child
	if c.Status != "held" || c.Quantity != 2 || c.ExpiresAt == nil || c.UnitAmount != 2500 || c.Currency != "EUR" {
		t.Fatalf("child claim = %+v", c)
	}
	// Quantity-neutral: 8 operational + 2 buyer = 10 held, 0 available at every point.
	staff, _ := st.StaffAvailability(ctx, org, slot)
	if staff.OperationalHeld != 8 || staff.BuyerHeld != 2 || staff.Available != 0 {
		t.Fatalf("staff availability after convert = %+v", staff)
	}
	// The child is a normal buyer claim: the checkout lifecycle accepts it.
	if _, err = st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatalf("finalize converted child: %v", err)
	}
	if _, err = st.Transition(ctx, org, c.ID, "confirmed"); err != nil {
		t.Fatalf("confirm converted child: %v", err)
	}
	// The operational source must NOT be transitionable through the checkout path.
	if _, err = st.Transition(ctx, org, op.ID, "released"); !errors.Is(err, ErrConflict) {
		t.Fatalf("checkout transition on operational hold = %v, want ErrConflict", err)
	}
	// Replay returns the original outcome even though the source has since changed.
	if _, _, err = st.ReleaseOperational(ctx, org, op.ID, 8, "staff:amy", "done", "k-rel"); err != nil {
		t.Fatal(err)
	}
	res2, replay, err := st.ConvertOperational(ctx, org, op.ID, tt, slot, 2, 2500, "EUR", "staff:amy", "walk-up sale", "k-conv")
	if err != nil || !replay {
		t.Fatalf("convert replay: %v replay=%v", err, replay)
	}
	if res2.SourceRemaining != 8 || res2.Child.ID != c.ID {
		t.Fatalf("replay outcome = %+v, want original remaining 8 and same child", res2)
	}
	// Same key, different input: conflict.
	if _, _, err = st.ConvertOperational(ctx, org, op.ID, tt, slot, 3, 2500, "EUR", "staff:amy", "walk-up sale", "k-conv"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("changed-fingerprint convert = %v, want ErrIdempotency", err)
	}
	// History is complete and append-only.
	hist, err := st.History(ctx, org, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range hist {
		actions = append(actions, e.Action)
	}
	want := []string{"place", "convert", "release"}
	if len(actions) != len(want) {
		t.Fatalf("history actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("history actions = %v, want %v", actions, want)
		}
	}
	if hist[1].RelatedClaimID == nil || *hist[1].RelatedClaimID != c.ID {
		t.Fatalf("convert history should link the child, got %+v", hist[1])
	}
	if _, err := db.Exec(`UPDATE claim_history SET reason='rewritten'`); err == nil {
		t.Fatal("claim_history accepted an UPDATE; append-only trigger missing")
	}
	if _, err := db.Exec(`DELETE FROM claim_history`); err == nil {
		t.Fatal("claim_history accepted a DELETE; append-only trigger missing")
	}
}

func TestOperationalWrongOrganizerAndBuyerClaimAreNotFound(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 2, "other", "promoter picks", "staff:amy", "ops", "k-place")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ReleaseOperational(ctx, uuid.New(), op.ID, 1, "staff:amy", "wrong org", "k-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong organizer = %v, want ErrNotFound", err)
	}
	buyer, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "buyer-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ReleaseOperational(ctx, org, buyer.ID, 1, "staff:amy", "not operational", "k-y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("buyer claim via operational endpoint = %v, want ErrNotFound", err)
	}
	// Idempotent place replays.
	rep, replay, err := st.PlaceOperationalHold(ctx, org, slot, 2, "other", "promoter picks", "staff:amy", "ops", "k-place")
	if err != nil || !replay || rep.ID != op.ID {
		t.Fatalf("place replay = %+v replay=%v err=%v", rep, replay, err)
	}
	// Same key, different shape: conflict.
	if _, _, err = st.PlaceOperationalHold(ctx, org, slot, 3, "other", "promoter picks", "staff:amy", "ops", "k-place"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("changed-fingerprint place = %v, want ErrIdempotency", err)
	}
}

func TestConvertSlotMismatchLeavesSourceIntact(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 6, "house", "front-of-house", "staff:amy", "ops", "k-place")
	if err != nil {
		t.Fatal(err)
	}
	// The expected slot is the precondition: a ticket type priced against another
	// performance must reject before any write, not after the carve committed.
	if _, _, err = st.ConvertOperational(ctx, org, op.ID, uuid.New(), uuid.New(), 2, 2500, "EUR", "staff:amy", "wrong slot", "k-conv"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched-slot convert = %v, want ErrConflict", err)
	}
	var qty, claims, history int32
	if err = db.QueryRowContext(ctx, `SELECT quantity,(SELECT count(*) FROM claims),(SELECT count(*) FROM claim_history WHERE action='convert') FROM claims WHERE id=$1`, op.ID).Scan(&qty, &claims, &history); err != nil {
		t.Fatal(err)
	}
	if qty != 6 || claims != 1 || history != 0 {
		t.Fatalf("after rejected convert: quantity=%d claims=%d convert-history=%d, want 6/1/0", qty, claims, history)
	}
}

func TestCapacityMathDoesNotWrapAtInt32Boundary(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, math.MaxInt32)
	// Nearly fill the pool with one operational hold (no API cap at store level).
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, math.MaxInt32-2, "kill", "capacity boundary", "staff:amy", "ops", "k-big"); err != nil {
		t.Fatal(err)
	}
	// 32-bit math would wrap (MaxInt32-2 + 5) negative and admit this oversell.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 5, 0, "", "buyer-overflow"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("boundary hold = %v, want ErrUnavailable", err)
	}
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 5, "house", "boundary", "staff:amy", "ops", "k-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("boundary operational place = %v, want ErrUnavailable", err)
	}
}
