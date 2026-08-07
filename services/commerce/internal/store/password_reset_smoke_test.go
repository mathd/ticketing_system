//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Password recovery against real Postgres (TKT-226, migration 0018).
//
// These are smoke-tagged rather than unit tests on purpose. Every claim this file makes
// — single-use, expiry, same-transaction enqueue, the concurrent-redemption race — is a
// property of the SQL and the driver, and a fake store cannot see either
// (docs/learnings/2026-08-06-a-fake-store-cannot-see-the-driver.md, where three TKT-222
// defects passed a green `make check`).

// composeFor is the message builder the request path takes. Tests use it to capture the
// raw token, which is the only place it exists outside the mail body.
func composeFor(captured *string) func(IssuedResetToken) (string, string, string) {
	return func(tok IssuedResetToken) (string, string, string) {
		if captured != nil {
			*captured = tok.Raw
		}
		return tok.Email, "Reset your password", "link: https://x.test/r?token=" + tok.Raw
	}
}

func seedCustomer(t *testing.T, db *sql.DB, ctx context.Context, password string) (uuid.UUID, string) {
	t.Helper()
	email := uniqueEmail("reset")
	account, err := RegisterCustomer(ctx, db, email, password)
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	return account.ID, email
}

func liveTokenCount(t *testing.T, db *sql.DB, ctx context.Context, customer uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE customer_id=$1 AND used_at IS NULL`,
		customer).Scan(&n); err != nil {
		t.Fatalf("count live tokens: %v", err)
	}
	return n
}

func mailCountFor(t *testing.T, db *sql.DB, ctx context.Context, recipient string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM mail_outbox WHERE recipient=$1`, recipient).Scan(&n); err != nil {
		t.Fatalf("count mail: %v", err)
	}
	return n
}

func TestRequestPasswordResetCommitsOneTokenAndOneMessage(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	ok, err := RequestPasswordReset(ctx, db, email, composeFor(&raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !ok {
		t.Fatal("a registered address must resolve")
	}
	if raw == "" {
		t.Fatal("compose must receive the raw token — it is the only place it exists")
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 1 {
		t.Fatalf("live tokens = %d, want 1", n)
	}
	if n := mailCountFor(t, db, ctx, email); n != 1 {
		t.Fatalf("enqueued messages = %d, want 1", n)
	}
}

// The database must never hold a usable token. This is the one property the SHA-256
// column buys (migration 0018 says so), and it is worth proving rather than assuming:
// a refactor that stored the raw value would break nothing else in this file.
func TestTheRawTokenIsNeverStored(t *testing.T) {
	db, ctx := outboxDB(t)
	_, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}

	var storedHash string
	if err := db.QueryRowContext(ctx,
		`SELECT token_hash FROM password_reset_tokens WHERE token_hash=$1`,
		hashResetToken(raw)).Scan(&storedHash); err != nil {
		t.Fatalf("the token must be findable by its hash: %v", err)
	}
	if storedHash == raw {
		t.Fatal("the stored value equals the raw token; the database holds a usable credential")
	}
	// Not merely "different" — no column of that row may contain the raw token.
	var found int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE token_hash LIKE '%' || $1 || '%'`,
		raw).Scan(&found); err != nil {
		t.Fatalf("scan for the raw token: %v", err)
	}
	if found != 0 {
		t.Fatal("the raw token appears in password_reset_tokens")
	}
}

// ok=false, err=nil, and nothing written. An unknown address is the ordinary case on a
// public endpoint, not an error — and if it wrote anything the two paths would diverge
// in a way an operator (or a timing attacker) could read.
func TestRequestPasswordResetOnAnUnknownAddressWritesNothing(t *testing.T) {
	db, ctx := outboxDB(t)
	unknown := uniqueEmail("nobody")

	var tokensBefore, mailBefore int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM password_reset_tokens`).Scan(&tokensBefore)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM mail_outbox`).Scan(&mailBefore)

	ok, err := RequestPasswordReset(ctx, db, unknown, composeFor(nil))
	if err != nil {
		t.Fatalf("an unknown address is not an error, got %v", err)
	}
	if ok {
		t.Fatal("an unknown address must not resolve")
	}
	var tokensAfter, mailAfter int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM password_reset_tokens`).Scan(&tokensAfter)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM mail_outbox`).Scan(&mailAfter)
	if tokensAfter != tokensBefore || mailAfter != mailBefore {
		t.Fatalf("unknown address wrote rows: tokens %d→%d, mail %d→%d",
			tokensBefore, tokensAfter, mailBefore, mailAfter)
	}
	if n := mailCountFor(t, db, ctx, unknown); n != 0 {
		t.Fatalf("a message was enqueued for an address with no account (%d)", n)
	}
}

// The token and its message commit together or not at all (ADR-016 §Decision 6's shape).
// Forced by a compose that returns a recipient the CHECK refuses: if the two INSERTs were
// not one transaction, the token would survive its undeliverable message.
func TestTheTokenRollsBackWhenItsMessageCannotBeEnqueued(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	_, err := RequestPasswordReset(ctx, db, email, func(tok IssuedResetToken) (string, string, string) {
		return "   ", "Reset your password", "body" // blank recipient: mail_outbox CHECK refuses
	})
	if err == nil {
		t.Fatal("an unenqueueable message must fail the request")
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 0 {
		t.Fatalf("live tokens = %d after a failed enqueue, want 0 — the token outlived its message", n)
	}
}

// Asking twice must not leave two live credentials in two mailboxes. The single-use
// predicate bounds each token; only this bounds their number.
func TestRequestingTwiceInvalidatesTheFirstToken(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var first, second string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&first)); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&second)); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if first == second {
		t.Fatal("two requests minted the same token")
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 1 {
		t.Fatalf("live tokens = %d after two requests, want 1", n)
	}
	if _, err := CompletePasswordReset(ctx, db, first, "a new password"); !errors.Is(err, ErrResetTokenUnusable) {
		t.Fatalf("the superseded token must be unusable, got %v", err)
	}
	if _, err := CompletePasswordReset(ctx, db, second, "a new password"); err != nil {
		t.Fatalf("the current token must work, got %v", err)
	}
}

func TestCompletePasswordResetIsSingleUse(t *testing.T) {
	db, ctx := outboxDB(t)
	_, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := CompletePasswordReset(ctx, db, raw, "first new password"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	_, err := CompletePasswordReset(ctx, db, raw, "second new password")
	if !errors.Is(err, ErrResetTokenUnusable) {
		t.Fatalf("a redeemed token must be unusable, got %v", err)
	}
	// And the second attempt must not have taken effect.
	if _, err := AuthenticateCustomer(ctx, db, email, "first new password"); err != nil {
		t.Fatalf("the first redemption's password must still be the live one: %v", err)
	}
}

// Two concurrent redemptions of one token: exactly one wins. A SELECT-then-UPDATE would
// let both through, and the second's password would silently replace the first's.
func TestConcurrentRedemptionHasExactlyOneWinner(t *testing.T) {
	db, ctx := outboxDB(t)
	_, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = CompletePasswordReset(ctx, db, raw, "racer password")
		}(i)
	}
	close(start)
	wg.Wait()

	var winners int
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrResetTokenUnusable):
		default:
			t.Fatalf("racer %d got an unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

func TestCompletePasswordResetRejectsAnExpiredToken(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Age it past its expiry. The predicate compares against the DATABASE clock, so
	// moving the row's expiry is the honest way to drive this.
	if _, err := db.ExecContext(ctx,
		`UPDATE password_reset_tokens SET expires_at = now() - interval '1 second' WHERE customer_id=$1`,
		customer); err != nil {
		t.Fatalf("age the token: %v", err)
	}
	if _, err := CompletePasswordReset(ctx, db, raw, "a new password"); !errors.Is(err, ErrResetTokenUnusable) {
		t.Fatalf("an expired token must be unusable, got %v", err)
	}
	// The old password must still work: a refused reset changes nothing.
	if _, err := AuthenticateCustomer(ctx, db, email, "correct horse battery"); err != nil {
		t.Fatalf("a refused reset must leave the credential untouched: %v", err)
	}
}

// The whole point of the ticket: the buyer gets back in, and the old password stops
// working. Proved through AuthenticateCustomer — what production calls — rather than by
// reading password_hash, so the test exercises the path a sign-in takes.
func TestCompletePasswordResetReplacesTheCredential(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "the forgotten one")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}
	got, err := CompletePasswordReset(ctx, db, raw, "the remembered one")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got != customer {
		t.Fatalf("completion returned customer %s, want %s — the caller destroys sessions by this id", got, customer)
	}
	if _, err := AuthenticateCustomer(ctx, db, email, "the remembered one"); err != nil {
		t.Fatalf("the new password must authenticate: %v", err)
	}
	if _, err := AuthenticateCustomer(ctx, db, email, "the forgotten one"); !errors.Is(err, ErrCustomerCredentialsInvalid) {
		t.Fatalf("the old password must stop working, got %v", err)
	}
}

// An unusable new password is refused BEFORE the token is spent. Otherwise a buyer who
// pastes something over 72 bytes burns their one-shot link on a request that could never
// have succeeded, and has to start over from a mailbox.
func TestAnUnusablePasswordDoesNotSpendTheToken(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}
	tooLong := strings.Repeat("x", bcryptMaxPasswordBytes+1)
	if _, err := CompletePasswordReset(ctx, db, raw, tooLong); !errors.Is(err, ErrCustomerPasswordUnusable) {
		t.Fatalf("want ErrCustomerPasswordUnusable, got %v", err)
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 1 {
		t.Fatalf("live tokens = %d after a refused password, want 1 — the token was spent", n)
	}
	if _, err := CompletePasswordReset(ctx, db, raw, "a usable password"); err != nil {
		t.Fatalf("the token must still work: %v", err)
	}
}

func TestUnknownAndMalformedTokensAreTheSameRefusal(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, tok := range []string{"", "   ", "not-a-token", strings.Repeat("z", 43)} {
		if _, err := CompletePasswordReset(ctx, db, tok, "a new password"); !errors.Is(err, ErrResetTokenUnusable) {
			t.Fatalf("token %q: want ErrResetTokenUnusable, got %v", tok, err)
		}
	}
}

// Redeeming one token kills the customer's others. A buyer who requested twice and used
// the second link must not leave the first live against a password they just changed.
func TestRedemptionInvalidatesSiblingTokens(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var only string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&only)); err != nil {
		t.Fatalf("request: %v", err)
	}
	// A second live token that did NOT come from the request path — the sibling case
	// only exists if something bypassed the invalidate-on-issue rule, so construct it
	// directly rather than pretending the happy path can produce it.
	sibling := "sibling-token-" + uuid.NewString()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO password_reset_tokens (token_hash, customer_id, expires_at)
		 VALUES ($1, $2, now() + interval '1 hour')`,
		hashResetToken(sibling), customer); err != nil {
		t.Fatalf("seed sibling token: %v", err)
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 2 {
		t.Fatalf("live tokens = %d, want 2 before redemption", n)
	}
	if _, err := CompletePasswordReset(ctx, db, only, "a new password"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 0 {
		t.Fatalf("live tokens = %d after redemption, want 0", n)
	}
	if _, err := CompletePasswordReset(ctx, db, sibling, "another password"); !errors.Is(err, ErrResetTokenUnusable) {
		t.Fatalf("the sibling token must be dead, got %v", err)
	}
}

// One customer's reset must not touch another's tokens or credential.
func TestResetIsScopedToOneCustomer(t *testing.T) {
	db, ctx := outboxDB(t)
	_, alice := seedCustomer(t, db, ctx, "alice password")
	bob, bobEmail := seedCustomer(t, db, ctx, "bob password")

	var bobToken string
	if _, err := RequestPasswordReset(ctx, db, bobEmail, composeFor(&bobToken)); err != nil {
		t.Fatalf("bob request: %v", err)
	}
	var aliceToken string
	if _, err := RequestPasswordReset(ctx, db, alice, composeFor(&aliceToken)); err != nil {
		t.Fatalf("alice request: %v", err)
	}
	if _, err := CompletePasswordReset(ctx, db, aliceToken, "alice new password"); err != nil {
		t.Fatalf("alice complete: %v", err)
	}
	if n := liveTokenCount(t, db, ctx, bob); n != 1 {
		t.Fatalf("bob's live tokens = %d after alice reset, want 1", n)
	}
	if _, err := AuthenticateCustomer(ctx, db, bobEmail, "bob password"); err != nil {
		t.Fatalf("bob's credential must be untouched: %v", err)
	}
}

// ResetTokenTTL must actually reach the row. A constant nothing reads is a constant that
// silently stops meaning anything.
func TestTheStampedExpiryFollowsResetTokenTTL(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	if _, err := RequestPasswordReset(ctx, db, email, composeFor(nil)); err != nil {
		t.Fatalf("request: %v", err)
	}
	var lifetime time.Duration
	var secs float64
	if err := db.QueryRowContext(ctx,
		`SELECT extract(epoch FROM (expires_at - created_at)) FROM password_reset_tokens
		  WHERE customer_id=$1 AND used_at IS NULL`, customer).Scan(&secs); err != nil {
		t.Fatalf("read expiry: %v", err)
	}
	lifetime = time.Duration(secs) * time.Second
	if lifetime != ResetTokenTTL {
		t.Fatalf("stamped lifetime = %s, want ResetTokenTTL (%s)", lifetime, ResetTokenTTL)
	}
}
