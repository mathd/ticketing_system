//go:build smoke

package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"ticketing/services/access/internal/lifecycle"
)

const testKID = "access-lifecycle/test"

// testConfig builds a store that can sign. Every append writes a signed
// integrity row (ADR-021 §D1), so there is no unsigned store to test against.
func testConfig(t *testing.T) Config {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := lifecycle.NewSigner(base64.RawStdEncoding.EncodeToString(priv.Seed()), testKID)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := lifecycle.NewKeyring(testKID + "=" + base64.RawStdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	return Config{Signer: signer, Keyring: keyring, Policy: DefaultPolicy()}
}

// verifyOnlyConfig mirrors what `access verify-lifecycle` builds: public keys,
// no signer.
func verifyOnlyConfig(t *testing.T, cfg Config) Config {
	t.Helper()
	return Config{Keyring: cfg.Keyring}
}

func schemaDB(t *testing.T, ctx context.Context) (*sql.DB, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACCESS_MIGRATION_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "access_migration_" + uuid.NewString()[:8]
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

func TestRedeemedLifecycleMigrationPreservesHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatalf("apply migration 0001: %v", err)
	}

	ticketID, orderID := uuid.New(), uuid.New()
	organizerID, slotID := uuid.New(), uuid.New()
	issuedID, deliveredID := uuid.New(), uuid.New()
	issuedAt := time.Date(2026, time.July, 12, 14, 30, 0, 0, time.UTC)
	deliveredAt := issuedAt.Add(2 * time.Minute)
	_, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',$8)`,
		ticketID, orderID, uuid.New(), organizerID, uuid.New(), slotID, uuid.New(), issuedAt)
	if err != nil {
		t.Fatalf("seed pre-upgrade ticket: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at)
		VALUES($1,$2,'issued',$3),($4,$2,'delivered',$5)`,
		issuedID, ticketID, issuedAt, deliveredID, deliveredAt)
	if err != nil {
		t.Fatalf("seed pre-upgrade history: %v", err)
	}

	cfg := testConfig(t)
	st := New(db, cfg)
	before, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatalf("apply migrations 0002 and 0003: %v", err)
	}
	after, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("history changed during upgrade: before=%+v after=%+v", before, after)
	}

	// Migration 0003 is DDL only: it must not have adopted anything. The backfill
	// is a separate job precisely because signing history cannot be held to
	// ADR-008's 30-second migrate deadline (ADR-021 §D9, amended for ADR-022).
	var coveredByMigration int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_event_integrity`).Scan(&coveredByMigration); err != nil {
		t.Fatal(err)
	}
	if coveredByMigration != 0 {
		t.Fatalf("migration 0003 chained %d rows; adoption belongs to the backfill job, not to migrate", coveredByMigration)
	}

	// Before the backfill, coverage is incomplete and the verifier must say so —
	// this is what keeps a half-adopted trail from being served.
	verifier := New(db, verifyOnlyConfig(t, cfg))
	if err = verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err == nil {
		t.Fatal("verify-lifecycle passed while legacy rows were still unchained")
	}

	chained, err := st.BackfillLifecycle(ctx, 8)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if chained != 1 {
		t.Fatalf("backfill chained %d tickets, want 1", chained)
	}
	// Resumable: a second run must be a no-op, not a duplicate chain. An
	// interrupted job is re-run, so this is the ordinary case, not an edge one.
	again, err := st.BackfillLifecycle(ctx, 8)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if again != 0 {
		t.Fatalf("second backfill chained %d tickets; it is not resumable", again)
	}

	// The pre-upgrade rows must be byte-for-byte what they were: the chain lives
	// beside lifecycle_events and never rewrites it (ADR-021 §D1).
	adopted, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(adopted, before) {
		t.Fatalf("backfill rewrote history: before=%+v after=%+v", before, adopted)
	}

	// One integrity row per legacy event, in History's order.
	rows, err := db.QueryContext(ctx, `SELECT event_id,sequence FROM lifecycle_event_integrity WHERE ticket_id=$1 ORDER BY sequence`, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	var covered []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatal(err)
		}
		if seq != int64(len(covered)+1) {
			t.Fatalf("backfilled sequence %d out of order", seq)
		}
		covered = append(covered, id)
	}
	_ = rows.Close()
	if len(covered) != 2 || covered[0] != issuedID || covered[1] != deliveredID {
		t.Fatalf("backfill adopted %v, want [issued delivered] in (occurred_at,id) order", covered)
	}

	var headSeq int64
	var headKey string
	if err = db.QueryRowContext(ctx, `SELECT last_sequence,key_id FROM lifecycle_heads WHERE ticket_id=$1`, ticketID).Scan(&headSeq, &headKey); err != nil {
		t.Fatalf("backfill left no head: %v", err)
	}
	if headSeq != 2 || headKey != testKID {
		t.Fatalf("backfilled head = sequence %d under %q, want 2 under %q", headSeq, headKey, testKID)
	}

	if err = verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify-lifecycle failed on a freshly backfilled trail: %v", err)
	}

	result, err := st.Redeem(ctx, RedeemInput{
		TicketID: ticketID, OrderID: orderID, OrganizerID: organizerID, SlotID: slotID,
	})
	if err != nil {
		t.Fatalf("redeem upgraded ticket: %v", err)
	}
	if !result.Accepted || result.Decision != DecisionAccepted || result.OccurredAt.IsZero() {
		t.Fatalf("redemption result = %+v", result)
	}
	history, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].ID != issuedID || history[1].ID != deliveredID || history[2].Type != "redeemed" {
		t.Fatalf("post-redemption history = %+v", history)
	}
	if !history[0].OccurredAt.Equal(issuedAt) || !history[1].OccurredAt.Equal(deliveredAt) {
		t.Fatalf("pre-upgrade timestamps changed: %+v", history[:2])
	}
	// The redemption chained onto the adopted baseline rather than starting over.
	if err = db.QueryRowContext(ctx, `SELECT last_sequence FROM lifecycle_heads WHERE ticket_id=$1`, ticketID).Scan(&headSeq); err != nil {
		t.Fatal(err)
	}
	if headSeq != 3 {
		t.Fatalf("head after redemption = %d, want 3", headSeq)
	}

	// A checkpoint over the backfilled + redeemed heads, then a full verify.
	if _, err = st.CheckpointOrganizer(ctx, organizerID, LastRoot{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err = verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify-lifecycle failed after redemption and checkpoint: %v", err)
	}

	if _, err = db.ExecContext(ctx, `UPDATE lifecycle_events SET occurred_at=now() WHERE id=$1`, issuedID); err == nil {
		t.Fatal("upgraded lifecycle history is no longer immutable")
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil || current != 3 || target != 3 {
		t.Fatalf("migration versions current=%d target=%d err=%v", current, target, err)
	}

	t.Logf("migration 0001 -> 0003 preserved %d events, adopted them as a signed baseline, and chained a redemption", len(before))
}

// The backfill adopts (occurred_at, id) — the exact order History has always
// exposed. Equal timestamps are real (Issue writes the issued event with the
// ticket's own issued_at), so the tie break is part of the signed order and has
// to be the same one the read path uses, or the chain would attest an order
// nobody ever saw.
func TestBackfillChainsInHistoryOrderOnTimestampTies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	ticketID := uuid.New()
	organizerID := uuid.New()
	tie := time.Date(2026, time.July, 12, 9, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',$8)`,
		ticketID, uuid.New(), uuid.New(), organizerID, uuid.New(), uuid.New(), uuid.New(), tie); err != nil {
		t.Fatal(err)
	}
	// Both events at the same instant, with ids chosen so the tie break is
	// observable rather than incidental.
	lowID := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	highID := uuid.MustParse("ff000000-0000-0000-0000-0000000000ff")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at) VALUES($1,$2,'issued',$3),($4,$2,'delivered',$3)`,
		highID, ticketID, tie, lowID); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	st := New(db, cfg)
	want, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.BackfillLifecycle(ctx, 8); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT event_id FROM lifecycle_event_integrity WHERE ticket_id=$1 ORDER BY sequence`, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("chained %d events, history has %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i].ID {
			t.Fatalf("chain order %v does not match History order %v: the trail would attest an order no reader ever saw", got, want)
		}
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after tie-ordered backfill: %v", err)
	}
}
