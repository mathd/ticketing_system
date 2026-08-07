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
	"github.com/jackc/pgx/v5/pgconn"
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

// `password_reset_tokens` never holds a usable token — and the name of this test says
// exactly that much and no more (ai-review [high]).
//
// It was first written as `TestTheRawTokenIsNeverStored`, which is FALSE: the composed
// message in `mail_outbox` contains the live reset link until the row is sent, and the
// scan below would have passed anyway because it only looks at one table. A test whose
// name claims more than it checks is worse than no test — it is the thing someone cites
// later to argue the credential is not at rest.
//
// The outbox exposure is real, deliberate and documented (ADR-050 §"the adversary",
// development.md, and 0018's own comment): something has to hold the message until it is
// sent. TestTheOutboxDoesHoldTheLiveLink below pins it as a KNOWN state rather than
// leaving it to be discovered.
func TestThePasswordResetTokenTableStoresOnlyAHash(t *testing.T) {
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

// The outbox DOES hold the live link, and this pins it as a known accepted state rather
// than an undiscovered one (ai-review [high]).
//
// If this test ever fails, something started encrypting or externalising the message
// body — which would be an improvement, and the right response is to update ADR-050's
// adversary section and development.md's warning, NOT to delete this test.
func TestTheOutboxDoesHoldTheLiveLinkUntilItIsSent(t *testing.T) {
	db, ctx := outboxDB(t)
	_, email := seedCustomer(t, db, ctx, "correct horse battery")

	var raw string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&raw)); err != nil {
		t.Fatalf("request: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx,
		`SELECT body FROM mail_outbox WHERE recipient=$1 ORDER BY created_at DESC LIMIT 1`,
		email).Scan(&body); err != nil {
		t.Fatalf("read the enqueued message: %v", err)
	}
	if !strings.Contains(body, raw) {
		t.Fatal("the enqueued message does not contain the token — the buyer would receive a dead link")
	}
	// Said out loud: a database reader can redeem this until it expires or is used.
	// That is the documented cost of holding a message before sending it (TKT-33 owns
	// retention).
	if _, err := CompletePasswordReset(ctx, db, raw, "read from the outbox"); err != nil {
		t.Fatalf("a token read out of mail_outbox is redeemable, and that is the point being pinned: %v", err)
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

// Concurrent issuance leaves exactly one live token (ai-review [high]).
//
// TestRequestingTwiceInvalidatesTheFirstToken is SEQUENTIAL and cannot fail on this:
// without the FOR UPDATE in RequestPasswordReset, two transactions both see no live token
// — neither has committed its INSERT — so both invalidate nothing and both insert, and the
// customer ends up with two live links in two mailboxes.
//
// THE OVERLAP IS FORCED, not hoped for (ai-review pass 2 [medium]). A first version just
// released two goroutines from a channel, and pass 2 was right that this only makes them
// ELIGIBLE to run: one can finish and commit before the other starts its lookup, so the
// test could pass against the buggy code. It happened to catch it, which is worse than
// failing — a probabilistic guard on a credential invariant reads as a proof.
//
// The barrier is a row lock held by the test itself. Both implementations — with and
// without FOR UPDATE — must pass through `UPDATE password_reset_tokens … WHERE
// customer_id = $1 AND used_at IS NULL`, so locking a live token row of that customer
// blocks both of them at the same statement. Releasing it lets them proceed genuinely
// interleaved.
func TestConcurrentIssuanceLeavesOneLiveToken(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	// A live token to lock. Seeded directly: the barrier needs a row to exist before
	// either racer runs, which the request path by definition cannot provide.
	barrierToken := "barrier-" + uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO password_reset_tokens (token_hash, customer_id, expires_at)
		VALUES ($1, $2, now() + interval '1 hour')`,
		hashResetToken(barrierToken), customer); err != nil {
		t.Fatalf("seed the barrier token: %v", err)
	}

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.ExecContext(ctx,
		`SELECT token_hash FROM password_reset_tokens WHERE customer_id = $1 AND used_at IS NULL FOR UPDATE`,
		customer); err != nil {
		_ = blocker.Rollback()
		t.Fatalf("hold the barrier: %v", err)
	}

	const racers = 2
	var wg sync.WaitGroup
	tokens := make([]string, racers)
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = RequestPasswordReset(ctx, db, email, composeFor(&tokens[i]))
		}(i)
	}

	// Both racers are now blocked: with the fix, one holds the customer row and waits on
	// the token row while the other waits on the customer row; without it, both wait on
	// the token row. Either way neither can commit until this releases, which is what
	// makes the overlap real rather than likely.
	waitForBlockedOn(t, db, ctx, racers)
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("release the barrier: %v", err)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if n := liveTokenCount(t, db, ctx, customer); n != 1 {
		t.Fatalf("live tokens = %d after %d genuinely overlapped requests, want 1", n, racers)
	}
	// And exactly one of the minted tokens is the live one, so a buyer opening the earlier
	// mail gets the ordinary refusal rather than a working link to an account whose
	// password someone else may already be changing.
	var usable int
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		if _, err := CompletePasswordReset(ctx, db, tok, "a new password"); err == nil {
			usable++
		} else if !errors.Is(err, ErrResetTokenUnusable) {
			t.Fatalf("unexpected error redeeming a raced token: %v", err)
		}
	}
	if usable != 1 {
		t.Fatalf("%d of the concurrently minted tokens were redeemable, want exactly 1", usable)
	}
}

// waitForBlockedOn blocks until `want` backends are waiting on a lock, so the barrier is
// released only once every racer has genuinely arrived at it. Polling pg_stat_activity is
// what turns "probably overlapped" into "observed overlapped"; a sleep would just move the
// guess somewhere less visible.
func waitForBlockedOn(t *testing.T, db *sql.DB, ctx context.Context, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND state = 'active'`).
			Scan(&blocked); err != nil {
			t.Fatalf("inspect lock waits: %v", err)
		}
		if blocked >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("only saw fewer than %d backends blocked on a lock; the barrier never engaged and this test would prove nothing", want)
}

// Issuance and redemption must take their locks in the SAME order (ai-review pass 2
// [high]). This is a regression test for a deadlock the FIRST fix introduced.
//
// Issuance locks the customer row and then updates that customer's token rows. Redemption
// originally did the reverse — redeemed the token, then updated customer_accounts — so a
// customer with a live token could have a request and a redemption form a cycle: issuance
// holding the customer and waiting for the token, redemption holding the token and waiting
// for the customer. Postgres breaks the cycle by aborting one, which is a 500 on a recovery
// path.
//
// FORCING IT, rather than looping and hoping: the test holds the customer row, starts
// issuance (which blocks on it), waits until that is observed, then starts redemption
// (which under the old order grabs the token first, then queues behind issuance for the
// customer). Releasing lets issuance take the customer and reach for a token redemption
// already holds — the cycle, deterministically. Under the corrected order redemption holds
// nothing while it waits, so there is no cycle to form and it simply finds the token
// invalidated.
func TestIssuanceAndRedemptionDoNotDeadlock(t *testing.T) {
	db, ctx := outboxDB(t)
	customer, email := seedCustomer(t, db, ctx, "correct horse battery")

	var live string
	if _, err := RequestPasswordReset(ctx, db, email, composeFor(&live)); err != nil {
		t.Fatalf("seed a live token: %v", err)
	}

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.ExecContext(ctx,
		`SELECT id FROM customer_accounts WHERE id = $1 FOR UPDATE`, customer); err != nil {
		_ = blocker.Rollback()
		t.Fatalf("hold the customer row: %v", err)
	}

	var wg sync.WaitGroup
	var issueErr, redeemErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, issueErr = RequestPasswordReset(ctx, db, email, composeFor(nil))
	}()
	// Issuance must be queued for the customer row BEFORE redemption starts, or the
	// waiters line up in the order that cannot deadlock and the test proves nothing.
	waitForBlockedOn(t, db, ctx, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, redeemErr = CompletePasswordReset(ctx, db, live, "a new password")
	}()
	waitForBlockedOn(t, db, ctx, 2)

	if err := blocker.Rollback(); err != nil {
		t.Fatalf("release the barrier: %v", err)
	}
	wg.Wait()

	// SQLSTATE 40P01. Neither operation may report it — a deadlock here is a 500 for a
	// buyer who is trying to get back into their account.
	for label, err := range map[string]error{"issuance": issueErr, "redemption": redeemErr} {
		if err == nil {
			continue
		}
		if errors.Is(err, ErrResetTokenUnusable) {
			// Redemption losing its token to a concurrent issuance is the ORDINARY
			// outcome of this interleaving, not a failure.
			continue
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.SQLState() == "40P01" {
			t.Fatalf("%s deadlocked: issuance and redemption take their locks in different orders", label)
		}
		t.Fatalf("%s failed unexpectedly: %v", label, err)
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
