package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// --- detaching a claimed order (TKT-225 / ADR-052) ---

// ErrOrderNotDetachable is the ONE refusal this operation reports.
//
// Unlike ClaimGuestOrder's single refusal, this is not about hiding an oracle:
// the caller already holds the service credential, so "does this order exist" is
// not a secret from them. It is one error because the three cases — no such
// order, not completed, already unattributed — need the same operator response:
// look at the order, this is not a thing to detach. A NULL attribution in
// particular is not a failure to report loudly; it is the state the caller wanted.
var ErrOrderNotDetachable = errors.New("order is not detachable")

// ErrDetachmentNotDescribed refuses a detach that records nothing about itself.
//
// Separate from ErrOrderNotDetachable because it is a CALLER bug, not a statement
// about the order: the operator sent a blank reason or actor. Telling them apart
// is what lets the handler answer 400 rather than 404.
var ErrDetachmentNotDescribed = errors.New("detachment needs a non-empty actor and reason")

// detachOrderStatement is the SECOND of exactly two production statements
// permitted to write orders.customer_id outside claimOrder's INSERT.
//
// Kept as a const for the same reason claimGuestOrderStatement is:
// attribution_invariant_test.go allowlists both BY TEXT and asserts the count is
// exactly two. A third is not covered by anything that reviewed these two.
//
// Why a detach may do what recovery must not, in the terms the claim's own comment
// uses: recovery and checkout replay are not ownership operations and must leave
// attribution as they found it. A claim IS the ownership operation and is the only
// NULL -> customer transition; this is the only customer -> NULL one. Together
// they are the whole of attribution's mutable life, and the predicates are what
// keep each narrow.
//
// `customer_id IS NOT NULL` is not redundant defensive coding. Without it,
// detaching an already-unattributed order reports success and the caller writes an
// audit row describing a detachment that did not happen — a false record in the
// one table whose entire purpose is being true.
//
// It never repoints: SET is a literal NULL, so no caller-supplied customer can
// reach this column through this statement. That is what keeps a detach from
// becoming a transfer (TKT-9 / TKT-160), which has a different adversary and needs
// a different argument.
//
// `RETURNING OLD.customer_id`, and the OLD is load-bearing.
//
// In an UPDATE's RETURNING clause, a column name is the NEW value — and so is a
// TABLE-QUALIFIED one. `RETURNING customer_id` and `RETURNING orders.customer_id`
// both come back NULL here, which would make every audit row record a detachment
// from nobody. Verified against the real engine rather than assumed: the first
// version of this statement shipped the bare form, and the second the qualified
// form, and both failed the same way.
//
// `OLD.column` is PostgreSQL **18**, which is what this stack runs
// (compose.yaml pins postgres:18.4). On an older server this statement is a syntax
// error at first execution rather than a silent wrong answer — a loud failure, and
// the smoke suite runs against the pinned image, so a downgrade cannot pass
// unnoticed.
//
// It reads from the same locked row in the same statement, rather than from a
// preflight SELECT that a concurrent claim could invalidate between read and write.
//
// updated_at is deliberately NOT touched, matching the claim: it means "when did
// this order's CHECKOUT last move" and recovery reads it to decide what is stale.
// A support action months later is not checkout activity.
const detachOrderStatement = `
	UPDATE orders
	   SET customer_id = NULL
	 WHERE id = $1
	   AND status = 'completed'
	   AND customer_id IS NOT NULL
	 RETURNING OLD.customer_id`

// DetachOrderAttribution restores a claimed order to unattributed, and records
// who did it and why.
//
// Both in ONE transaction, deliberately: the attribution change is the effect and
// the audit row is the evidence, and a world in which one exists without the other
// is worse than either failing. A detach with no record is the untraceable
// purchase-mover this operation exists not to be; a record with no detach is a
// false accusation.
//
// The order row is locked by the UPDATE itself rather than by a preflight
// SELECT ... FOR UPDATE. Under READ COMMITTED a concurrent claim either commits
// first — in which case this statement re-evaluates against its version and
// detaches the new owner, which is correct and recorded — or blocks and then finds
// customer_id already NULL and refuses. Neither order produces a wrong audit row.
//
// ADR-021, the adversary: this refuses an HTTP caller who does not hold the
// service credential. It constrains nobody with database access, and the audit row
// it writes is ordinary commerce state that such a writer can alter or delete.
// Accountability against carelessness, not tamper evidence.
// Replay is by KEY, and it is what makes a retry safe (ai-review [high]).
//
// Because a detached order is immediately re-claimable (ADR-052 § 4), a detach is
// NOT naturally idempotent: detach A, lose the response, B claims the now-free
// order, retry the identical request — and without a key the retry detaches B, a
// customer the operator never reviewed, recorded under the reason they wrote about
// A. Retry timing would decide who loses their purchase.
//
// So the key is looked up FIRST, inside the same transaction that would do the
// work. A hit returns the customer the original call detached and touches nothing.
func DetachOrderAttribution(ctx context.Context, db *sql.DB, order uuid.UUID, key, actor, reason string) (uuid.UUID, error) {
	key, actor, reason = strings.TrimSpace(key), strings.TrimSpace(actor), strings.TrimSpace(reason)
	// Checked before the transaction opens: a blank field can never succeed, and
	// the database CHECK constraints would refuse it anyway. Failing here means the
	// refusal is the same whether or not the order exists, so a caller cannot use a
	// deliberately blank reason to probe for orders.
	if key == "" || actor == "" || reason == "" {
		return uuid.Nil, ErrDetachmentNotDescribed
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin detach: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The replay check, and it is the FAST path only — not the authority.
	//
	// Under READ COMMITTED two concurrent FIRST attempts with the same key both
	// read no row here and both proceed; the unique index is what actually
	// serializes them, and the loser is handled at the insert below. Saying this
	// read "cannot be raced" would be wrong, and the difference is a 500 for the
	// caller instead of a replay.
	if replayed, found, err := lookupDetachment(ctx, tx, order, key); err != nil {
		return uuid.Nil, err
	} else if found {
		return replayed, nil
	}

	var detached uuid.UUID
	switch err := tx.QueryRowContext(ctx, detachOrderStatement, order).Scan(&detached); {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing to detach — but that may be because a concurrent attempt with THIS
		// key already did it (ai-review pass 2 [high]). Both transactions can miss
		// the replay read above under READ COMMITTED; the winner then commits, and
		// this UPDATE re-evaluates against a row that is already NULL. Answering 404
		// here would tell the caller their operation failed when their key's
		// operation succeeded.
		//
		// Re-checked on a fresh connection, because this transaction's snapshot
		// predates the winner's commit and would miss the row again.
		//
		// **This branch is NOT covered by a test, and no sequential test can cover
		// it** (ai-review pass 3 [medium], honestly). Reaching it requires two
		// transactions to both pass the replay read before either commits; a
		// sequential retry takes the fast path above and never arrives here.
		// Deleting this branch leaves the whole gate green — verified twice. The
		// unique-violation branch below has the same property. They are written
		// from the READ COMMITTED semantics rather than from a red test, and that
		// is a weaker warrant than everything else in this file.
		_ = tx.Rollback()
		if winner, found, lookupErr := lookupDetachment(ctx, db, order, key); lookupErr != nil {
			return uuid.Nil, lookupErr
		} else if found {
			return winner, nil
		}
		return uuid.Nil, ErrOrderNotDetachable
	case err != nil:
		return uuid.Nil, fmt.Errorf("detach order attribution: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_attribution_detachments(id, idempotency_key, order_id, customer_id, reason, actor)
		VALUES($1, $2, $3, $4, $5, $6)`,
		uuid.New(), key, order, detached, reason, actor); err != nil {
		if !isUniqueViolation(err) {
			return uuid.Nil, fmt.Errorf("record detachment: %w", err)
		}
		// Lost a race with a concurrent FIRST attempt carrying the same key. The
		// winner already detached and recorded it; this transaction rolls back —
		// including its own UPDATE — and reports what the winner did, which is
		// exactly what a replay reports. Read on a fresh connection because this
		// transaction is now aborted and can serve no further queries.
		_ = tx.Rollback()
		winner, found, lookupErr := lookupDetachment(ctx, db, order, key)
		switch {
		case lookupErr != nil:
			return uuid.Nil, lookupErr
		case !found:
			// The unique index fired but no row is visible: the winner has not
			// committed yet. Report the collision rather than inventing an answer —
			// a retry resolves it, and a wrong customer id here would be recorded
			// by the caller as fact.
			return uuid.Nil, fmt.Errorf("record detachment: concurrent detachment with the same key is still in flight")
		}
		return winner, nil
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit detach: %w", err)
	}
	return detached, nil
}

// lookupDetachment reads a previous detachment by key. Takes any querier so the
// replay fast-path can use the open transaction and the unique-violation recovery
// can use a fresh connection — the aborted transaction can serve neither.
func lookupDetachment(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, order uuid.UUID, key string) (uuid.UUID, bool, error) {
	var customer uuid.UUID
	switch err := q.QueryRowContext(ctx, `
		SELECT customer_id FROM order_attribution_detachments
		 WHERE order_id = $1 AND idempotency_key = $2`, order, key).Scan(&customer); {
	case err == nil:
		return customer, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, false, nil
	default:
		return uuid.Nil, false, fmt.Errorf("look up detachment: %w", err)
	}
}
