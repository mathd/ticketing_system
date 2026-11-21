//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TKT-525, migration 0007: the append-only abandonment marker for one refund leg.
//
// These tests stand the database up at 0006, write legs in the old shape, then apply 0007, as
// migration_0006_smoke_test.go does and for the same reason: a fully migrated database cannot
// show what the migration did to data written before it.

const migration0007Env = "PAYMENTS_MIGRATION_0007_TEST_DATABASE_URL"

func seedPre0007(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,'charge-0007','fingerprint','captured',$2,$3,2000,'EUR','pi_0007','captured',2000,2000,now())`,
		org, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed operation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,
		                                provider_idempotency_key,amount,currency)
		VALUES($1,'charge-0007','bound-leg','psp-refund-leg-v1:bound',1000,'EUR')`, org); err != nil {
		t.Fatalf("seed bound leg: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,
		                                provider_idempotency_key,amount,currency,status,provider_ref,fact_id,completed_at,
		                                confirmed_amount,confirmed_currency)
		VALUES($1,'charge-0007','completed-leg','psp-refund-leg-v1:done',1000,'EUR','refunded','re_done',$2,now(),1000,'EUR')`,
		org, uuid.New()); err != nil {
		t.Fatalf("seed completed leg: %v", err)
	}
}

func markLeg(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, org uuid.UUID, refundKey, ref string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
		VALUES($1,'charge-0007',$2,$3)`, org, refundKey, ref)
	return err
}

func pgConstraint(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.ConstraintName
	}
	return ""
}

func payments0007Version(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// 0007 applies over existing bound and completed legs without touching them, and the marker's
// own constraints each refuse with their exact name.
func TestMigration0007PreservesLegsAndConstrainsMarkers(t *testing.T) {
	db, ctx := migrationDB(t, migration0007Env)
	if err := MigrateUpTo(ctx, db, 6); err != nil {
		t.Fatalf("migrate to 0006: %v", err)
	}
	org := uuid.New()
	seedPre0007(t, db, ctx, org)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("0007 must apply over existing legs: %v", err)
	}
	var legs int
	var boundSum int64
	if err := db.QueryRowContext(ctx, `SELECT count(*), sum(amount) FILTER (WHERE status='bound') FROM payment_refund_legs WHERE organizer_id=$1`, org).
		Scan(&legs, &boundSum); err != nil || legs != 2 || boundSum != 1000 {
		t.Fatalf("legs after 0007: count=%d bound sum=%d err=%v, want 2 and 1000", legs, boundSum, err)
	}

	if err := markLeg(ctx, db, org, "bound-leg", "re_failed"); err != nil {
		t.Fatalf("a marker for an existing leg was refused: %v", err)
	}
	for _, c := range []struct {
		name, refundKey, ref, want string
	}{
		{"no such leg", "missing-leg", "re_x", "payment_refund_leg_abandonments_leg_fkey"},
		{"blank provider reference", "completed-leg", "  ", "payment_refund_leg_abandonments_provider_ref_check"},
		{"second marker for one leg", "bound-leg", "re_other", "payment_refund_leg_abandonments_pkey"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := pgConstraint(markLeg(ctx, db, org, c.refundKey, c.ref)); got != c.want {
				t.Fatalf("violated %q, want %q", got, c.want)
			}
		})
	}
	// The marker names its leg in this organizer only.
	if got := pgConstraint(markLeg(ctx, db, uuid.New(), "bound-leg", "re_x")); got != "payment_refund_leg_abandonments_leg_fkey" {
		t.Fatalf("a marker for the same keys in another organizer violated %q, want the leg foreign key", got)
	}
	for name, stmt := range map[string]string{
		"update":   `UPDATE payment_refund_leg_abandonments SET provider_ref='re_rewritten'`,
		"delete":   `DELETE FROM payment_refund_leg_abandonments`,
		"truncate": `TRUNCATE payment_refund_leg_abandonments`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("%s was not refused by the append-only trigger (err=%v)", name, err)
			}
		})
	}
	var ref string
	if err := db.QueryRowContext(ctx, `SELECT provider_ref FROM payment_refund_leg_abandonments WHERE organizer_id=$1`, org).Scan(&ref); err != nil || ref != "re_failed" {
		t.Fatalf("the marker did not survive intact: ref=%q err=%v", ref, err)
	}
}

// The Down refuses with a marker present and leaves version and marker intact; with no marker it
// removes the table and 0007 applies again.
func TestMigration0007DownRefusesOverMarkers(t *testing.T) {
	db, ctx := migrationDB(t, migration0007Env)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org := uuid.New()
	seedPre0007(t, db, ctx, org)
	if err := markLeg(ctx, db, org, "bound-leg", "re_failed"); err != nil {
		t.Fatal(err)
	}
	err := MigrateDownTo(ctx, db, 6)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back 0007") {
		t.Fatalf("0007 rolled back over an abandonment marker (err=%v)", err)
	}
	if v := payments0007Version(t, ctx, db); v != 7 {
		t.Fatalf("a refused Down left the schema at %d", v)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payment_refund_leg_abandonments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a refused Down did not leave the marker intact: n=%d err=%v", n, err)
	}

	if _, err := db.ExecContext(ctx, `ALTER TABLE payment_refund_leg_abandonments DISABLE TRIGGER USER;
		DELETE FROM payment_refund_leg_abandonments;
		ALTER TABLE payment_refund_leg_abandonments ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDownTo(ctx, db, 6); err != nil {
		t.Fatalf("0007 down with no markers: %v", err)
	}
	if v := payments0007Version(t, ctx, db); v != 6 {
		t.Fatalf("schema at %d after a clean Down, want 6", v)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_name='payment_refund_leg_abandonments')`).Scan(&exists); err != nil || exists {
		t.Fatalf("Down left the marker table: exists=%t err=%v", exists, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-apply 0007: %v", err)
	}
}

// The Down locks before it checks. A marker insert that is open when the Down starts makes the
// Down wait; once it commits, the check must see it and refuse.
func TestMigration0007DownLocksBeforeItChecksMarkers(t *testing.T) {
	db, ctx := migrationDB(t, migration0007Env)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org := uuid.New()
	seedPre0007(t, db, ctx, org)

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if err := markLeg(ctx, writer, org, "bound-leg", "re_failed"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- MigrateDownTo(ctx, db, 6) }()

	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			  AND ltrim(query) LIKE 'LOCK TABLE payment_operations, payment_refund_legs, payment_refund_leg_abandonments%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the Down finished before it waited on the open marker insert (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the Down never waited on the open marker insert")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "cannot roll back 0007") {
		t.Fatalf("0007 rolled back over a marker that committed while it waited (err=%v). The check ran before the lock", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payment_refund_leg_abandonments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the committed marker is gone: n=%d err=%v", n, err)
	}
}
