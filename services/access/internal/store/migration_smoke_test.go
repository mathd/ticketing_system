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
	"strconv"
	"strings"
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
	// Snapshot with direct SQL in History's historical (occurred_at,id) order:
	// History itself now joins the integrity table, which does not exist at
	// schema version 1 — production never reads a half-migrated schema (the
	// out-of-band job finishes first, ADR-022), so the pre-upgrade read has no
	// production analogue to preserve.
	type immutable struct {
		ID         uuid.UUID
		Type       string
		OccurredAt time.Time
	}
	readRaw := func() []immutable {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id,event_type,occurred_at FROM lifecycle_events WHERE ticket_id=$1 ORDER BY occurred_at,id`, ticketID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []immutable
		for rows.Next() {
			var e immutable
			if err := rows.Scan(&e.ID, &e.Type, &e.OccurredAt); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	immutables := func(events []LifecycleEvent) []immutable {
		out := make([]immutable, len(events))
		for i, e := range events {
			out[i] = immutable{ID: e.ID, Type: e.Type, OccurredAt: e.OccurredAt}
		}
		return out
	}
	before := readRaw()
	if _, err = provider.Up(ctx); err != nil {
		t.Fatalf("apply migrations 0002 through 0004: %v", err)
	}
	after, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(immutables(after), before) {
		t.Fatalf("history changed during upgrade: before=%+v after=%+v", before, after)
	}
	for _, e := range after {
		if e.Sequence != nil {
			t.Fatalf("event %s has a sequence before the backfill adopted it", e.ID)
		}
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
	if !reflect.DeepEqual(immutables(adopted), before) {
		t.Fatalf("backfill rewrote history: before=%+v after=%+v", before, adopted)
	}
	for i, e := range adopted {
		if e.Sequence == nil || *e.Sequence != int64(i+1) {
			t.Fatalf("adopted event %s at position %d has sequence %v, want %d", e.ID, i, e.Sequence, i+1)
		}
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
	if err := rows.Err(); err != nil {
		t.Fatalf("integrity coverage iteration: %v", err)
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
	// 0009 added scanner device enrolment (ai-review S1). Pinned rather than
	// derived: this assertion exists so that adding a migration is a decision
	// someone states here, not a number that drifts.
	if err != nil || current != 9 || target != 9 {
		t.Fatalf("migration versions current=%d target=%d err=%v", current, target, err)
	}

	t.Logf("migration 0001 -> 0004 preserved %d events, adopted them as a signed baseline, and chained a redemption", len(before))
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
	if err := rows.Err(); err != nil {
		t.Fatalf("chain read: %v", err)
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

// ADR-025 §D7's migration order is binding: widened CHECK (distinct name), then
// the partial unique index while the table-wide UNIQUE still protects the
// table, then the drops. The order lives in the SQL source, so pin it there —
// the functional tests below prove the destination, this one proves the path.
func TestRepeatableAdmissionMigrationStatementOrder(t *testing.T) {
	raw, err := fs.ReadFile(migrationsFS, "migrations/0004_repeatable_admission_events.sql")
	if err != nil {
		t.Fatalf("migration 0004 is missing: %v", err)
	}
	sql := string(raw)
	steps := []string{
		"ADD CONSTRAINT lifecycle_events_event_type_admission_check",
		"CREATE UNIQUE INDEX lifecycle_events_singleton_type_uidx",
		"CREATE INDEX lifecycle_events_ticket_idx",
		"DROP CONSTRAINT lifecycle_events_ticket_id_event_type_key",
		"DROP CONSTRAINT lifecycle_events_event_type_check",
	}
	last := -1
	for _, s := range steps {
		i := strings.Index(sql, s)
		if i < 0 {
			t.Fatalf("migration 0004 lacks %q", s)
		}
		if i < last {
			t.Fatalf("migration 0004 runs %q out of ADR-025 §D7 order", s)
		}
		last = i
	}
	for _, banned := range []string{"CONCURRENTLY", "NOT VALID", "NO TRANSACTION"} {
		if strings.Contains(sql, banned) {
			t.Fatalf("migration 0004 contains %q (ADR-020/ADR-025 §D7 forbid it here)", banned)
		}
	}
}

// The destination schema: singletons stay unique per ticket via the partial
// index, repeatable types accept multiple rows, unknown types stay rejected.
func TestRepeatableAdmissionMigrationEnforcesPartialSingletonUniqueness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	ticketID := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',now())`,
		ticketID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	insert := func(eventType string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO lifecycle_events(id,ticket_id,event_type) VALUES($1,$2,$3)`,
			uuid.New(), ticketID, eventType)
		return err
	}
	for _, singleton := range []string{"issued", "delivered", "redeemed", "refunded"} {
		if err := insert(singleton); err != nil {
			t.Fatalf("first %s: %v", singleton, err)
		}
		if err := insert(singleton); err == nil {
			t.Fatalf("second %s row was accepted; singleton uniqueness is gone", singleton)
		}
	}
	for _, repeatable := range []string{"entry", "exit", "duplicate_admit"} {
		if err := insert(repeatable); err != nil {
			t.Fatalf("first %s: %v", repeatable, err)
		}
		if err := insert(repeatable); err != nil {
			t.Fatalf("second %s row rejected; %s is a repeatable type (ADR-025 §D1): %v", repeatable, repeatable, err)
		}
	}
	// `refunded` was this assertion's example of an unknown type until TKT-157 made
	// it a real one — and a singleton one, so it moves into the loop above rather
	// than merely being deleted from here. The stand-in is deliberately not a name
	// from ADR-003's roadmap (transferred, resold, exchanged, reissued,
	// invalidated), any of which could become real the same way.
	if err := insert("not_a_real_event_type"); err == nil {
		t.Fatal("unknown event type accepted; the widened CHECK is missing or too wide")
	}

	// Pin the final schema objects by name: the partial predicate, the plain
	// replacement index for repeatable-row reads (plan A1), and exactly one
	// CHECK on event_type.
	var predicate string
	if err := db.QueryRowContext(ctx, `SELECT pg_get_expr(ix.indpred, ix.indrelid) FROM pg_index ix
		JOIN pg_class c ON c.oid=ix.indexrelid WHERE c.relname='lifecycle_events_singleton_type_uidx'`).
		Scan(&predicate); err != nil {
		t.Fatalf("partial unique index missing: %v", err)
	}
	for _, want := range []string{"issued", "delivered", "redeemed", "refunded"} {
		if !strings.Contains(predicate, want) {
			t.Fatalf("partial index predicate %q lacks %q", predicate, want)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname='lifecycle_events_ticket_idx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("lifecycle_events_ticket_idx missing (n=%d err=%v): repeatable-row reads would seq-scan", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='lifecycle_events'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%event_type%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("want exactly one event_type CHECK after 0004, got %d (err=%v)", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname='lifecycle_events_ticket_id_event_type_key'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("table-wide UNIQUE still present after 0004 (n=%d err=%v)", n, err)
	}
}

// 0004 is DDL over existing rows: a signed chain written at version 3 must come
// through byte-for-byte, and the verifier must still pass.
func TestRepeatableAdmissionMigrationPreservesSignedHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	st := New(db, cfg)
	s := issueTicket(t, ctx, st, uuid.New())
	messageID, err := st.DeliveryID(ctx, s.ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.MarkDelivered(ctx, s.ticketID, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Redeem(ctx, s.redeemInput()); err != nil {
		t.Fatal(err)
	}

	snapshot := func(query string) string {
		t.Helper()
		var out string
		if err := db.QueryRowContext(ctx, query).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	const eventsQ = `SELECT coalesce(string_agg(id::text||':'||event_type||':'||occurred_at::text, ',' ORDER BY id),'') FROM lifecycle_events`
	const integrityQ = `SELECT coalesce(string_agg(event_id::text||':'||sequence||':'||encode(entry_hash,'hex')||':'||encode(previous_hash,'hex'), ',' ORDER BY event_id),'') FROM lifecycle_event_integrity`
	const headQ = `SELECT string_agg(ticket_id::text||':'||last_sequence||':'||encode(last_hash,'hex')||':'||key_id||':'||encode(signature,'hex'), ',' ORDER BY ticket_id) FROM lifecycle_heads`
	events, integrity, heads := snapshot(eventsQ), snapshot(integrityQ), snapshot(headQ)

	if _, err = provider.Up(ctx); err != nil {
		t.Fatalf("apply migration 0004: %v", err)
	}
	if got := snapshot(eventsQ); got != events {
		t.Fatalf("0004 changed lifecycle_events:\nbefore %s\nafter  %s", events, got)
	}
	if got := snapshot(integrityQ); got != integrity {
		t.Fatalf("0004 changed integrity rows:\nbefore %s\nafter  %s", integrity, got)
	}
	if got := snapshot(headQ); got != heads {
		t.Fatalf("0004 changed heads:\nbefore %s\nafter  %s", heads, got)
	}
	if err = New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify-lifecycle after 0004: %v", err)
	}
}

func TestRepeatableAdmissionMigrationIsIrreversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration 0004 rolled back; immutable ticket history is not protected")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname='lifecycle_events_singleton_type_uidx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("failed down attempt altered the schema (n=%d err=%v)", n, err)
	}
}

// The measured-migration obligation (ADR-025 §D7): the COMPLETE 0004 — CHECK
// validation scan, two index builds, both drops — at representative volume,
// against ADR-008/ADR-022's 30-second bound. Opt-in: seeding ~3×N rows takes
// minutes, which does not belong in every make check. Run with e.g.
// ACCESS_MIGRATION_MEASUREMENT_TICKETS=3333334; the result is recorded in
// docs/learnings/TKT-84-lifecycle-migration-measurement.md.
func TestRepeatableAdmissionMigrationRepresentativeVolume(t *testing.T) {
	nStr := os.Getenv("ACCESS_MIGRATION_MEASUREMENT_TICKETS")
	if nStr == "" {
		t.Skip("ACCESS_MIGRATION_MEASUREMENT_TICKETS is not set")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		t.Fatalf("ACCESS_MIGRATION_MEASUREMENT_TICKETS=%q is not a positive integer", nStr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}

	// Set-based seed at version 3: every row matches the partial-index
	// predicate, the worst case for the new index builds. Seeding time is
	// excluded from the measurement.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'signed-credential',now()
		FROM generate_series(1,$1)`, n); err != nil {
		t.Fatalf("seed tickets: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at)
		SELECT gen_random_uuid(), t.id, e.event_type, now()
		FROM tickets t CROSS JOIN (VALUES ('issued'),('delivered'),('redeemed')) AS e(event_type)`); err != nil {
		t.Fatalf("seed lifecycle events: %v", err)
	}
	var rows int64
	var tableSize string
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_events`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT pg_size_pretty(pg_total_relation_size('lifecycle_events'))`).Scan(&tableSize); err != nil {
		t.Fatal(err)
	}

	// The production bound: ADR-008's 30-second migrate context, fresh.
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer migrateCancel()
	start := time.Now()
	if _, err := provider.UpTo(migrateCtx, 4); err != nil {
		t.Fatalf("migration 0004 breached the 30s bound at %d lifecycle rows (%s): %v", rows, tableSize, err)
	}
	elapsed := time.Since(start)

	var version string
	_ = db.QueryRowContext(ctx, `SELECT version()`).Scan(&version)
	t.Logf("migration 0004: %v for %d lifecycle rows (%s) on %s", elapsed, rows, tableSize, version)
	if elapsed > 15*time.Second {
		t.Logf("WARNING: above the 15s engineering target — ship only with the reduced margin explicitly accepted")
	}
}

// 0006 (TKT-87): slot policy projection, typed quarantine facts, and the
// derived pass-policy conflict state table. Statement-order + banned-keyword
// pin, same shape as 0004's.
func TestPassPolicyMigrationStatementOrder(t *testing.T) {
	raw, err := fs.ReadFile(migrationsFS, "migrations/0006_pass_admission_policy.sql")
	if err != nil {
		t.Fatalf("migration 0006 is missing: %v", err)
	}
	sql := string(raw)
	steps := []string{
		"CREATE TABLE slot_re_entry_policies",
		"ADD COLUMN event_type",
		"CREATE INDEX lifecycle_integrity_quarantine_ticket_idx",
		"CREATE TABLE pass_policy_conflicts",
	}
	last := -1
	for _, s := range steps {
		i := strings.Index(sql, s)
		if i < 0 {
			t.Fatalf("migration 0006 lacks %q", s)
		}
		if i < last {
			t.Fatalf("migration 0006 runs %q out of order", s)
		}
		last = i
	}
	for _, banned := range []string{"CONCURRENTLY", "NOT VALID", "NO TRANSACTION"} {
		if strings.Contains(sql, banned) {
			t.Fatalf("migration 0006 contains %q (ADR-020/ADR-022 forbid it here)", banned)
		}
	}
}

// The destination schema for 0006: policy rows carry ADR-005's cross-field
// invariants; existing quarantine rows are typed 'redeemed' (they are
// single-entry degraded/reconciliation records — ADR-025 §D1 gives single
// tickets no entry/exit vocabulary); pass facts type at insert; the conflict
// state table enforces bounded rule/status.
func TestPassPolicyMigrationSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}

	// A pre-0006 quarantine row, to prove the backfill types it 'redeemed'.
	ticketID := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',now())`,
		ticketID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,admitted_at) VALUES($1,$2,'pre-0006 degraded admission',now())`,
		ticketID, uuid.New()); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("migration 0006: %v", err)
	}

	var eventType string
	if err := db.QueryRowContext(ctx, `SELECT event_type FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, ticketID).Scan(&eventType); err != nil {
		t.Fatalf("quarantine event_type column missing: %v", err)
	}
	if eventType != "redeemed" {
		t.Fatalf("pre-0006 quarantine row typed %q, want redeemed", eventType)
	}
	insertQuarantine := func(eventType string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at,event_type) VALUES($1,$2,'reconciled offline fact',$3,now(),$4)`,
			ticketID, uuid.New(), uuid.New(), eventType)
		return err
	}
	for _, ok := range []string{"entry", "exit"} {
		if err := insertQuarantine(ok); err != nil {
			t.Fatalf("quarantine insert %s: %v", ok, err)
		}
	}
	if err := insertQuarantine("duplicate_admit"); err == nil {
		t.Fatal("quarantine accepted event_type duplicate_admit; the CHECK is too wide")
	}

	// Policy rows: ADR-005 cross-field invariants mirrored from catalog's 0004.
	insertPolicy := func(mode string, maxEntries any, requiresExit bool) error {
		_, err := db.ExecContext(ctx, `INSERT INTO slot_re_entry_policies(slot_id,organizer_id,mode,max_entries,requires_exit) VALUES($1,$2,$3,$4,$5)`,
			uuid.New(), uuid.New(), mode, maxEntries, requiresExit)
		return err
	}
	if err := insertPolicy("single", nil, false); err != nil {
		t.Fatalf("single policy: %v", err)
	}
	if err := insertPolicy("multi", nil, true); err != nil {
		t.Fatalf("multi policy: %v", err)
	}
	if err := insertPolicy("count_limited", 3, false); err != nil {
		t.Fatalf("count_limited policy: %v", err)
	}
	if err := insertPolicy("count_limited", nil, false); err == nil {
		t.Fatal("count_limited without max_entries accepted")
	}
	if err := insertPolicy("multi", 3, false); err == nil {
		t.Fatal("max_entries on non-count_limited mode accepted")
	}
	if err := insertPolicy("count_limited", 0, false); err == nil {
		t.Fatal("non-positive max_entries accepted")
	}
	if err := insertPolicy("open_bar", nil, false); err == nil {
		t.Fatal("unknown mode accepted")
	}

	// Conflict state rows: bounded rule/status, positive version.
	insertConflict := func(rule, status string, version int) error {
		_, err := db.ExecContext(ctx, `INSERT INTO pass_policy_conflicts(ticket_id,organizer_id,slot_id,rule,occurrence_id,status,version) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			ticketID, uuid.New(), uuid.New(), rule, uuid.New(), status, version)
		return err
	}
	if err := insertConflict("entry_limit_reached", "raised", 1); err != nil {
		t.Fatalf("conflict insert: %v", err)
	}
	if err := insertConflict("exit_required", "withdrawn", 2); err != nil {
		t.Fatalf("conflict insert: %v", err)
	}
	if err := insertConflict("vibes", "raised", 1); err == nil {
		t.Fatal("unknown conflict rule accepted")
	}
	if err := insertConflict("exit_required", "maybe", 1); err == nil {
		t.Fatal("unknown conflict status accepted")
	}
	if err := insertConflict("exit_required", "raised", 0); err == nil {
		t.Fatal("non-positive conflict version accepted")
	}

	// The union read (trace ∪ quarantine) is indexed by ticket or every pass
	// scan seq-scans quarantine (ADR-019's lesson, generalized by 0004).
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname='lifecycle_integrity_quarantine_ticket_idx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("lifecycle_integrity_quarantine_ticket_idx missing (n=%d err=%v)", n, err)
	}
}

// 0006's measured-migration obligation (ADR-025 §D7 pattern, ADR-008's 30s
// bound): the complete migration — two new tables, the quarantine event_type
// column (constant default: metadata-only on this PG), its CHECK validation
// scan, and the quarantine ticket index build — at representative quarantine
// volume. Quarantine is an error table; realistic volumes are orders of
// magnitude below what this seeds.
func TestPassPolicyMigrationRepresentativeVolume(t *testing.T) {
	nStr := os.Getenv("ACCESS_MIGRATION_MEASUREMENT_QUARANTINE_ROWS")
	if nStr == "" {
		t.Skip("ACCESS_MIGRATION_MEASUREMENT_QUARANTINE_ROWS is not set")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		t.Fatalf("ACCESS_MIGRATION_MEASUREMENT_QUARANTINE_ROWS=%q is not a positive integer", nStr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}

	// Seed at version 5: reconciliation-shaped rows (occurrence-keyed), the
	// realistic bulk. Seeding time is excluded from the measurement.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),'signed-credential',now()
		FROM generate_series(1,100)`); err != nil {
		t.Fatalf("seed tickets: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_integrity_quarantine(ticket_id,organizer_id,reason,occurrence_id,occurred_at)
		SELECT t.id, t.organizer_id, 'measurement seed', gen_random_uuid(), now()
		FROM tickets t CROSS JOIN generate_series(1, $1/100)`, n); err != nil {
		t.Fatalf("seed quarantine rows: %v", err)
	}
	var rows int64
	var tableSize string
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_integrity_quarantine`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT pg_size_pretty(pg_total_relation_size('lifecycle_integrity_quarantine'))`).Scan(&tableSize); err != nil {
		t.Fatal(err)
	}

	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer migrateCancel()
	start := time.Now()
	if _, err := provider.UpTo(migrateCtx, 6); err != nil {
		t.Fatalf("migration 0006 breached the 30s bound at %d quarantine rows (%s): %v", rows, tableSize, err)
	}
	elapsed := time.Since(start)

	var version string
	_ = db.QueryRowContext(ctx, `SELECT version()`).Scan(&version)
	t.Logf("migration 0006: %v for %d quarantine rows (%s) on %s", elapsed, rows, tableSize, version)
	if elapsed > 15*time.Second {
		t.Logf("WARNING: above the 15s engineering target — ship only with the reduced margin explicitly accepted")
	}
}
