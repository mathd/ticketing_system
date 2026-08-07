package store

// Password recovery (TKT-226). See ADR-050 for the mail path and ADR-049 § TKT-226
// amendment for what this changes about customer identity.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrResetTokenUnusable is the ONE refusal for a redemption. Unknown, expired and
// already-used are the same error by construction — one sentinel, so no caller can
// branch them apart and tell a prober which tokens were ever real.
//
// This mirrors ErrCustomerCredentialsInvalid's reasoning exactly (ADR-049 §3): two
// call sites constructing "the same" refusal are two call sites one of which
// eventually says something slightly different.
var ErrResetTokenUnusable = errors.New("password reset token unusable")

// ResetTokenTTL is how long a mailed link works.
//
// One hour, and it is a real choice rather than an inherited one. The session TTL is
// eight hours because it bounds a working visit (ADR-049 §4); a reset link bounds the
// gap between asking for mail and reading it, which is minutes for anyone actually
// locked out. The cost of it being short is a second request, which is free. The cost
// of it being long is a live credential sitting in a mailbox.
const ResetTokenTTL = time.Hour

// resetTokenBytes is the raw token's entropy. 32 bytes = 256 bits, the same size as the
// storefront's session token, and the reason password_reset_tokens stores a plain
// SHA-256 rather than a KDF: there is no dictionary to slow down.
const resetTokenBytes = 32

// hashResetToken is the stored form. SHA-256, hex, lower-case — pinned by the
// migration's CHECK, so a change in encoding fails loudly at the constraint rather than
// quietly never matching.
func hashResetToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newResetToken mints the raw credential. base64url so it survives a URL query
// parameter untouched — no percent-encoding, which is what makes a mailed link
// copy-pasteable and what stops a client library from re-encoding it into a value the
// hash no longer matches.
func newResetToken() (string, error) {
	b := make([]byte, resetTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint reset token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssuedResetToken is what the request path needs to compose a mail. It carries the RAW
// token, which is the only moment it exists in this process: it is written to the
// message body and then discarded, and nothing persists it.
type IssuedResetToken struct {
	Raw   string
	Email string
}

// RequestPasswordReset mints a token for an address and enqueues its message, in ONE
// transaction, and reports whether the address resolved.
//
// `ok == false` is NOT an error. An unknown address is the ordinary case on a public
// endpoint and the caller must answer identically either way; making it an error would
// invite a handler to map it to a distinguishable status, which is the oracle AC-5
// forbids.
//
// compose builds the message from the raw token. It is a parameter rather than inlined
// because the copy is locale-dependent and lives in the API layer, and because this
// function must not know what a password reset email says.
//
// EVERY OUTSTANDING TOKEN FOR THE CUSTOMER IS INVALIDATED FIRST. Without it, asking for
// three resets leaves three live credentials in three mailboxes, and the buyer who
// re-requested because the first "didn't arrive" has widened the window rather than
// narrowed it. The single-use predicate bounds each token; only this bounds their
// number.
func RequestPasswordReset(
	ctx context.Context,
	db *sql.DB,
	email string,
	compose func(IssuedResetToken) (recipient, subject, body string),
) (ok bool, err error) {
	key := customerEmailKey(email)
	if key == "" {
		return false, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin password reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE, and it is load-bearing rather than defensive (ai-review [high]).
	//
	// Without it, two concurrent requests for the same address BOTH see no live token
	// — neither has committed its INSERT yet — so both invalidate nothing, both insert,
	// and the customer ends up with two live tokens in two mailboxes. The "issuing
	// invalidates the outstanding ones" rule below would silently be a rule about
	// sequential requests only, and the test that covers it is sequential.
	//
	// The CUSTOMER row is the right lock identity: both transactions resolve the same
	// account, so the second blocks here, and once it proceeds the invalidating UPDATE
	// sees the first's committed token. This is NOT ADR-029's trap, where a row lock
	// fails because the conflicting write INSERTs a different row — there, the locked
	// row was not the one both writers contended on. Here it is.
	var customerID uuid.UUID
	var display string
	err = tx.QueryRowContext(ctx, `
		SELECT id, email FROM customer_accounts WHERE email_key = $1 FOR UPDATE`, key).
		Scan(&customerID, &display)
	if errors.Is(err, sql.ErrNoRows) {
		// No account. Commit nothing, report nothing, and let the caller answer
		// exactly as it would have for a real address.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve customer for reset: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE password_reset_tokens SET used_at = now()
		WHERE customer_id = $1 AND used_at IS NULL`, customerID); err != nil {
		return false, fmt.Errorf("invalidate outstanding reset tokens: %w", err)
	}

	raw, err := newResetToken()
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO password_reset_tokens (token_hash, customer_id, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))`,
		hashResetToken(raw), customerID, ResetTokenTTL.Seconds()); err != nil {
		return false, fmt.Errorf("insert reset token: %w", err)
	}

	recipient, subject, body := compose(IssuedResetToken{Raw: raw, Email: display})
	// The mail row shares this transaction, which is the whole point (ADR-016
	// §Decision 6's shape): a token can never exist without an owed message, and a
	// message can never be owed for a token that was rolled back.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO mail_outbox (recipient, subject, body) VALUES ($1, $2, $3)`,
		recipient, subject, body); err != nil {
		return false, fmt.Errorf("enqueue reset mail: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit password reset: %w", err)
	}
	return true, nil
}

// CompletePasswordReset redeems a token and sets a new password, returning the customer
// whose sessions the caller must now destroy.
//
// ONE statement does the redemption. A SELECT-then-UPDATE is a race two concurrent
// redemptions both win — and under this table's predicate that means two different new
// passwords, the second silently overwriting the first. The conditional UPDATE makes
// the loser see zero rows and refuse, which is the ordinary answer.
func CompletePasswordReset(ctx context.Context, db *sql.DB, rawToken, newPassword string) (uuid.UUID, error) {
	// Refuse an unusable password BEFORE touching the token, so a caller who submits a
	// 200-byte password does not burn their one-shot credential on a request that was
	// never going to succeed. hashCustomerPassword owns the rules (empty, >72 bytes).
	hash, err := hashCustomerPassword(newPassword)
	if err != nil {
		return uuid.Nil, err
	}
	// A syntactically impossible token never reaches the database. It cannot match the
	// PRIMARY KEY's CHECK anyway; refusing here keeps a malformed input from being a
	// cheaper or more expensive answer than a well-formed miss.
	if strings.TrimSpace(rawToken) == "" {
		return uuid.Nil, ErrResetTokenUnusable
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin reset completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Redeem and resolve in one conditional statement. `used_at IS NULL` is what makes
	// it single-use; `expires_at > now()` uses the DATABASE clock, never the caller's.
	var customerID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE password_reset_tokens
		   SET used_at = now()
		 WHERE token_hash = $1
		   AND used_at IS NULL
		   AND expires_at > now()
		RETURNING customer_id`, hashResetToken(rawToken)).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrResetTokenUnusable
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("redeem reset token: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE customer_accounts SET password_hash = $2 WHERE id = $1`,
		customerID, hash); err != nil {
		return uuid.Nil, fmt.Errorf("set new password: %w", err)
	}

	// Any OTHER live token for this customer dies with the redemption. A buyer who
	// requested twice and used the second link must not leave the first one live in a
	// mailbox against a password they have just changed.
	if _, err = tx.ExecContext(ctx, `
		UPDATE password_reset_tokens SET used_at = now()
		WHERE customer_id = $1 AND used_at IS NULL`, customerID); err != nil {
		return uuid.Nil, fmt.Errorf("invalidate sibling reset tokens: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit reset completion: %w", err)
	}
	return customerID, nil
}
