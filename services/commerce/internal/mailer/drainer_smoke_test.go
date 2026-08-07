//go:build smoke

package mailer

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/shared/mail"

	"ticketing/services/commerce/internal/store"
)

// The mail drainer against real Postgres (TKT-226 / ADR-050).
//
// Real Postgres rather than a fake store, because every claim here is a property of the
// SQL: the claim lease, the `attempts >= max` dead-letter arithmetic, the backoff that
// stops a poison row starving the queue, and the claim_id guard that stops a superseded
// claimant retiring someone else's row. A fake cannot see any of it.

func drainerDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, ctx
}

// quiet keeps the drainer's expected WARN/ERROR lines out of test output. It also means
// a test asserting on log CONTENT has to install its own writer, which is what
// TestTheDrainerLogsNothingFromTheMessage does.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// enqueue writes one message directly, returning its id. Tests that only need "a row
// exists" use this rather than driving RequestPasswordReset, so a failure here points at
// the drainer rather than at the reset path.
func enqueue(t *testing.T, db *sql.DB, ctx context.Context, recipient, subject, body string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO mail_outbox (recipient, subject, body) VALUES ($1,$2,$3) RETURNING id`,
		recipient, subject, body).Scan(&id); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

func rowState(t *testing.T, db *sql.DB, ctx context.Context, id uuid.UUID) (sentAt, deadAt sql.NullTime, attempts int, lastErr sql.NullString) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT sent_at, dead_lettered_at, attempts, last_error FROM mail_outbox WHERE id=$1`, id).
		Scan(&sentAt, &deadAt, &attempts, &lastErr); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return
}

func uniqueRecipient() string { return "drain+" + uuid.NewString()[:8] + "@example.test" }

func TestDrainOnceSendsAndRetiresTheRow(t *testing.T) {
	db, ctx := drainerDB(t)
	to := uniqueRecipient()
	id := enqueue(t, db, ctx, to, "Reset your password", "https://x.test/r?token=abc")

	f := mail.NewFake()
	d := New(db, f, time.Second, 32, quiet())
	if n := d.DrainOnce(ctx); n < 1 {
		t.Fatalf("sent = %d, want at least 1", n)
	}

	var found bool
	for _, m := range f.Sent() {
		if m.To == to {
			found = true
			if m.Subject != "Reset your password" || m.Body != "https://x.test/r?token=abc" {
				t.Fatalf("the row's message reached the sender mangled: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("the enqueued message never reached the sender")
	}
	sentAt, deadAt, attempts, _ := rowState(t, db, ctx, id)
	if !sentAt.Valid {
		t.Fatal("a delivered row must be retired")
	}
	if deadAt.Valid {
		t.Fatal("a delivered row must not be dead-lettered")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

// A retired row is never sent again. This is what stops a restart re-mailing every reset
// link the system has ever issued.
func TestASentMessageIsNotClaimedAgain(t *testing.T) {
	db, ctx := drainerDB(t)
	to := uniqueRecipient()
	enqueue(t, db, ctx, to, "s", "b")

	f := mail.NewFake()
	d := New(db, f, time.Second, 32, quiet())
	d.DrainOnce(ctx)
	d.DrainOnce(ctx)

	var count int
	for _, m := range f.Sent() {
		if m.To == to {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the message was sent %d times across two drains, want 1", count)
	}
}

// The invariant the outbox exists for: a failed send must NOT look delivered.
func TestAFailedSendLeavesTheRowUnsentAndRetryable(t *testing.T) {
	db, ctx := drainerDB(t)
	id := enqueue(t, db, ctx, uniqueRecipient(), "s", "b")

	f := mail.NewFake()
	f.FailWith(mail.ErrFakeRefused)
	d := New(db, f, time.Second, 32, quiet())
	if n := d.DrainOnce(ctx); n != 0 {
		t.Fatalf("sent = %d on a failing sender, want 0", n)
	}

	sentAt, deadAt, attempts, lastErr := rowState(t, db, ctx, id)
	if sentAt.Valid {
		t.Fatal("a row whose send FAILED was marked sent — a lost reset now looks delivered")
	}
	if deadAt.Valid {
		t.Fatal("one failure must not dead-letter")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Fatal("the failure cause must be durable for an operator")
	}

	// And it comes back once the backoff lapses.
	if _, err := db.ExecContext(ctx, `UPDATE mail_outbox SET next_attempt_at=now() WHERE id=$1`, id); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	f.FailWith(nil)
	if n := d.DrainOnce(ctx); n < 1 {
		t.Fatalf("a recovered sender must deliver the retried row, sent = %d", n)
	}
	sentAt, _, _, _ = rowState(t, db, ctx, id)
	if !sentAt.Valid {
		t.Fatal("the retry did not retire the row")
	}
}

// Backoff is what keeps a poison row from starving newer messages: claiming is
// oldest-first, so an immediately-retryable failure at the head is re-selected forever.
func TestAFailedSendIsNotRetriedImmediately(t *testing.T) {
	db, ctx := drainerDB(t)
	id := enqueue(t, db, ctx, uniqueRecipient(), "s", "b")

	f := mail.NewFake()
	f.FailWith(mail.ErrFakeRefused)
	d := New(db, f, time.Second, 32, quiet())
	d.DrainOnce(ctx)

	_, _, attemptsAfterFirst, _ := rowState(t, db, ctx, id)
	d.DrainOnce(ctx)
	_, _, attemptsAfterSecond, _ := rowState(t, db, ctx, id)
	if attemptsAfterSecond != attemptsAfterFirst {
		t.Fatalf("the row was re-claimed inside its backoff (attempts %d → %d)",
			attemptsAfterFirst, attemptsAfterSecond)
	}
}

// Bounded retries. Without the quarantine one permanently-failing message is claimed
// forever; with it, the row stops being selected and stays visible to an operator.
func TestAPoisonMessageIsDeadLetteredAndStopsBeingClaimed(t *testing.T) {
	db, ctx := drainerDB(t)
	id := enqueue(t, db, ctx, uniqueRecipient(), "s", "b")

	f := mail.NewFake()
	f.FailWith(mail.ErrFakeRefused)
	d := New(db, f, time.Second, 32, quiet())

	for range store.MaxMailAttempts + 1 {
		// Clear the backoff each pass so the loop drives attempts rather than the clock.
		if _, err := db.ExecContext(ctx, `UPDATE mail_outbox SET next_attempt_at=now() WHERE id=$1`, id); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		d.DrainOnce(ctx)
	}

	_, deadAt, attempts, _ := rowState(t, db, ctx, id)
	if !deadAt.Valid {
		t.Fatalf("a row failing %d times must be dead-lettered, not retried forever (attempts=%d)",
			store.MaxMailAttempts, attempts)
	}
	// Quarantined means unclaimable even with a working sender and no backoff.
	f.FailWith(nil)
	if _, err := db.ExecContext(ctx, `UPDATE mail_outbox SET next_attempt_at=now() WHERE id=$1`, id); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	before := len(f.Sent())
	d.DrainOnce(ctx)
	if len(f.Sent()) != before {
		t.Fatal("a dead-lettered row was claimed again")
	}
}

// The claim_id guard. A drainer whose lease lapsed mid-send must not retire a row
// another drainer has since claimed, or it would mask a send that never happened.
func TestAStaleClaimantCannotRetireAnotherClaimantsRow(t *testing.T) {
	db, ctx := drainerDB(t)
	id := enqueue(t, db, ctx, uniqueRecipient(), "s", "b")

	first, err := store.ClaimMail(ctx, db, 10, time.Hour)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	var mine *store.OutboundMessage
	for i := range first {
		if first[i].ID == id {
			mine = &first[i]
		}
	}
	if mine == nil {
		t.Fatal("the enqueued row was not claimed")
	}
	// Its lease lapses and a second drainer takes it.
	if _, err := db.ExecContext(ctx, `UPDATE mail_outbox SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}
	if _, err := store.ClaimMail(ctx, db, 10, time.Hour); err != nil {
		t.Fatalf("second claim: %v", err)
	}

	retired, err := store.MarkMailSent(ctx, db, id, mine.ClaimID)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if retired {
		t.Fatal("the superseded claimant retired a row the new claimant owns")
	}
	// The same guard on the release path.
	if err := store.ReleaseMail(ctx, db, id, mine.ClaimID, errors.New("stale")); err != nil {
		t.Fatalf("release: %v", err)
	}
	var lastErr sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT last_error FROM mail_outbox WHERE id=$1`, id).Scan(&lastErr); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if lastErr.Valid && lastErr.String == "stale" {
		t.Fatal("the superseded claimant wrote over the new claimant's row")
	}
}

// A leased row is invisible to a concurrent drainer, which is what makes two replicas
// take disjoint work instead of double-sending everything.
func TestALeasedRowIsNotClaimedTwice(t *testing.T) {
	db, ctx := drainerDB(t)
	id := enqueue(t, db, ctx, uniqueRecipient(), "s", "b")

	if _, err := store.ClaimMail(ctx, db, 50, time.Hour); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := store.ClaimMail(ctx, db, 50, time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, m := range second {
		if m.ID == id {
			t.Fatal("a leased row was claimed by a second drainer")
		}
	}
}

// For a password reset the body IS a live credential and the recipient is the fact the
// endpoint refuses to disclose. Neither may reach a log line, on any path — and the
// failure path is where the reflex to "log what went wrong" is strongest.
func TestTheDrainerLogsNothingFromTheMessage(t *testing.T) {
	db, ctx := drainerDB(t)
	const secretToken = "s3cr3t-token-value-do-not-log"
	to := uniqueRecipient()
	enqueue(t, db, ctx, to, "Reset your password", "https://x.test/r?token="+secretToken)

	var captured strings.Builder
	log := slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := mail.NewFake()
	f.FailWith(errors.New("provider refused"))
	d := New(db, f, time.Second, 32, log)
	d.DrainOnce(ctx)

	out := captured.String()
	if out == "" {
		t.Fatal("the failure path logged nothing at all; this test would pass vacuously")
	}
	for _, forbidden := range []string{secretToken, to, "Reset your password"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("the drainer logged %q:\n%s", forbidden, out)
		}
	}
}
