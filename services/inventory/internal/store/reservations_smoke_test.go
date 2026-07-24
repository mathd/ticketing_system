//go:build smoke

package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// dbClockOffset returns the database clock plus the given interval, so cutoffs are never
// decided by host/DB clock skew.
func dbClockOffset(t *testing.T, db *sql.DB, interval string) time.Time {
	t.Helper()
	var ts time.Time
	if err := db.QueryRow(`SELECT clock_timestamp() + $1::interval`, interval).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestGroupReservationPlacementExpiryAndAudit(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	exp := dbClockOffset(t, db, "1 hour")

	res, replay, err := st.PlaceGroupReservation(ctx, org, slot, 6, "Acme Travel", exp, "", "staff:amy", "contract allotment", "k-res")
	if err != nil || replay {
		t.Fatalf("place: %v replay=%v", err, replay)
	}
	if res.Status != "held" || res.Quantity != 6 || res.Counterparty != "Acme Travel" || res.ExpiresAt == nil {
		t.Fatalf("place returned %+v", res)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Held != 6 || a.Available != 4 {
		t.Fatalf("availability = %+v, want held=6 available=4", a)
	}
	staff, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if staff.ReservationHeld != 6 || staff.BuyerHeld != 0 || staff.OperationalHeld != 0 || staff.Available != 4 {
		t.Fatalf("staff availability = %+v, want reservation_held=6 available=4", staff)
	}
	// Explicit expiry must be in the future by DB decision time.
	if _, _, err = st.PlaceGroupReservation(ctx, org, slot, 1, "Acme Travel", dbClockOffset(t, db, "-1 second"), "", "staff:amy", "late", "k-past"); !errors.Is(err, ErrConflict) {
		t.Fatalf("past-expiry place = %v, want ErrConflict", err)
	}
	// Capacity invariant includes reservations.
	if _, _, err = st.PlaceGroupReservation(ctx, org, slot, 5, "Big Group", dbClockOffset(t, db, "1 hour"), "", "staff:amy", "overbook", "k-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("overplace = %v, want ErrUnavailable", err)
	}
	// Exact replay returns the original outcome; a changed shape conflicts.
	rep, replay, err := st.PlaceGroupReservation(ctx, org, slot, 6, "Acme Travel", exp, "", "staff:amy", "contract allotment", "k-res")
	if err != nil || !replay || rep.ID != res.ID {
		t.Fatalf("place replay = %+v replay=%v err=%v", rep, replay, err)
	}
	if _, _, err = st.PlaceGroupReservation(ctx, org, slot, 7, "Acme Travel", exp, "", "staff:amy", "contract allotment", "k-res"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("changed-fingerprint place = %v, want ErrIdempotency", err)
	}
	// Lazy give-back: past the expiry, the unconverted quantity is publicly claimable
	// again with no sweeper — the next mutation's sweep settles it (ADR-010).
	if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, res.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "", "buyer-full"); err != nil {
		t.Fatalf("public hold after reservation expiry: %v", err)
	}
	hist, err := st.History(ctx, org, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].Action != "reserve" || hist[1].Action != "expire" || hist[1].Actor != "system" {
		t.Fatalf("history = %+v, want reserve then system expire", hist)
	}
}

func TestGroupReservationRepeatedDrawDownIsQuantityNeutral(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 200)
	tt := uuid.New()
	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 200, "Acme Travel", dbClockOffset(t, db, "30 days"), "", "staff:amy", "contract", "k-res")
	if err != nil {
		t.Fatal(err)
	}
	d1, replay, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 10, 2500, "EUR", "staff:amy", "first batch", "k-d1")
	if err != nil || replay {
		t.Fatalf("draw 10: %v replay=%v", err, replay)
	}
	if d1.SourceRemaining != 190 || d1.SourceStatus != "held" || d1.Child.Quantity != 10 || d1.Child.Status != "held" || d1.Child.ExpiresAt == nil {
		t.Fatalf("draw 10 = %+v", d1)
	}
	d2, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 50, 2500, "EUR", "staff:amy", "second batch", "k-d2")
	if err != nil || d2.SourceRemaining != 140 {
		t.Fatalf("draw 50 = %+v err=%v", d2, err)
	}
	// Quantity-neutral: 140 reserved + 60 buyer-held = 200, nothing publicly claimable.
	staff, _ := st.StaffAvailability(ctx, org, slot)
	if staff.ReservationHeld != 140 || staff.BuyerHeld != 60 || staff.Available != 0 {
		t.Fatalf("staff availability = %+v, want reservation=140 buyer=60 available=0", staff)
	}
	// Children are normal buyer claims: the checkout lifecycle accepts them; the source
	// must never be transitionable through checkout.
	if _, err = st.Transition(ctx, org, d1.Child.ID, "finalizing"); err != nil {
		t.Fatalf("finalize child: %v", err)
	}
	if _, err = st.Transition(ctx, org, d1.Child.ID, "confirmed"); err != nil {
		t.Fatalf("confirm child: %v", err)
	}
	if _, err = st.Transition(ctx, org, res.ID, "released"); !errors.Is(err, ErrConflict) {
		t.Fatalf("checkout transition on reservation = %v, want ErrConflict", err)
	}
	// Wrong organizer / non-reservation claim / wrong slot / overdraw all leave the
	// source untouched.
	if _, _, err = st.DrawDownGroupReservation(ctx, uuid.New(), res.ID, tt, slot, 1, 2500, "EUR", "staff:amy", "x", "k-x1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong organizer = %v, want ErrNotFound", err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, d2.Child.ID, tt, slot, 1, 2500, "EUR", "staff:amy", "x", "k-x2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("buyer claim as source = %v, want ErrNotFound", err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, uuid.New(), 1, 2500, "EUR", "staff:amy", "x", "k-x3"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched slot = %v, want ErrConflict", err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 141, 2500, "EUR", "staff:amy", "x", "k-x4"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overdraw = %v, want ErrConflict", err)
	}
	var qty int32
	if err = db.QueryRowContext(ctx, `SELECT quantity FROM claims WHERE id=$1`, res.ID).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	if qty != 140 {
		t.Fatalf("source quantity after rejected draws = %d, want 140", qty)
	}
	// Replay returns the original outcome; a changed shape conflicts.
	r1, replay, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 10, 2500, "EUR", "staff:amy", "first batch", "k-d1")
	if err != nil || !replay || r1.Child.ID != d1.Child.ID || r1.SourceRemaining != 190 {
		t.Fatalf("draw replay = %+v replay=%v err=%v", r1, replay, err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 11, 2500, "EUR", "staff:amy", "first batch", "k-d1"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("changed-fingerprint draw = %v, want ErrIdempotency", err)
	}
	// A whole draw turns the source terminal, keeping its original quantity on record.
	d3, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 140, 2500, "EUR", "staff:amy", "final batch", "k-d3")
	if err != nil || d3.SourceRemaining != 0 || d3.SourceStatus != "released" {
		t.Fatalf("whole draw = %+v err=%v", d3, err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 1, 2500, "EUR", "staff:amy", "x", "k-x5"); !errors.Is(err, ErrConflict) {
		t.Fatalf("draw after terminal = %v, want ErrConflict", err)
	}
	// History links every draw to its child.
	hist, err := st.History(ctx, org, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reserve", "draw_down", "draw_down", "draw_down"}
	if len(hist) != len(want) {
		t.Fatalf("history = %+v, want actions %v", hist, want)
	}
	for i, w := range want {
		if hist[i].Action != w {
			t.Fatalf("history[%d].Action = %s, want %s", i, hist[i].Action, w)
		}
	}
	if hist[1].RelatedClaimID == nil || *hist[1].RelatedClaimID != d1.Child.ID {
		t.Fatalf("draw history should link the child, got %+v", hist[1])
	}
}

func TestGroupReservationChannelAccounting(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	tt := uuid.New()

	// A channel reservation needs an active allocation with headroom and consumes it.
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "agency", Cap: 6}})
	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 5, "Acme Travel", dbClockOffset(t, db, "1 hour"), "agency", "staff:amy", "contract", "k-res")
	if err != nil || res.Channel != "agency" {
		t.Fatalf("channel place: %+v err=%v", res, err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "agency", "ch-2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("channel hold past cap = %v, want ErrUnavailable", err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "agency", "ch-1"); err != nil {
		t.Fatalf("channel hold within cap: %v", err)
	}
	// Draw-down children inherit the source channel; channel consumption is unchanged.
	d, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 2, 2500, "EUR", "staff:amy", "batch", "k-d")
	if err != nil {
		t.Fatal(err)
	}
	var childChannel string
	if err = db.QueryRowContext(ctx, `SELECT channel_code FROM claims WHERE id=$1`, d.Child.ID).Scan(&childChannel); err != nil {
		t.Fatal(err)
	}
	if childChannel != "agency" {
		t.Fatalf("child channel = %q, want agency", childChannel)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "agency", "ch-3"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("channel still full after draw = %v, want ErrUnavailable", err)
	}
	// Past release_at the allocation stops admitting new holds, but draw-down of already
	// consumed quantity proceeds (ADR-024: existing claims finish their lifecycle).
	if _, err = db.ExecContext(ctx, `UPDATE channel_allocations SET release_at=clock_timestamp()-interval '1 second' WHERE pool_id=$1`, slot); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "agency", "ch-4"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hold on released channel = %v, want ErrUnavailable", err)
	}
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 1, 2500, "EUR", "staff:amy", "post-release", "k-d2"); err != nil {
		t.Fatalf("draw-down after allocation release: %v", err)
	}

	// An unchanneled reservation may not eat capacity reserved for active allocations.
	org2, slot2 := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org2, slot2, []ChannelAllocation{{Channel: "agency", Cap: 6}})
	if _, _, err = st.PlaceGroupReservation(ctx, org2, slot2, 5, "Acme Travel", dbClockOffset(t, db, "1 hour"), "", "staff:amy", "contract", "k-pub"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unchanneled place into reserved capacity = %v, want ErrUnavailable", err)
	}
	if _, _, err = st.PlaceGroupReservation(ctx, org2, slot2, 4, "Acme Travel", dbClockOffset(t, db, "1 hour"), "", "staff:amy", "contract", "k-pub2"); err != nil {
		t.Fatalf("unchanneled place within public share: %v", err)
	}
}

func TestGroupReservationChildSurvivesSourceExpiry(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	tt := uuid.New()
	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 10, "Acme Travel", dbClockOffset(t, db, "1 hour"), "", "staff:amy", "contract", "k-res")
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 3, 2500, "EUR", "staff:amy", "batch", "k-d")
	if err != nil {
		t.Fatal(err)
	}
	// The source expires with 7 unconverted.
	if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, res.ID); err != nil {
		t.Fatal(err)
	}
	// The child's lifecycle is independent of the dead source.
	if _, err = st.Transition(ctx, org, d.Child.ID, "finalizing"); err != nil {
		t.Fatalf("finalize child after source expiry: %v", err)
	}
	if _, err = st.Transition(ctx, org, d.Child.ID, "confirmed"); err != nil {
		t.Fatalf("confirm child after source expiry: %v", err)
	}
	// A new draw on the expired source rejects — and settles the lazy expiry.
	if _, _, err = st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 1, 2500, "EUR", "staff:amy", "late", "k-late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("draw on expired source = %v, want ErrConflict", err)
	}
	var status string
	if err = db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, res.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Fatalf("source status = %s, want expired (lazily settled by the rejected draw)", status)
	}
	// The unconverted 7 are public again; the confirmed 3 are not.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 7, 0, "", "", "buyer-back"); err != nil {
		t.Fatalf("public hold after give-back: %v", err)
	}
	// Replaying the original draw still returns its immutable outcome.
	r, replay, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 3, 2500, "EUR", "staff:amy", "batch", "k-d")
	if err != nil || !replay || r.Child.ID != d.Child.ID {
		t.Fatalf("draw replay after source expiry = %+v replay=%v err=%v", r, replay, err)
	}
}

// A draw-down that begins before the source's expiry but queues on the pool lock across
// the cutoff must reject: the expiry verdict is decided at decision time
// (clock_timestamp), not transaction start — the TKT-78 lock-queue rule, scoped to this
// path only (liveClaims deliberately stays on now(), ADR-024/ADR-027).
func TestDrawDownQueuedAcrossSourceExpiryRejects(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	tt := uuid.New()
	exp := dbClockOffset(t, db, "3 seconds")
	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 10, "Acme Travel", exp, "", "staff:amy", "contract", "k-res")
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err = blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, tt, slot, 2, 2500, "EUR", "staff:amy", "queued", "k-q")
		done <- err
	}()
	// Handshake: the draw-down's own pool-lock statement — not any waiter — observed
	// queued while DB time is still before the expiry. Fails the setup, not the
	// assertion, if it never queues (the vacuity trap from TKT-78 review pass 2).
	for {
		var waiting, before bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type='Lock' AND state='active'
				  AND query LIKE '%grp-draw pool lock%' AND pid <> pg_backend_pid()
			), clock_timestamp() < $1`, exp).Scan(&waiting, &before); err != nil {
			t.Fatal(err)
		}
		if waiting {
			if !before {
				t.Fatal("lock waiter observed only after the expiry; widen the margin")
			}
			break
		}
		if !before {
			t.Fatal("draw-down never blocked on the pool lock before the expiry")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Cross the cutoff by DB time while the waiter stays queued, then let it through.
	for {
		var past bool
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() > $1`, exp).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("draw-down completed while the pool lock was held: %v", err)
	default:
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrConflict) {
		t.Fatalf("queued draw-down crossed the source expiry: got %v want ErrConflict", err)
	}
	// The rejected draw settled the expiry: everything is public again.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "", "buyer-back"); err != nil {
		t.Fatalf("public hold after settled expiry: %v", err)
	}
}

// A release against an EXPIRED claim is vacuously satisfied: expiry already freed the
// seats, so the obligation the release discharges is gone either way. Before TKT-115
// this answered ErrConflict — indistinguishable from `confirmed` (a genuinely sold
// seat) — so commerce recovery parked refunded orders whose holds had merely expired
// as "confirmed claim; manual reconciliation". Confirm stays a conflict: an expired
// claim can never buy a seat.
func TestReleaseOfExpiredClaimIsVacuouslySatisfied(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	tt := uuid.New()
	claim, _, err := st.CreateHold(ctx, org, slot, tt, 2, 1250, "EUR", "", "k-expired-release")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.Transition(ctx, org, claim.ID, "released")
	if err != nil {
		t.Fatalf("release of an expired claim must be vacuously satisfied, got %v", err)
	}
	if got.Status != "expired" {
		t.Fatalf("status = %q, want expired (the release changes nothing)", got.Status)
	}
	if _, err := st.Transition(ctx, org, claim.ID, "confirmed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("confirm of an expired claim must stay a conflict, got %v", err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Available != 10 {
		t.Fatalf("available = %d, want 10 (expiry freed the seats; release must not double-free)", a.Available)
	}
}
