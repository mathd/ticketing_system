//go:build smoke

package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// schemaDB gives each test its own schema on the migration-test database, mirroring the
// access/catalog migration-upgrade harnesses: upgrades are proven against data seeded at
// the OLD version, which the always-fully-migrated store-test database cannot express.
func schemaDB(t *testing.T, ctx context.Context) (*sql.DB, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_MIGRATION_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "commerce_migration_" + uuid.NewString()[:8]
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	return db, provider
}

// seedV4Order inserts a reservation + order pair against the version-4 schema.
func seedV4Order(t *testing.T, ctx context.Context, db *sql.DB, status string) uuid.UUID {
	t.Helper()
	resID, orderID := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,1,1000,1000,'EUR','finalizing')`,
		resID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,$3,$4,'fp')`,
		orderID, resID, status, "mig-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// Migration 0005's backfill re-opens EXACTLY the two populations that were parked
// awaiting the capabilities TKT-115 ships — and nothing else. Rows parked for attempt
// exhaustion or manual reconciliation have different semantics and must stay parked.
func TestMigration0005BackfillReopensOnlyTheTKT56Populations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("apply migrations through 0004: %v", err)
	}

	park := func(orderID uuid.UUID, reason string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			UPDATE orders SET status='reconciliation_required',recovery_parked_at=now(),
			    recovery_last_error=$2,recovery_attempts=7,
			    updated_at=now()-interval '10 minutes' WHERE id=$1`, orderID, reason); err != nil {
			t.Fatal(err)
		}
	}
	needsStatus := seedV4Order(t, ctx, db, "created")
	park(needsStatus, "payment result unknown; needs PSP status (TKT-56)")
	needsRefund := seedV4Order(t, ctx, db, "created")
	park(needsRefund, "captured payment whose claim is gone; needs void/refund (TKT-56)")
	exhausted := seedV4Order(t, ctx, db, "created")
	park(exhausted, "claim is confirmed but payment did not capture; needs manual reconciliation")
	unparked := seedV4Order(t, ctx, db, "created")

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply migration 0005: %v", err)
	}

	assertRow := func(orderID uuid.UUID, wantParked, wantErrRetained bool, wantAttempts int) {
		t.Helper()
		var parked *time.Time
		var attempts int
		var lastErr *string
		if err := db.QueryRowContext(ctx, `SELECT recovery_parked_at,recovery_attempts,recovery_last_error FROM orders WHERE id=$1`, orderID).Scan(&parked, &attempts, &lastErr); err != nil {
			t.Fatal(err)
		}
		if (parked != nil) != wantParked {
			t.Fatalf("order %s parked=%v, want parked=%v", orderID, parked != nil, wantParked)
		}
		if attempts != wantAttempts {
			t.Fatalf("order %s attempts=%d, want %d", orderID, attempts, wantAttempts)
		}
		if wantErrRetained && lastErr == nil {
			t.Fatalf("order %s: the backfill must retain recovery_last_error as operator context", orderID)
		}
	}
	assertRow(needsStatus, false, true, 0)
	assertRow(needsRefund, false, true, 0)
	assertRow(exhausted, true, true, 7)
	assertRow(unparked, false, false, 0)

	// The re-opened rows are immediately claimable through the recreated partial index.
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]bool{}
	for _, c := range claimed {
		found[c.OrderID] = true
	}
	if !found[needsStatus] || !found[needsRefund] {
		t.Fatalf("re-opened TKT-56 rows must be claimable: needsStatus=%v needsRefund=%v", found[needsStatus], found[needsRefund])
	}
	if found[exhausted] {
		t.Fatal("a row parked for manual reconciliation must not be re-opened by the backfill")
	}
}

// 0005 widens the vocabulary CHECKs: no_side_effect joins terminal_outcome and refunded
// joins orders.status. Rejected values stay rejected.
func TestMigration0005VocabularyChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	orderID := seedV4Order(t, ctx, db, "created")
	if _, err := db.ExecContext(ctx, `UPDATE orders SET terminal_outcome='no_side_effect' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("no_side_effect must be an accepted terminal_outcome after 0005: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='refunded' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("refunded must be an accepted order status after 0005: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET terminal_outcome='captured' WHERE id=$1`, orderID); err == nil {
		t.Fatal("terminal_outcome must still reject values that do not prove absence of a side effect")
	}
}
