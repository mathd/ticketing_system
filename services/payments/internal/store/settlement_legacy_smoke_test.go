//go:build smoke

package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Second review pass: migration 0004 first REFUSED to apply to any database that
// already held captures. That made the invariant true of the table at the cost of
// bricking every upgrade — any database that has ever completed a checkout has
// such facts, including a developer's persistent volume.
//
// The composition of an old capture is genuinely unrecoverable: the journal knows
// the amount, not which part was face value and which was owed to whom. So the
// backfill records what IS true — the whole amount, attributed to nobody — under
// its own entry kind. The ledger balances, the upgrade works, and the captures
// whose split is unknown are queryable rather than silently absent.
func TestMigrationBackfillsCapturesThatPredateTheLedger(t *testing.T) {
	dsn := os.Getenv("PAYMENTS_LEGACY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PAYMENTS_LEGACY_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatal(err)
	}
	// Stop one version SHORT of the ledger, so the capture below is written by a
	// schema that knew nothing about settlement — which is the whole point.
	if _, err := p.UpTo(ctx, 3); err != nil {
		t.Fatalf("migrate to 0003: %v", err)
	}

	org, order := uuid.New(), uuid.New()
	j := New(db, fullRing(t))
	legacy := Fact{
		ID: uuid.New(), OrganizerID: org, Type: "payment.captured",
		OccurredAt: time.Now().UTC(), BuyerID: uuid.New(), Amount: 4200, Currency: "EUR",
		Payload: map[string]string{"order_id": order.String()},
	}
	if _, _, err := j.Append(ctx, legacy); err != nil {
		t.Fatalf("a capture under the pre-ledger schema must commit: %v", err)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("0004 must apply to a database that already holds captures: %v", err)
	}

	var kind string
	var amount int64
	var payee, feeCode *string
	if err := db.QueryRowContext(ctx,
		`SELECT entry_kind, amount, payee_id::text, fee_code FROM settlement_entries
		  WHERE capture_fact_id=$1`, legacy.ID).Scan(&kind, &amount, &payee, &feeCode); err != nil {
		t.Fatalf("the pre-existing capture has no ledger line: %v", err)
	}
	if kind != "legacy_unattributed" {
		t.Errorf("entry_kind=%q, want legacy_unattributed — the split is unknown, and the "+
			"ledger must say so rather than guess", kind)
	}
	if amount != 4200 {
		t.Errorf("backfilled %d, want the whole captured 4200", amount)
	}
	if payee != nil || feeCode != nil {
		t.Errorf("payee=%v fee_code=%v, want both absent — nobody is known to be owed this", payee, feeCode)
	}
}

// Third review pass: the backfill cast `payload ->> 'order_id'` to uuid without
// guarding it. Migration 0001 never required payloads to carry an order_id, and
// /internal/facts accepts an arbitrary payload for an allowlisted type — so a
// capture with a missing or malformed one aborts the whole migration on an opaque
// cast error. That is the bricking failure again in a different disguise.
//
// The migration must fail on this — such a fact genuinely cannot be attributed —
// but it must fail SAYING SO, and only for the rows that are actually broken.
func TestMigrationNamesCapturesItCannotAttribute(t *testing.T) {
	dsn := os.Getenv("PAYMENTS_LEGACY_MALFORMED_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PAYMENTS_LEGACY_MALFORMED_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 3); err != nil {
		t.Fatalf("migrate to 0003: %v", err)
	}

	// Written by direct SQL, and that is the point rather than a shortcut: the Go
	// append path validates the payload, so a capture with no order_id can only
	// come from outside it — a broken writer, or an operator's hand. Those are
	// exactly the rows the migration must not abort opaquely on.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO journal_entries
		  (fact_id,organizer_id,sequence,fact_type,occurred_at,buyer_id,amount,currency,
		   payload,previous_hash,entry_hash,key_id,signature)
		VALUES ($1,$2,1,'payment.captured',now(),$3,1500,'EUR','{}'::jsonb,
		        decode(repeat('00',32),'hex'), decode(repeat('11',32),'hex'), 'legacy',
		        decode(repeat('22',32),'hex'))`,
		uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed a capture with no order_id: %v", err)
	}

	_, err = p.Up(ctx)
	if err == nil {
		t.Fatal("0004 applied over a capture it cannot attribute — the ledger would claim " +
			"completeness it does not have")
	}
	if !strings.Contains(err.Error(), "no usable order_id") {
		t.Errorf("error was %q, want it to name the cause — an opaque cast failure tells an "+
			"operator nothing about what to inspect", err)
	}
}
