//go:build smoke

// TKT-96 end-to-end: the catalog `reemit-policies` one-shot re-emits published
// slots' re_entry policy under a distinct deterministic envelope id, so access
// re-projects the real policy for slots published before the field existed —
// while inventory's pool provisioning stays a strict no-op.
//
// PM-1 (plan-review): the genuine pre-ride-along state (a publication envelope
// with re_entry ABSENT) is no longer reachable through the live API — catalog's
// reEntryData always populates the field. So the bug is SIMULATED faithfully:
// publish a multi slot (access projects multi), then force access's projected
// row back to single, exactly as the historical field-absent replay left it.
// The re-emission must re-converge it. This is the whole ticket: emission was
// fixed, already-emitted history was not.
package smoke_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func reemitSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// publishMultiSlot creates and publishes an ungrouped `multi` re_entry slot via
// the public contract and returns its performance id.
func publishMultiSlot(t *testing.T) string {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	suffix := reemitSuffix()
	venue := created(t, catalog+"/venues", map[string]any{
		"name": "Reemit Arena " + suffix, "ga_capacity": 500,
	})
	event := created(t, catalog+"/events", map[string]any{
		"name":         map[string]string{"fr": "Passe " + suffix, "en": "Pass " + suffix},
		"description":  map[string]string{"fr": "Passe multi-entrée.", "en": "Multi-entry pass."},
	})
	perf := created(t, catalog+"/performances", map[string]any{
		"event_id": event["id"], "venue_id": venue["id"],
		"kind": "operating_day", "operating_date": "2026-09-18", "opens_at": "10:00", "closes_at": "22:00",
		"timezone": "Europe/Paris",
		"re_entry": map[string]any{"mode": "multi", "requires_exit": true},
	})
	created(t, catalog+"/ticket-types", map[string]any{
		"performance_id": perf["id"],
		"name":  map[string]string{"fr": "Passe", "en": "Pass"},
		"price": map[string]any{"amount": 9000, "currency": "EUR"},
	})
	publishURL := fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"])
	if code, body := postJSON(t, publishURL, nil); code != http.StatusOK {
		t.Fatalf("publish: status %d: %s", code, body)
	}
	return fmt.Sprintf("%v", perf["id"])
}

// runReemitPolicies runs the catalog reemit-policies one-shot against the stack
// (ADR-022 one-shot job shape), like the commerce migrate probe.
func runReemitPolicies(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm",
		"--network", project+"_default",
		"-e", "DATABASE_URL="+containerDSN("catalog", "catalog"),
		"-e", "NATS_URL=nats://nats:4222",
		project+"-catalog", "reemit-policies").CombinedOutput()
	if err != nil {
		t.Fatalf("reemit-policies run failed: %v: %s", err, out)
	}
}

func accessConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn("access", "access"))
	if err != nil {
		t.Fatalf("connect access db: %v", err)
	}
	return conn
}

// TestPolicyReemitEnforcesRealPolicyAtGate is COS-1: a multi slot whose access
// projection was left at single (the simulated pre-ride-along gap) has its real
// multi policy restored by the re-emission — proven end-to-end through the live
// stream (catalog emit -> access projection).
func TestPolicyReemitEnforcesRealPolicyAtGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	slotID := publishMultiSlot(t)
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()

	// Access projects the real multi policy from the live publication first.
	retry(t, 30*time.Second, func() error {
		var mode string
		err := conn.QueryRow(ctx, `SELECT mode FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&mode)
		if err != nil {
			return fmt.Errorf("policy row not projected yet: %w", err)
		}
		if mode != "multi" {
			return fmt.Errorf("mode=%s, want multi", mode)
		}
		return nil
	})

	// Simulate the pre-ride-along state: force the projection back to single, as
	// a field-absent historical replay would have left it (PM-1).
	if _, err := conn.Exec(ctx,
		`UPDATE slot_re_entry_policies SET mode='single', requires_exit=false, max_entries=NULL WHERE slot_id=$1`,
		slotID); err != nil {
		t.Fatalf("force single: %v", err)
	}

	runReemitPolicies(t)

	// The re-emission (distinct id) escapes access's consumed_events dedup and
	// re-converges the row to the real policy.
	retry(t, 30*time.Second, func() error {
		var mode string
		var requiresExit bool
		err := conn.QueryRow(ctx,
			`SELECT mode, requires_exit FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&mode, &requiresExit)
		if err != nil {
			return err
		}
		if mode != "multi" || !requiresExit {
			return fmt.Errorf("mode=%s requires_exit=%v, want multi/true after re-emit", mode, requiresExit)
		}
		return nil
	})
}

// TestPolicyReemitRerunIsNoOp is COS-2: re-running the one-shot is idempotent —
// the re-emission carries a stable deterministic id, so the second run's envelope
// is dedup'd by access's consumed_events (ON CONFLICT DO NOTHING) and the policy
// row's updated_at does not advance.
func TestPolicyReemitRerunIsNoOp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	slotID := publishMultiSlot(t)
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()

	// Snapshot the live-publish projection timestamp first.
	var livePublished time.Time
	retry(t, 30*time.Second, func() error {
		return conn.QueryRow(ctx, `SELECT updated_at FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&livePublished)
	})

	// The first re-emission carries a NEW envelope id, so it is NOT dedup'd — it
	// re-runs the upsert and advances updated_at past the live-publish value.
	// Wait for that to prove the re-emission actually reached the projection.
	runReemitPolicies(t)
	var firstUpdated time.Time
	retry(t, 30*time.Second, func() error {
		if err := conn.QueryRow(ctx, `SELECT updated_at FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&firstUpdated); err != nil {
			return err
		}
		if !firstUpdated.After(livePublished) {
			return fmt.Errorf("first re-emission not yet projected (updated_at still %s)", firstUpdated)
		}
		return nil
	})

	// The SECOND run re-emits the SAME deterministic id → access's consumed_events
	// dedup (ON CONFLICT DO NOTHING) swallows it → updated_at must NOT advance.
	runReemitPolicies(t)
	time.Sleep(5 * time.Second)
	var secondUpdated time.Time
	if err := conn.QueryRow(ctx, `SELECT updated_at FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).Scan(&secondUpdated); err != nil {
		t.Fatal(err)
	}
	if !secondUpdated.Equal(firstUpdated) {
		t.Fatalf("policy updated_at advanced on rerun (%s -> %s) — re-emission id is not stable, COS-2 broken",
			firstUpdated, secondUpdated)
	}
}

// TestPolicyReemitDoesNotReprovisionInventory is COS-3: the re-emission is
// consumed by inventory too (it consumes the same subject), but must not
// duplicate-provision — inventory_pools.slot_id is a PK with ON CONFLICT DO
// NOTHING, so a distinct-id re-publication for an already-provisioned slot with
// unchanged capacity leaves exactly one pool row of unchanged capacity. Test
// evidence, not assertion (the COS-3 requirement).
func TestPolicyReemitDoesNotReprovisionInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	slotID := publishMultiSlot(t)
	inv, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatalf("connect inventory db: %v", err)
	}
	defer func() { _ = inv.Close(ctx) }()

	var poolsBefore, capBefore int
	retry(t, 30*time.Second, func() error {
		err := inv.QueryRow(ctx,
			`SELECT count(*), coalesce(max(capacity),0) FROM inventory_pools WHERE slot_id=$1`, slotID).
			Scan(&poolsBefore, &capBefore)
		if err != nil {
			return err
		}
		if poolsBefore != 1 {
			return fmt.Errorf("pool not provisioned yet: count=%d", poolsBefore)
		}
		return nil
	})

	runReemitPolicies(t)
	time.Sleep(5 * time.Second) // allow inventory to consume the re-emission

	var poolsAfter, capAfter int
	if err := inv.QueryRow(ctx,
		`SELECT count(*), coalesce(max(capacity),0) FROM inventory_pools WHERE slot_id=$1`, slotID).
		Scan(&poolsAfter, &capAfter); err != nil {
		t.Fatal(err)
	}
	if poolsAfter != 1 || capAfter != capBefore {
		t.Fatalf("inventory pool changed after re-emit: count %d->%d capacity %d->%d, want one pool, unchanged capacity",
			poolsBefore, poolsAfter, capBefore, capAfter)
	}
}
