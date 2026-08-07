package store

// The mail outbox's claim protocol (TKT-226 / ADR-050).
//
// This is completion_outbox's protocol (store.go: ClaimOutbox / MarkPublished /
// ReleaseOutbox) applied to a different table, and the duplication is deliberate:
// those functions name `completion_outbox` in their SQL and return an order-shaped
// row, so sharing them means a row-shape abstraction over two tables that buys nothing
// today. Two instances do not earn one; a third would. Recorded in ADR-050 so a
// reviewer files this as a decision rather than as copy-paste.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MaxMailAttempts bounds retries before a message is quarantined. Ten, matching
// MaxOutboxAttempts: with the same exponential backoff that is roughly forty minutes of
// trying, which outlasts an ordinary provider blip and is well inside ResetTokenTTL —
// a message that has not left in an hour is delivering a link that has already expired.
const MaxMailAttempts = 10

// OutboundMessage is one claimed row. It carries the recipient and the composed body,
// which is PII and, for a reset, a live credential. Nothing here may be logged.
type OutboundMessage struct {
	ID        uuid.UUID
	Recipient string
	Subject   string
	Body      string
	Attempts  int
	ClaimID   uuid.UUID
}

// ClaimMail leases up to limit sendable messages, oldest first. SKIP LOCKED lets
// concurrent drainers take disjoint work without blocking each other.
func ClaimMail(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]OutboundMessage, error) {
	claim := uuid.New()
	rows, err := db.QueryContext(ctx, `
		UPDATE mail_outbox
		SET lease_until=now()+make_interval(secs => $2), claim_id=$3, attempts=attempts+1
		WHERE id IN (
			SELECT id FROM mail_outbox
			WHERE sent_at IS NULL
			  AND dead_lettered_at IS NULL
			  AND next_attempt_at<=now()
			  AND (lease_until IS NULL OR lease_until<=now())
			ORDER BY next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		RETURNING id,recipient,subject,body,attempts,claim_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, fmt.Errorf("claim mail outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OutboundMessage
	for rows.Next() {
		var m OutboundMessage
		if err := rows.Scan(&m.ID, &m.Recipient, &m.Subject, &m.Body, &m.Attempts, &m.ClaimID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMailSent retires a message, but only if the caller still holds the claim.
// Conditional on claim_id: a claimant whose lease lapsed mid-send must not retire a row
// another drainer has since claimed, or it would mask a send that never happened.
// Returns false when the claim was lost.
func MarkMailSent(ctx context.Context, db OutboxDB, id, claimID uuid.UUID) (bool, error) {
	result, err := db.ExecContext(ctx, `
		UPDATE mail_outbox SET sent_at=now(),lease_until=NULL,claim_id=NULL,last_error=NULL
		WHERE id=$1 AND claim_id=$2 AND sent_at IS NULL`, id, claimID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ReleaseMail returns a message to the claimable set after a failed send, backing off
// before it may be retried and quarantining it once attempts are exhausted.
//
// `cause.Error()` is written to last_error. That string comes from the sender, so a
// Sender implementation that puts the recipient or the message body into its error text
// would write a credential into a column an operator greps — the port's contract is
// that it does not, and shared/go/mail's fake and validator carry nothing from Message
// into their errors.
func ReleaseMail(ctx context.Context, db OutboxDB, id, claimID uuid.UUID, cause error) error {
	_, err := db.ExecContext(ctx, `
		UPDATE mail_outbox
		SET lease_until=NULL,
		    claim_id=NULL,
		    last_error=$3,
		    -- Exponential, capped: 2^attempts seconds up to 5 minutes.
		    next_attempt_at=now() + least(make_interval(secs => power(2, least(attempts, 8))::double precision), interval '5 minutes'),
		    dead_lettered_at=CASE WHEN attempts>=$4 THEN now() ELSE NULL END
		WHERE id=$1 AND claim_id=$2 AND sent_at IS NULL`,
		id, claimID, cause.Error(), MaxMailAttempts)
	return err
}
