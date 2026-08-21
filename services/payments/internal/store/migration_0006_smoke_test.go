//go:build smoke

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TKT-257, migration 0006. These tests stand a database up at the PRE-0006 schema, write
// rows in the old shape, then apply 0006 and observe what it did to them.
//
// That distinction is the whole point, and it is why the sibling test in
// refund_legs_smoke_test.go is not sufficient on its own: inserting a NULL-confirmation row
// into an ALREADY-migrated database proves things about the schema 0006 leaves behind, and
// nothing whatsoever about the migration. A migration that rejected, rewrote, or dropped
// existing completed rows would leave exactly the same final schema and pass that test
// (found by the TKT-257 adversarial review).
//
// Each test gets its OWN database: DownTo rolls the schema back, so sharing the suite's
// database would tear the schema out from under every other test in the package.

// migrationDB creates a throwaway database and returns a connection to it. Named from the
// test so a failure leaves an inspectable artifact under a predictable name.
func migrationDB(t *testing.T, name string) (*sql.DB, context.Context) {
	t.Helper()
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	dbName := "payments_migration_" + name
	// Terminate first: a leftover connection from a crashed run blocks DROP.
	if _, err := admin.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(dbName)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(dbName)); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("pgx", replaceDBName(dsn, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		a, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = a.Close() }()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, dbName)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(dbName))
	})
	return db, ctx
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// replaceDBName swaps the database out of a postgres DSN, keeping credentials and options.
func replaceDBName(dsn, name string) string {
	q := ""
	if i := strings.Index(dsn, "?"); i >= 0 {
		dsn, q = dsn[:i], dsn[i:]
	}
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn + q
	}
	return dsn[:i+1] + name + q
}

// seedPre0006 writes a captured operation, a COMPLETED refund leg and a COMPLETED whole
// refund compensation, all in the shape the code produced before 0006 existed. Every one of
// these three rows is load-bearing: each exercises a different table 0006 alters, and
// deleting any of them removes the only evidence that 0006 preserved that table's history.
func seedPre0006(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) {
	t.Helper()
	const key = "pre-0006-charge"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,5000,'EUR','pi_pre','captured',5000,5000,now())`,
		org, key, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed pre-0006 operation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,
		                                provider_idempotency_key,amount,currency,status,provider_ref,fact_id,completed_at)
		VALUES($1,$2,'pre-leg','psp-leg-v1:pre',1250,'EUR','refunded','re_pre',$3,now())`,
		org, key, uuid.New()); err != nil {
		t.Fatalf("seed pre-0006 completed leg: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,
		                                  status,provider_ref,fact_id,amount,currency,completed_at)
		VALUES($1,$2,'void','psp-comp-v1:pre','voided','pi_pre_void',$3,5000,'EUR',now())`,
		org, key+"-void", uuid.New()); err != nil {
		t.Fatalf("seed pre-0006 completed compensation: %v", err)
	}
}

// The migration must not reject, rewrite, or orphan data written under the old schema, and
// the rows it preserves must read as ABSENT rather than having their request-derived figures
// promoted to provider confirmations.
func TestMigration0006PreservesPreMigrationRows(t *testing.T) {
	db, ctx := migrationDB(t, "preserve")
	// Stand the schema up at 0005 — the version before this ticket's migration.
	if err := MigrateUpTo(ctx, db, 5); err != nil {
		t.Fatalf("migrate to 0005: %v", err)
	}
	org := uuid.New()
	seedPre0006(t, db, ctx, org)

	// The migration under test, applied to a database that already holds old-shaped money.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("0006 must apply to a database holding pre-0006 completed rows: %v", err)
	}

	j := New(db, fullRing(t))
	const key = "pre-0006-charge"

	op, found, err := j.LookupOperation(ctx, org, key)
	if err != nil || !found {
		t.Fatalf("the pre-0006 operation must survive: err=%v found=%t", err, found)
	}
	if op.CapturedAmount != 5000 {
		t.Fatalf("captured_amount rewritten by the migration: %d, want 5000 untouched", op.CapturedAmount)
	}
	if op.ConfirmedCapturedAmount != nil {
		t.Fatalf("a pre-0006 operation has no provider confirmation and must not be back-filled, got %d", *op.ConfirmedCapturedAmount)
	}

	leg, found, err := j.LookupRefundLeg(ctx, org, key, "pre-leg")
	if err != nil || !found {
		t.Fatalf("the pre-0006 completed leg must survive: err=%v found=%t", err, found)
	}
	if !leg.Completed || leg.Amount != 1250 {
		t.Fatalf("a completed legacy leg must keep reading as completed for its bound amount: %+v", leg)
	}
	if leg.ConfirmedAmount != nil {
		t.Fatalf("a legacy leg must not be back-filled, got %d", *leg.ConfirmedAmount)
	}

	comp, found, err := j.LookupCompensation(ctx, org, key+"-void", "void")
	if err != nil || !found {
		t.Fatalf("the pre-0006 completed compensation must survive: err=%v found=%t", err, found)
	}
	if !comp.Completed || comp.ConfirmedAmount != nil {
		t.Fatalf("a legacy compensation must stay completed and unconfirmed: %+v", comp)
	}

	// And it is still REFUNDABLE. This is the failure the additive design exists to avoid:
	// repointing CapturedAmount at the new NULL-for-legacy column would leave every
	// assertion above passing while making legacy money impossible to return, because
	// BindRefundLeg refuses unless captured evidence is above zero.
	if _, err := j.BindRefundLeg(ctx, org, key, "post-migration-leg", 1000, "EUR"); err != nil {
		t.Fatalf("a pre-0006 captured operation must stay refundable after 0006: %v", err)
	}
}

// The down migration is a guard, and a guard is only real if it refuses. Both directions:
// it succeeds when no confirmation exists, and refuses rather than silently destroying
// evidence of money that left the account when any does.
func TestMigration0006DownRefusesToDestroyConfirmedEvidence(t *testing.T) {
	db, ctx := migrationDB(t, "down")
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org := uuid.New()
	const key = "down-charge"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,5000,'EUR','pi_down','captured',5000,5000,now())`,
		org, key, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}

	// No confirmation anywhere yet: the rollback is permitted.
	if err := MigrateDownTo(ctx, db, 5); err != nil {
		t.Fatalf("0006 must roll back cleanly when no confirmed evidence exists: %v", err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name='payment_operations' AND column_name='confirmed_captured_amount')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the down migration did not drop confirmed_captured_amount")
	}

	// Re-apply, then record a confirmation — now the guard must fire.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-apply 0006: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE payment_operations SET confirmed_captured_amount=5000,confirmed_currency='EUR' WHERE organizer_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	err := MigrateDownTo(ctx, db, 5)
	if err == nil {
		t.Fatal("rolling back over confirmed provider evidence must be REFUSED; it silently destroys the record of money that left the account")
	}
	if !strings.Contains(err.Error(), "cannot roll back 0006") {
		t.Fatalf("the refusal must name the migration that refused, got %v", err)
	}
	// The evidence is still there — refused, not partially destroyed.
	var confirmed sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT confirmed_captured_amount FROM payment_operations WHERE organizer_id=$1`, org).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if !confirmed.Valid || confirmed.Int64 != 5000 {
		t.Fatalf("a refused rollback must leave the evidence intact, got %v", confirmed)
	}
}
