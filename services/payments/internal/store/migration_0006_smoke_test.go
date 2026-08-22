//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
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

// migrationDB opens one of the DEDICATED databases scripts/smoke.sh provisions for these
// tests, and resets it so the test can migrate from any version.
//
// Provisioned by the smoke script as superuser rather than created here, following the
// precedent settlement_legacy_smoke_test.go set for exactly this need (a database migrated
// only part-way). The first version of this file created its own databases from the test and
// passed every local check; in CI the `payments` role has no CREATEDB and all three tests
// failed with "permission denied to create database". Creating them in the script also makes
// the isolation visible to whoever reads the suite, instead of hiding it in a helper.
//
// Dedicated, because these tests roll the schema BACK. Sharing the store suite's database
// would tear 0006 out from under every other test in this package while they run.
func migrationDB(t *testing.T, env string) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s is not set", env)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	// Start from nothing regardless of what a previous run left behind — including a
	// half-rolled-back schema from a failed down test.
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset %s: %v", env, err)
	}
	return db, ctx
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
	db, ctx := migrationDB(t, "PAYMENTS_MIGRATION_TEST_DATABASE_URL")
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
	db, ctx := migrationDB(t, "PAYMENTS_MIGRATION_DOWN_TEST_DATABASE_URL")
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

// The refund-confirmation invariant is enforced in CompleteCompensation itself, not only in
// the HTTP handler that calls it. This method is exported and is the persistence boundary
// for the rule, so a direct caller or a future recovery path could otherwise mark a
// compensation `refunded` with no provider evidence — the exact state this ticket makes
// unreachable. A guard only one caller happens to satisfy is a convention, not an invariant.
//
// Each case is refused for its OWN reason and each is asserted separately: a single
// "everything empty" case would be satisfied by a guard that checked only the amount.
func TestCompleteCompensationEnforcesTheConfirmationRule(t *testing.T) {
	db, ctx := migrationDB(t, "PAYMENTS_COMPENSATION_TEST_DATABASE_URL")
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org := uuid.New()
	const key = "comp-invariant"
	bind := func(t *testing.T, kind, sourceKey string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,amount,currency)
			VALUES($1,$2,$3,'psp-comp-v1:'||$2,2500,'EUR')`, org, sourceKey, kind); err != nil {
			t.Fatal(err)
		}
	}
	j := New(db, fullRing(t))

	bind(t, "refund", key)
	for _, c := range []struct {
		name      string
		confirmed ConfirmedRefund
	}{
		{"no confirmation at all", ConfirmedRefund{}},
		{"amount without currency", ConfirmedRefund{Amount: 2500}},
		{"currency without amount", ConfirmedRefund{Currency: "EUR"}},
		{"negative amount", ConfirmedRefund{Amount: -2500, Currency: "EUR"}},
		{"currency the schema would reject", ConfirmedRefund{Amount: 2500, Currency: "eur"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := j.CompleteCompensation(ctx, org, key, "refund", "refunded", "re_x", uuid.New(), c.confirmed); !errors.Is(err, ErrCompensationNotCompleted) {
				t.Fatalf("a refund must not complete on %+v, got %v", c.confirmed, err)
			}
		})
	}
	// Still BOUND — refused, not half-completed.
	comp, found, err := j.LookupCompensation(ctx, org, key, "refund")
	if err != nil || !found {
		t.Fatalf("lookup: %v found=%t", err, found)
	}
	if comp.Completed || comp.ConfirmedAmount != nil {
		t.Fatalf("a refused completion must leave the compensation bound and unconfirmed: %+v", comp)
	}
	// And it completes once the evidence is supplied — proving the refusals were about the
	// confirmation and not some other precondition this fixture failed.
	if err := j.CompleteCompensation(ctx, org, key, "refund", "refunded", "re_ok", uuid.New(), ConfirmedRefund{Amount: 2500, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	comp, _, err = j.LookupCompensation(ctx, org, key, "refund")
	if err != nil {
		t.Fatal(err)
	}
	if comp.ConfirmedAmount == nil || *comp.ConfirmedAmount != 2500 || comp.ConfirmedCurrency != "EUR" {
		t.Fatalf("the provider's figure must be persisted: %+v", comp)
	}

	// The mirror rule: a VOID moved nothing on the ledger, so a confirmation arriving on that
	// path is a contradiction and must be refused rather than stored.
	const voidKey = "comp-invariant-void"
	bind(t, "void", voidKey)
	if err := j.CompleteCompensation(ctx, org, voidKey, "void", "voided", "pi_x", uuid.New(), ConfirmedRefund{Amount: 2500, Currency: "EUR"}); !errors.Is(err, ErrCompensationNotCompleted) {
		t.Fatalf("a void carrying provider money must be refused, got %v", err)
	}
	if err := j.CompleteCompensation(ctx, org, voidKey, "void", "voided", "pi_x", uuid.New(), ConfirmedRefund{}); err != nil {
		t.Fatalf("a void with no confirmation must complete: %v", err)
	}
	voided, _, err := j.LookupCompensation(ctx, org, voidKey, "void")
	if err != nil {
		t.Fatal(err)
	}
	if !voided.Completed || voided.ConfirmedAmount != nil {
		t.Fatalf("a completed void must carry no confirmation: %+v", voided)
	}

	// Re-completing under the SAME fact is the concurrent-duplicate case: both requests pass
	// the handler short-circuit, both append the deterministic fact, one UPDATE wins. The
	// loser must see success, not a 500 (ai-review, third pass).
	if err := j.CompleteCompensation(ctx, org, key, "refund", "refunded", "re_ok", comp.FactID, ConfirmedRefund{Amount: 2500, Currency: "EUR"}); err != nil {
		t.Fatalf("re-completing under the same fact must be idempotent success, got %v", err)
	}
	// A DIFFERENT fact is not a duplicate — it would report someone else's result as this
	// call's.
	if err := j.CompleteCompensation(ctx, org, key, "refund", "refunded", "re_ok", uuid.New(), ConfirmedRefund{Amount: 2500, Currency: "EUR"}); !errors.Is(err, ErrCompensationNotCompleted) {
		t.Fatalf("completing under a different fact must be refused, got %v", err)
	}
	// A completion that matches no bound row reports it, rather than returning nil while the
	// caller has already appended a compensating fact to an append-only journal.
	if err := j.CompleteCompensation(ctx, org, "no-such-key", "refund", "refunded", "re_y", uuid.New(), ConfirmedRefund{Amount: 100, Currency: "EUR"}); !errors.Is(err, ErrCompensationNotCompleted) {
		t.Fatalf("a completion matching no row must say so, got %v", err)
	}
}
