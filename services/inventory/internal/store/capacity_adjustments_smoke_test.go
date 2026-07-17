//go:build smoke

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-76 / ADR-026: staff capacity adjustment with the clamp floor. Raises apply freely;
// a cut below demand clamps to max(new, confirmed + held) and blocks new claims until
// demand drains to the target — never force-releasing anything (forward-only).

func TestCapacityAdjustmentRaisesCutsAndDrainsToTarget(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	// Demand 60: 20 confirmed + 10 operational + 30 buyer-held.
	confirmedHold, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 20, 0, "", "", "adj-confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, confirmedHold.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, confirmedHold.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 10, "house", "board", "staff", "setup", "adj-op")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 30, 0, "", "", "adj-buyer"); err != nil {
		t.Fatal(err)
	}

	// Raise applies freely.
	adj, replay, err := st.AdjustCapacity(ctx, org, slot, 150, "staff", "extra seats", "adj-raise")
	if err != nil || replay {
		t.Fatalf("raise: %v replay=%v", err, replay)
	}
	if adj.CapacityBefore != 100 || adj.Capacity != 150 || adj.TargetCapacity != nil || adj.Status != "applied" {
		t.Fatalf("raise outcome %+v", adj)
	}

	// Cut below demand clamps to demand and records the target.
	adj, _, err = st.AdjustCapacity(ctx, org, slot, 50, "staff", "stage reconfig", "adj-cut")
	if err != nil {
		t.Fatal(err)
	}
	if adj.CapacityBefore != 150 || adj.Capacity != 60 || adj.TargetCapacity == nil || *adj.TargetCapacity != 50 || adj.Status != "clamped" {
		t.Fatalf("cut outcome %+v", adj)
	}

	// While demand exceeds the target, new claims of every kind reject.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "adj-blocked"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("buyer hold above target: %v", err)
	}
	if _, _, err = st.PlaceOperationalHold(ctx, org, slot, 1, "house", "late", "staff", "late", "adj-blocked-op"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("operational hold above target: %v", err)
	}
	// Conversion is quantity-neutral and stays allowed.
	if _, _, err = st.ConvertOperational(ctx, org, op.ID, uuid.New(), slot, 5, 1000, "EUR", "staff", "guest", "adj-convert"); err != nil {
		t.Fatalf("convert during clamp: %v", err)
	}

	// Availability reflects the clamp without any sweeper: capacity=demand, available=0.
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Capacity != 60 || a.Available != 0 {
		t.Fatalf("clamped availability %+v", a)
	}
	sa, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if sa.Capacity != 60 || sa.TargetCapacity == nil || *sa.TargetCapacity != 50 {
		t.Fatalf("staff availability %+v", sa)
	}

	// Buyer holds expire (30 original + 5 converted): demand drains to 25.
	if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE pool_id=$1 AND claim_kind='buyer' AND status='held'`, slot); err != nil {
		t.Fatal(err)
	}
	// Reads derive effective capacity from live claims — no reconciliation needed first.
	a, err = st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Capacity != 50 || a.Held != 5 || a.Confirmed != 20 || a.Available != 25 {
		t.Fatalf("drained availability %+v", a)
	}

	// Demand is 25 ≤ 50: new holds admit up to the target, not past it.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 26, 0, "", "", "adj-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hold past target: %v", err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 25, 0, "", "", "adj-fill"); err != nil {
		t.Fatalf("hold up to target: %v", err)
	}

	// The write path reconciled: materialized capacity settled at the target, target cleared.
	var capacity int32
	var target *int32
	if err = db.QueryRowContext(ctx, `SELECT capacity,target_capacity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&capacity, &target); err != nil {
		t.Fatal(err)
	}
	if capacity != 50 || target != nil {
		t.Fatalf("pool not reconciled: capacity=%d target=%v", capacity, target)
	}

	// Forward-only: the confirmed admission was never touched.
	var confirmed int32
	if err = db.QueryRowContext(ctx, `SELECT confirmed_quantity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != 20 {
		t.Fatalf("confirmed changed: %d", confirmed)
	}
}

func TestCapacityAdjustmentIdempotencyAndAudit(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	h, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "", "aud-hold")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, h.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, h.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}

	adj, replay, err := st.AdjustCapacity(ctx, org, slot, 5, "alice", "downsize", "aud-cut")
	if err != nil || replay {
		t.Fatalf("cut: %v replay=%v", err, replay)
	}
	if adj.Capacity != 10 || adj.TargetCapacity == nil || *adj.TargetCapacity != 5 || adj.Status != "clamped" {
		t.Fatalf("cut outcome %+v", adj)
	}

	// Exact replay returns the original immutable outcome, even after a later adjustment.
	if _, err = db.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.AdjustCapacity(ctx, org, slot, 200, "bob", "expand", "aud-raise"); err != nil {
		t.Fatal(err)
	}
	rep, replay, err := st.AdjustCapacity(ctx, org, slot, 5, "alice", "downsize", "aud-cut")
	if err != nil || !replay {
		t.Fatalf("replay: %v replay=%v", err, replay)
	}
	if rep.CapacityBefore != 100 || rep.Capacity != 10 || rep.TargetCapacity == nil || *rep.TargetCapacity != 5 || rep.Status != "clamped" {
		t.Fatalf("replay outcome %+v", rep)
	}

	// Same key, different request → ErrIdempotency.
	if _, _, err = st.AdjustCapacity(ctx, org, slot, 7, "alice", "downsize", "aud-cut"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("key reuse: %v", err)
	}

	// Invalid targets reject.
	if _, _, err = st.AdjustCapacity(ctx, org, slot, 0, "alice", "zero", "aud-zero"); err == nil {
		t.Fatal("zero capacity accepted")
	}

	// Audit trail: who/when/from→to, clamp target, append-only.
	entries, err := st.CapacityHistory(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("history entries %d", len(entries))
	}
	first := entries[0]
	if first.Action != "adjust_capacity" || first.Actor != "alice" || first.Reason != "downsize" ||
		first.Quantity != 100 || first.QuantityAfter != 10 || first.StatusAfter != "clamped" ||
		first.TargetCapacity == nil || *first.TargetCapacity != 5 {
		t.Fatalf("audit row %+v", first)
	}
	if entries[1].StatusAfter != "applied" || entries[1].Actor != "bob" || entries[1].TargetCapacity != nil {
		t.Fatalf("audit row %+v", entries[1])
	}
	if _, err = db.ExecContext(ctx, `UPDATE claim_history SET reason='tampered' WHERE pool_id=$1`, slot); err == nil {
		t.Fatal("claim_history UPDATE succeeded")
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM claim_history WHERE pool_id=$1`, slot); err == nil {
		t.Fatal("claim_history DELETE succeeded")
	}

	// Archived pools are terminal and reject adjustment; closed pools accept it.
	if _, err = db.ExecContext(ctx, `UPDATE inventory_pools SET closure_status='closed' WHERE slot_id=$1`, slot); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.AdjustCapacity(ctx, org, slot, 300, "alice", "while closed", "aud-closed"); err != nil {
		t.Fatalf("adjust while closed: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE inventory_pools SET lifecycle_status='archived' WHERE slot_id=$1`, slot); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.AdjustCapacity(ctx, org, slot, 400, "alice", "too late", "aud-archived"); !errors.Is(err, ErrSlotArchived) {
		t.Fatalf("adjust archived: %v", err)
	}
}

// TKT-76 ai-review finding 2: the in-op expiry sweep reconciles a draining cut, so the
// recorded capacity_before must be the settled value, not the pre-sweep clamp.
func TestAdjustmentAfterExpirySettlesBeforeRecording(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "", "settle-hold"); err != nil {
		t.Fatal(err)
	}
	adj, _, err := st.AdjustCapacity(ctx, org, slot, 5, "staff", "cut", "settle-cut")
	if err != nil || adj.Status != "clamped" || adj.Capacity != 10 {
		t.Fatalf("cut: %v %+v", err, adj)
	}
	if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE pool_id=$1 AND status='held'`, slot); err != nil {
		t.Fatal(err)
	}
	// The next adjustment sweeps, settles the cut at its target (5), and must record
	// THAT as capacity_before.
	adj, _, err = st.AdjustCapacity(ctx, org, slot, 200, "staff", "expand", "settle-raise")
	if err != nil {
		t.Fatal(err)
	}
	if adj.CapacityBefore != 5 || adj.Capacity != 200 || adj.Status != "applied" || adj.TargetCapacity != nil {
		t.Fatalf("post-expiry adjustment %+v", adj)
	}
	entries, err := st.CapacityHistory(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if last := entries[len(entries)-1]; last.Quantity != 5 || last.QuantityAfter != 200 {
		t.Fatalf("audit from→to %+v", last)
	}
}
