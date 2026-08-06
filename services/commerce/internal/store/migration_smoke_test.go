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

// TKT-153: the reservation carries the provenance of the price it was created
// with, as a snapshot rather than a reference — so closing or superseding the
// rule later cannot rewrite what a buyer was charged.
func TestPriceResolutionSnapshotColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	res := uuid.New()
	seed := func(snapshot any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
			                         quantity,unit_amount,total_amount,currency,status,
			                         price_resolution_snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,900,900,'EUR','held',$7)
			ON CONFLICT(id) DO UPDATE SET price_resolution_snapshot = EXCLUDED.price_resolution_snapshot`,
			res, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), snapshot)
		return err
	}

	// A legacy row — priced before this migration — stays NULL. Backfilling
	// 'no_eligible_rule' would fabricate a resolution that never happened.
	if err := seed(nil); err != nil {
		t.Fatalf("a NULL snapshot must be accepted (legacy and staff-path rows): %v", err)
	}

	winner := `{"resolver_version":2,"winner":{"rule_id":"` + uuid.New().String() +
		`","scope_level":"event"},"candidates":[]}`
	if err := seed([]byte(winner)); err != nil {
		t.Fatalf("a winner snapshot must be accepted: %v", err)
	}
	var (
		version int32
		ruleID  uuid.UUID
		scope   string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT price_resolver_version, price_rule_id, price_rule_scope_level FROM reservations WHERE id=$1`,
		res).Scan(&version, &ruleID, &scope); err != nil {
		t.Fatalf("the generated trace projections must be readable without a JSON path: %v", err)
	}
	if version != 2 || scope != "event" || ruleID == uuid.Nil {
		t.Errorf("projections = %d/%v/%q, want them derived from the snapshot", version, ruleID, scope)
	}

	fallback := `{"resolver_version":2,"winner":null,"candidates":[],"fallback_reason":"no_eligible_rule"}`
	if err := seed([]byte(fallback)); err != nil {
		t.Fatalf("a fallback snapshot must be accepted: %v", err)
	}

	// The XOR the application enforces before persisting, enforced again here:
	// a document claiming both a winner and a fallback is incoherent, and a
	// snapshot is the one record that must stay interpretable years later.
	both := `{"resolver_version":2,"winner":{"rule_id":"` + uuid.New().String() +
		`","scope_level":"event"},"candidates":[],"fallback_reason":"no_eligible_rule"}`
	if err := seed([]byte(both)); err == nil {
		t.Error("a snapshot with BOTH a winner and a fallback_reason must be rejected")
	}
	neither := `{"resolver_version":2,"winner":null,"candidates":[]}`
	if err := seed([]byte(neither)); err == nil {
		t.Error("a snapshot with NEITHER a winner nor a fallback_reason must be rejected")
	}
	if err := seed([]byte(`[]`)); err == nil {
		t.Error("a non-object snapshot must be rejected")
	}
}

// 0012's Down must refuse to run once a cancellation refund run exists. Silently dropping
// the record of who was repaid on a cancelled event is worse than refusing to roll back —
// it is the only place that record lives per-order.
func TestMigration0012DownRefusesToDestroyCancellationHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	org, run := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cancellation_refund_runs(organizer_id,id,slot_id,idempotency_key,request_fingerprint,actor,reason,cutoff_at)
		VALUES($1,$2,$3,'k','fp','ops@example.test','event cancelled',now())`,
		org, run, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Unwind everything above 0012 first. provider.Down() rolls back exactly ONE
	// migration, so this assertion silently stopped testing 0012 the moment 0013
	// landed on top of it (TKT-173) — it rolled back the new migration, saw that
	// succeed, and reported a missing guard. DownTo pins the target explicitly so the
	// next migration cannot move this test's aim again.
	if _, err := provider.DownTo(ctx, 12); err != nil {
		t.Fatalf("unwind to 0012: %v", err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0012 rolled back over an existing cancellation refund run — the guard is missing")
	}

	// And with the history cleared it rolls back cleanly, so the guard is a guard and not
	// a permanently broken Down.
	if _, err := db.ExecContext(ctx, `DELETE FROM cancellation_refund_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("0012 down on an empty history: %v", err)
	}
}

// TKT-215 / migration 0014. Two columns, and the second one — face_value_amount —
// exists because of a defect found at plan review: once total_amount became the
// GROSS charge, the exchange delta compared it against a price-only target and
// refunded the service fee on an EVEN exchange.
func TestMigration0014FeeCompositionColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	insert := func(id uuid.UUID, total, face int64, snapshot any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
			                         quantity,unit_amount,total_amount,face_value_amount,currency,status,
			                         fee_resolution_snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,900,$7,$8,'EUR','held',$9)
			ON CONFLICT(id) DO UPDATE SET fee_resolution_snapshot = EXCLUDED.fee_resolution_snapshot`,
			id, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), total, face, snapshot)
		return err
	}

	// A fee-free sale: the two numbers agree, and the snapshot is NULL — the same
	// state every pre-migration row is in.
	if err := insert(uuid.New(), 900, 900, nil); err != nil {
		t.Fatalf("a fee-free reservation must be accepted: %v", err)
	}
	// A sale carrying a passed-on fee: gross above face.
	env := `{"resolution":{"resolver_version":1,"fees":[]},"breakdown":[],` +
		`"face_value":900,"passed_on_fees":300,"absorbed_fees":0,"total_amount":1200}`
	if err := insert(uuid.New(), 1200, 900, []byte(env)); err != nil {
		t.Fatalf("a fee-carrying reservation must be accepted: %v", err)
	}

	// face > total is unrepresentable: total = face + passed_on and passed_on is
	// never negative, so a row claiming otherwise is corrupt.
	if err := insert(uuid.New(), 900, 1200, nil); err == nil {
		t.Error("a face value above the total must be rejected")
	}

	// The envelope must AGREE with the columns it explains. A provenance
	// document that contradicts the row is worse than none — it is a record that
	// lies, and TKT-217 settles real money from it.
	for name, bad := range map[string]string{
		"a face value disagreeing with the column": `{"resolution":{},"breakdown":[],"face_value":111,"passed_on_fees":300,"total_amount":1200}`,
		"a total disagreeing with the column":      `{"resolution":{},"breakdown":[],"face_value":900,"passed_on_fees":300,"total_amount":999}`,
		"no resolution document":                   `{"breakdown":[],"face_value":900,"passed_on_fees":300,"total_amount":1200}`,
		"a breakdown that is not an array":         `{"resolution":{},"breakdown":{},"face_value":900,"passed_on_fees":300,"total_amount":1200}`,
		"no totals at all":                         `{"resolution":{},"breakdown":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := insert(uuid.New(), 1200, 900, []byte(bad)); err == nil {
				t.Error("the shape CHECK must reject this envelope")
			}
		})
	}
}

// The backfill is exact rather than approximate, and that is the whole argument
// for it: before 0014 no reservation carries a fee, so face = total is TRUE for
// every existing row and exchange behaviour is UNCHANGED rather than newly
// corrected. This test seeds a row under the old schema and proves it.
func TestMigration0014BackfillsFaceValueFromTheTotal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	// Stop BELOW 0014 so the row is written by the pre-fee schema, exactly as a
	// production row would have been.
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("apply migrations up to 0013: %v", err)
	}
	legacy := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,3,900,2700,'EUR','held')`,
		legacy, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply 0014: %v", err)
	}
	var face, total int64
	if err := db.QueryRowContext(ctx,
		`SELECT face_value_amount, total_amount FROM reservations WHERE id=$1`, legacy).
		Scan(&face, &total); err != nil {
		t.Fatal(err)
	}
	if face != 2700 || total != 2700 {
		t.Errorf("face=%d total=%d, want both 2700 — a pre-fee row's total IS its face value, "+
			"so the backfill is exact and the exchange delta it feeds is unchanged", face, total)
	}
}

// The Down guard, on both halves. The snapshot half is the obvious one; the
// face-value half is the one that matters, because dropping that column once a
// fee has been charged silently re-breaks the exchange delta.
func TestMigration0014DownRefusesToDestroyFeeComposition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	res := uuid.New()
	env := `{"resolution":{"fees":[]},"breakdown":[],"face_value":900,"passed_on_fees":300,` +
		`"absorbed_fees":0,"total_amount":1200}`
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,face_value_amount,currency,status,
		                         fee_resolution_snapshot)
		VALUES($1,$2,$3,$4,$5,$6,1,900,1200,900,'EUR','held',$7)`,
		res, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), []byte(env)); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0014 rolled back over a stored fee snapshot — the guard is missing")
	}

	// Clearing the snapshot is not enough while the face value still differs from
	// the total: that row's exchange delta depends on the column.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET fee_resolution_snapshot = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0014 rolled back over a reservation whose face value differs from its total — " +
			"dropping the column there silently re-breaks the exchange delta")
	}

	// With both cleared it rolls back cleanly, so the guard is a guard rather
	// than a permanently broken Down.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET total_amount = face_value_amount`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("0014 down on fee-free data: %v", err)
	}
}
