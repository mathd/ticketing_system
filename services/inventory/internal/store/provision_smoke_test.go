//go:build smoke

package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestGroupedDaysConvergeOnOneInventoryPool(t *testing.T) {
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INVENTORY_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	schema := "inventory_festival_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") }()
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := New(db, 10*time.Minute)
	organizerID, festivalID := uuid.New(), uuid.New()
	for range 2 {
		if err = st.Provision(ctx, uuid.New(), festivalID, organizerID, 1000); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var capacity int32
	if err = db.QueryRowContext(ctx, `SELECT count(*),max(capacity) FROM inventory_pools WHERE slot_id=$1`, festivalID).Scan(&count, &capacity); err != nil {
		t.Fatal(err)
	}
	if count != 1 || capacity != 1000 {
		t.Fatalf("festival pools=%d capacity=%d, want one shared pool of 1000", count, capacity)
	}

	// TKT-76 AC3: the group is the pool — one adjustment moves the shared capacity.
	if _, _, err = st.AdjustCapacity(ctx, organizerID, festivalID, 800, "staff", "festival resize", "fest-adjust"); err != nil {
		t.Fatal(err)
	}
	a, err := st.Availability(ctx, organizerID, festivalID, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Capacity != 800 || a.Available != 800 {
		t.Fatalf("group adjustment not reflected: %+v", a)
	}
}

// TKT-76 AC4: catalog's published capacity is the initial snapshot only — a later
// publication event must not overwrite an adjusted pool once it has claims.
func TestProvisionDoesNotOverwriteAdjustedPoolWithClaims(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	if _, _, err := st.AdjustCapacity(ctx, org, slot, 80, "staff", "resize", "prov-adjust"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 90, 0, "", "", "prov-hold"); err == nil {
		t.Fatal("hold beyond adjusted capacity accepted")
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "", "prov-claim"); err != nil {
		t.Fatal(err)
	}
	if err := st.Provision(ctx, uuid.New(), slot, org, 500); err != nil {
		t.Fatal(err)
	}
	var capacity int32
	var target *int32
	if err := db.QueryRowContext(ctx, `SELECT capacity,target_capacity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&capacity, &target); err != nil {
		t.Fatal(err)
	}
	if capacity != 80 || target != nil {
		t.Fatalf("republish overwrote adjusted pool: capacity=%d target=%v", capacity, target)
	}
}

// TKT-76 ai-review finding 1: an APPLIED adjustment on a claim-free pool looks exactly
// like an untouched pool to the old guard — a later publication event must not restore
// catalog's snapshot over it. Inventory owns capacity after any adjustment.
func TestProvisionDoesNotOverwriteAdjustedEmptyPool(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	if _, _, err := st.AdjustCapacity(ctx, org, slot, 80, "staff", "resize", "empty-adjust"); err != nil {
		t.Fatal(err)
	}
	if err := st.Provision(ctx, uuid.New(), slot, org, 500); err != nil {
		t.Fatal(err)
	}
	var capacity int32
	if err := db.QueryRowContext(ctx, `SELECT capacity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&capacity); err != nil {
		t.Fatal(err)
	}
	if capacity != 80 {
		t.Fatalf("republish overwrote adjusted empty pool: capacity=%d", capacity)
	}
}

// TKT-76 ai-review round 2: an adjustment committed while Provision waits on the pool
// row must survive. A single upsert's WHERE subqueries evaluate against the pre-wait
// snapshot and miss it — Provision must lock first, then decide in a fresh statement.
// Deterministic: a manual transaction holds the adjustment uncommitted, a pg_stat_activity
// handshake proves Provision queued behind it, then the adjustment commits first.
func TestProvisionQueuedBehindAdjustmentDoesNotOverwrite(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	// The adjustment transaction takes ONLY the pool row lock first. Against a merely
	// locked (unmodified) committed tuple, Provision's INSERT ... ON CONFLICT DO NOTHING
	// resolves without waiting, so the statement that queues is exactly the one under
	// test: the lock-before-decide SELECT ... FOR UPDATE. (With the row already updated
	// uncommitted, the INSERT absorbs the wait instead and the handshake proves nothing —
	// ai-review round 3.) The adjustment's writes land only after the handshake, so the
	// guarded UPDATE's snapshot must be taken after the lock wait to see them.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- st.Provision(ctx, uuid.New(), slot, org, 500) }()
	// Handshake: Provision's SELECT ... FOR UPDATE — that exact statement, not any
	// inventory_pools waiter — observed queued while the adjustment is still
	// uncommitted. If the lock-before-decide statement is ever removed, no waiter
	// matches and this handshake times out: the test fails rather than passing
	// vacuously (ai-review round 3).
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type='Lock' AND state='active'
				  AND query LIKE '%FROM inventory_pools WHERE slot_id=$1 FOR UPDATE%' AND pid <> pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Provision never queued on the pool lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Provision is queued: commit the adjustment it must not overwrite.
	if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET capacity=80, updated_at=now() WHERE slot_id=$1`, slot); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO claim_history(id,organizer_id,pool_id,action,actor,reason,quantity,quantity_after,status_after,idempotency_key,request_fingerprint)
			VALUES($1,$2,$3,'adjust_capacity','staff','resize',100,80,'applied','race-adjust','fp')`, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var capacity int32
	if err := db.QueryRowContext(ctx, `SELECT capacity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&capacity); err != nil {
		t.Fatal(err)
	}
	if capacity != 80 {
		t.Fatalf("queued Provision overwrote the committed adjustment: capacity=%d", capacity)
	}
}
