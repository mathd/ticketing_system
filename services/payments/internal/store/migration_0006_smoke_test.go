//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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

	target, err := withDBName(dsn, dbName)
	if err != nil {
		t.Fatalf("build a DSN for the throwaway database: %v", err)
	}
	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	// ASSERT what we actually connected to, before running anything destructive. This test
	// calls MigrateDownTo, which drops columns — so a DSN rewrite that silently returned the
	// ORIGINAL database would roll 0006 out of the schema every other test in this package
	// is running against, concurrently, and the migration coverage claimed here would be a
	// claim about the shared database instead. The rewrite is derived from an environment
	// variable whose shape this code does not control, so it is checked rather than trusted
	// (TKT-257 ai-review, second pass).
	var current string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != dbName {
		t.Fatalf("refusing to run destructive migration tests against %q; the throwaway database %q was not reached", current, dbName)
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

// withDBName points a PostgreSQL DSN at a different database, keeping everything else.
//
// Both DSN forms libpq accepts are handled, because getting this wrong is silent: a rewrite
// that fails to change the database returns a perfectly valid connection to the WRONG one.
//
//   - URL form   — postgres://user:pw@host:port/dbname?opts
//   - keyword form — host=... dbname=... user=...  (also accepted by pgx)
//
// An unrecognized shape is an ERROR rather than a pass-through. The caller is about to run
// destructive migrations, so "I could not tell which database this is" must not resolve to
// "use whatever it was" — and the caller additionally verifies current_database().
func withDBName(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + name
		return u.String(), nil
	}
	if !strings.Contains(dsn, "=") {
		return "", fmt.Errorf("unrecognized PostgreSQL DSN form: not a URL and not keyword=value")
	}
	// Keyword form: replace dbname (and the `database` alias), preserving every other pair.
	// Values may be single-quoted, so a naive split on '=' is not enough; fields are
	// whitespace-separated outside quotes.
	fields, replaced := strings.Fields(dsn), false
	out := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		k, _, ok := strings.Cut(f, "=")
		if ok && (k == "dbname" || k == "database") {
			if replaced {
				return "", fmt.Errorf("ambiguous DSN: more than one dbname")
			}
			out, replaced = append(out, "dbname="+name), true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		out = append(out, "dbname="+name)
	}
	return strings.Join(out, " "), nil
}

// The rewrite is the thing that decides which database gets dropped, so it is tested on both
// DSN forms rather than only the one this environment happens to use today. Before this,
// a keyword DSN returned the input UNCHANGED — the shared test database, silently.
func TestWithDBNameRewritesEveryDSNForm(t *testing.T) {
	cases := []struct {
		name, dsn, want string
	}{
		{"url", "postgres://payments:pw@localhost:15432/payments_store_smoke", "postgres://payments:pw@localhost:15432/scratch"},
		{"url with options", "postgres://payments:pw@localhost:15432/payments_store_smoke?sslmode=disable", "postgres://payments:pw@localhost:15432/scratch?sslmode=disable"},
		{"postgresql scheme", "postgresql://u@h:5432/old", "postgresql://u@h:5432/scratch"},
		{"keyword", "host=localhost port=5432 dbname=payments_store_smoke user=payments", "host=localhost port=5432 dbname=scratch user=payments"},
		{"keyword alias", "host=localhost database=payments_store_smoke user=payments", "host=localhost dbname=scratch user=payments"},
		{"keyword without a database", "host=localhost user=payments", "host=localhost user=payments dbname=scratch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := withDBName(c.dsn, "scratch")
			if err != nil {
				t.Fatalf("withDBName(%q): %v", c.dsn, err)
			}
			if got != c.want {
				t.Fatalf("withDBName(%q)\n got=%q\nwant=%q", c.dsn, got, c.want)
			}
			if strings.Contains(got, "payments_store_smoke") {
				t.Fatalf("the shared database survived the rewrite: %q", got)
			}
		})
	}
	if _, err := withDBName("not-a-dsn", "scratch"); err == nil {
		t.Fatal("an unrecognized DSN form must be an error, never a pass-through to the original database")
	}
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

// The refund-confirmation invariant is enforced in CompleteCompensation itself, not only in
// the HTTP handler that calls it. This method is exported and is the persistence boundary
// for the rule, so a direct caller or a future recovery path could otherwise mark a
// compensation `refunded` with no provider evidence — the exact state this ticket makes
// unreachable. A guard only one caller happens to satisfy is a convention, not an invariant.
//
// Each case is refused for its OWN reason and each is asserted separately: a single
// "everything empty" case would be satisfied by a guard that checked only the amount.
func TestCompleteCompensationEnforcesTheConfirmationRule(t *testing.T) {
	db, ctx := migrationDB(t, "compensation")
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

	// A completion that matches no bound row reports it, rather than returning nil while the
	// caller has already appended a compensating fact to an append-only journal.
	if err := j.CompleteCompensation(ctx, org, "no-such-key", "refund", "refunded", "re_y", uuid.New(), ConfirmedRefund{Amount: 100, Currency: "EUR"}); !errors.Is(err, ErrCompensationNotCompleted) {
		t.Fatalf("a completion matching no row must say so, got %v", err)
	}
}
