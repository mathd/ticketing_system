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
