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
func DetachOrderAttribution(ctx context.Context, db *sql.DB, order uuid.UUID, actor, reason string) (uuid.UUID, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	// Checked before the transaction opens: a blank actor or reason can never
	// succeed, and the database CHECK constraints would refuse it anyway. Failing
	// here means the refusal is the same whether or not the order exists, so a
	// caller cannot use a deliberately blank reason to probe for orders.
	if actor == "" || reason == "" {
		return uuid.Nil, ErrDetachmentNotDescribed
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin detach: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var detached uuid.UUID
	switch err := tx.QueryRowContext(ctx, detachOrderStatement, order).Scan(&detached); {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, ErrOrderNotDetachable
	case err != nil:
		return uuid.Nil, fmt.Errorf("detach order attribution: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_attribution_detachments(id, order_id, customer_id, reason, actor)
		VALUES($1, $2, $3, $4, $5)`,
		uuid.New(), order, detached, reason, actor); err != nil {
		return uuid.Nil, fmt.Errorf("record detachment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit detach: %w", err)
	}
	return detached, nil
}
