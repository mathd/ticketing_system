package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Operator outcomes for failed post-purchase refunds (TKT-525, ADR-087).
//
// A staff or bulk refund whose provider refund failed stays pending, with both allowances
// reserved. An operator records what was established outside the platform as an append-only
// outcome. This file is the Commerce half of `refunded_outside`: the outcome becomes the
// refund's completion evidence, in place of a payments fact.
//
// What it deliberately does NOT do (D15, D16): it appends no Commerce `order.refunded` fact
// and forwards nothing to the payments journal, and it does not touch CompleteOrderRefund or
// the ordinary fact path. Consumers of the payments journal or of order_facts must read
// refund_outcomes for outside refunds.

const (
	// OutcomeRefundedOutside: the buyer received the full stored amount by a route outside
	// the platform. The refund completes and its reversal runs.
	OutcomeRefundedOutside = "refunded_outside"
	// OutcomeAbandoned: nobody will refund this attempt. The refund stays pending (S2).
	OutcomeAbandoned = "abandoned"

	refundOutcomeSubject = "order_refund"
)

var (
	// ErrRefundOutcomeNotFound reports a refund that does not exist in the selected organizer,
	// including one whose source reservation belongs to another organizer. A foreign refund and
	// a missing one get the same answer, so the command cannot be used to probe other tenants.
	ErrRefundOutcomeNotFound = errors.New("refund not found for this organizer")
	// ErrRefundOutcomeConflict reports an outcome already recorded for the refund with a
	// different kind, operator name, reason or external reference.
	ErrRefundOutcomeConflict = errors.New("a different outcome is already recorded for this refund")
	// ErrRefundOutcomeIneligible reports a refund that payments already completed. Its money
	// moved through the platform; there is nothing for an operator to attest.
	ErrRefundOutcomeIneligible = errors.New("refund already completed through payments")
	// ErrRefundOutcomeStale reports that the refund or its order changed between the scoped
	// inspection and the locked write. Nothing was written; run the command again.
	ErrRefundOutcomeStale = errors.New("refund changed since it was inspected")
)

// RefundOutcomeRequest is one operator command. Amount and currency are NOT here: they are
// copied from the refund row, so an outcome always covers the whole stored refund.
type RefundOutcomeRequest struct {
	OrganizerID, RefundID uuid.UUID
	Kind                  string
	OperatorName          string
	// ExternalRef is the outside transfer reference for refunded_outside. It is empty for
	// abandoned, whose reference comes from payments.
	ExternalRef string
	Reason      string
}

// Validate checks a request before anything is opened. The bounds match refund_outcomes'
// CHECK constraints, so a request that passes cannot be refused by the database for its shape.
func (r RefundOutcomeRequest) Validate() error {
	if r.OrganizerID == uuid.Nil {
		return errors.New("organizer id is required")
	}
	if r.RefundID == uuid.Nil {
		return errors.New("refund id is required")
	}
	switch r.Kind {
	case OutcomeRefundedOutside:
		if err := boundedText("external reference", r.ExternalRef, 200); err != nil {
			return err
		}
	case OutcomeAbandoned:
		if r.ExternalRef != "" {
			return errors.New("an abandoned outcome takes no external reference: payments supplies the provider's")
		}
	default:
		return fmt.Errorf("outcome kind must be %s or %s", OutcomeRefundedOutside, OutcomeAbandoned)
	}
	if err := boundedText("operator name", r.OperatorName, 200); err != nil {
		return err
	}
	return boundedText("reason", r.Reason, 500)
}

func boundedText(name, v string, max int) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(v) > max {
		return fmt.Errorf("%s must be at most %d bytes", name, max)
	}
	return nil
}

// RefundOutcome is one recorded operator outcome.
type RefundOutcome struct {
	OrganizerID  uuid.UUID
	RefundID     uuid.UUID
	Kind         string
	Amount       int64
	Currency     string
	ExternalRef  string
	OperatorName string
	// SessionUser is PostgreSQL's session_user on the connection that wrote the row. On the
	// documented access path it is the shared `commerce` login; OperatorName is the only
	// personal attribution (ADR-087).
	SessionUser string
	Reason      string
	RecordedAt  time.Time
}

// matches reports whether a repeated command is the same command. Amount and currency are
// not compared: the operator never supplies them. Neither is an abandoned outcome's external
// reference: payments supplies it, so it is not part of the command.
func (o RefundOutcome) matches(in RefundOutcomeRequest) bool {
	if o.Kind != in.Kind || o.OperatorName != in.OperatorName || o.Reason != in.Reason {
		return false
	}
	return in.Kind == OutcomeAbandoned || o.ExternalRef == in.ExternalRef
}

// RefundOutcomeTarget is what the scoped inspection read, before any payments call. The
// locked write rechecks it.
type RefundOutcomeTarget struct {
	OrganizerID, RefundID, OrderID uuid.UUID
	Quantity                       int32
	Amount                         int64
	Currency                       string
	Status                         string
	// ChargeKey is the order's checkout idempotency key: the key payments bound the charge
	// under, and the source key of the refund's leg.
	ChargeKey string
	// Outcome is the outcome already recorded for this refund, or nil.
	Outcome *RefundOutcome
}

// Completed reports a refund already completed, by either evidence source.
func (t RefundOutcomeTarget) Completed() bool { return t.Status == "completed" }

// Replay decides a repeated command against an existing outcome without any payments call:
// the original record when it is the same command, a conflict otherwise. found=false when no
// outcome exists yet.
func (t RefundOutcomeTarget) Replay(in RefundOutcomeRequest) (RefundOutcome, bool, error) {
	if t.Outcome == nil {
		return RefundOutcome{}, false, nil
	}
	if !t.Outcome.matches(in) {
		return RefundOutcome{}, true, ErrRefundOutcomeConflict
	}
	return *t.Outcome, true, nil
}

// InspectRefundOutcomeTarget reads one refund, its order's charge key, and any recorded
// outcome, scoped to the organizer on BOTH the refund row and its source reservation. It holds
// no lock; the caller makes its payments call after this and before RecordRefundedOutside.
func InspectRefundOutcomeTarget(ctx context.Context, db *sql.DB, org, refundID uuid.UUID) (RefundOutcomeTarget, error) {
	t := RefundOutcomeTarget{OrganizerID: org, RefundID: refundID}
	err := db.QueryRowContext(ctx, `
		SELECT rf.order_id, rf.quantity, rf.amount, rf.currency, rf.status, o.idempotency_key
		FROM order_refunds rf
		JOIN orders o ON o.id = rf.order_id
		JOIN reservations r ON r.id = o.reservation_id
		WHERE rf.organizer_id=$1 AND rf.id=$2 AND r.organizer_id=$1`, org, refundID).
		Scan(&t.OrderID, &t.Quantity, &t.Amount, &t.Currency, &t.Status, &t.ChargeKey)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundOutcomeTarget{}, ErrRefundOutcomeNotFound
	}
	if err != nil {
		return RefundOutcomeTarget{}, err
	}
	o, found, err := lookupRefundOutcome(ctx, db, org, refundID)
	if err != nil {
		return RefundOutcomeTarget{}, err
	}
	if found {
		t.Outcome = &o
	}
	return t, nil
}

func lookupRefundOutcome(ctx context.Context, q rowQuerier, org, refundID uuid.UUID) (RefundOutcome, bool, error) {
	o := RefundOutcome{OrganizerID: org, RefundID: refundID}
	err := q.QueryRowContext(ctx, `
		SELECT kind, amount, currency, external_ref, operator_name, session_user_name, reason, recorded_at
		FROM refund_outcomes WHERE organizer_id=$1 AND subject_kind='order_refund' AND subject_id=$2`, org, refundID).
		Scan(&o.Kind, &o.Amount, &o.Currency, &o.ExternalRef, &o.OperatorName, &o.SessionUser, &o.Reason, &o.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundOutcome{}, false, nil
	}
	if err != nil {
		return RefundOutcome{}, false, err
	}
	o.RecordedAt = o.RecordedAt.UTC()
	return o, true, nil
}

// outsideOrderLockSQL locks the refund's order, scoped through its source reservation. It is
// a named constant so the lock-order test can observe this exact statement waiting.
const outsideOrderLockSQL = `SELECT o.idempotency_key, r.organizer_id, r.quantity FROM orders o JOIN reservations r ON r.id = o.reservation_id WHERE o.id=$1 FOR UPDATE OF o`

// RecordRefundedOutside appends a refunded_outside outcome and completes the refund with it as
// evidence, in ONE short transaction. The caller has already inspected the refund and obtained
// payments' read-only proof that the refund's leg is still bound and its provider refund failed
// (D13); no lock was held across that call.
//
// Lock order is the one CompleteOrderRefund uses: the refund row, then its order (D16). Taking
// the order first would invert it against every ordinary completion.
//
// Under those locks it rechecks everything the proof relied on: the source reservation still
// belongs to the organizer, and the order's charge key and the refund's order, amount and
// currency are what was inspected. Then it decides replay or conflict against any outcome that
// committed in the meantime, refuses an ordinarily completed refund, and writes.
//
// replay=true returns the ORIGINAL record, with its time and attribution, and writes nothing.
func RecordRefundedOutside(ctx context.Context, db *sql.DB, in RefundOutcomeRequest, inspected RefundOutcomeTarget) (RefundOutcome, bool, error) {
	if in.Kind != OutcomeRefundedOutside {
		return RefundOutcome{}, false, fmt.Errorf("RecordRefundedOutside takes kind %s, got %q", OutcomeRefundedOutside, in.Kind)
	}
	if err := in.Validate(); err != nil {
		return RefundOutcome{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	locked, existing, err := lockOutcomeTarget(ctx, tx, in, inspected)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if existing != nil {
		return *existing, true, tx.Commit()
	}
	out, err := appendRefundOutcome(ctx, tx, in, locked, in.ExternalRef)
	if err != nil {
		return RefundOutcome{}, false, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE order_refunds SET status='completed', completed_at=now(), outside_completion_kind=$3
		WHERE organizer_id=$1 AND id=$2 AND status='pending'`, in.OrganizerID, in.RefundID, OutcomeRefundedOutside)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return RefundOutcome{}, false, fmt.Errorf("complete refund outside: %d rows, err %v", n, err)
	}
	if err := advanceOrderRefundProjection(ctx, tx, locked.order, locked.quantity, locked.amount, locked.sold); err != nil {
		return RefundOutcome{}, false, err
	}
	return out, false, tx.Commit()
}

// RecordAbandoned appends an `abandoned` outcome (TKT-525 S2, ADR-087), in ONE short
// transaction, after payments has committed its abandonment marker for the refund's leg and
// returned the provider's reference for the failed refund. That reference becomes the
// outcome's external reference; the operator never types it.
//
// It takes the same locks and rechecks as RecordRefundedOutside and writes ONLY the outcome.
// The refund stays pending; the order projection, the tickets and the capacity keep their
// state. The quantity allowance is released because BindOrderRefund's sum excludes a refund
// with a matching abandoned outcome, so the release happens exactly when this commits.
//
// replay=true returns the ORIGINAL record, with its time, attribution and reference, and
// writes nothing.
func RecordAbandoned(ctx context.Context, db *sql.DB, in RefundOutcomeRequest, inspected RefundOutcomeTarget, providerRef string) (RefundOutcome, bool, error) {
	if in.Kind != OutcomeAbandoned {
		return RefundOutcome{}, false, fmt.Errorf("RecordAbandoned takes kind %s, got %q", OutcomeAbandoned, in.Kind)
	}
	if err := in.Validate(); err != nil {
		return RefundOutcome{}, false, err
	}
	if !strings.HasPrefix(providerRef, "re_") || len(providerRef) <= len("re_") || len(providerRef) > 200 {
		return RefundOutcome{}, false, fmt.Errorf("abandoned outcome needs the provider's refund reference, got %q", providerRef)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	locked, existing, err := lockOutcomeTarget(ctx, tx, in, inspected)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if existing != nil {
		return *existing, true, tx.Commit()
	}
	out, err := appendRefundOutcome(ctx, tx, in, locked, providerRef)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	return out, false, tx.Commit()
}

// lockedOutcomeTarget is the refund and order as read under their row locks.
type lockedOutcomeTarget struct {
	order    uuid.UUID
	quantity int32
	amount   int64
	currency string
	sold     int32
}

// lockOutcomeTarget locks the refund row and then its order (CompleteOrderRefund's order, D16),
// rechecks everything the scoped inspection read before the payments call, and decides a
// repeated command. existing is non-nil for an exact replay of an outcome already recorded.
//
// Under those locks: the source reservation still belongs to the organizer; the order's charge
// key and the refund's order, amount and currency are what was inspected; any outcome that
// committed in the meantime is a replay or a conflict; and only a pending refund is eligible.
func lockOutcomeTarget(ctx context.Context, tx *sql.Tx, in RefundOutcomeRequest, inspected RefundOutcomeTarget) (lockedOutcomeTarget, *RefundOutcome, error) {
	var l lockedOutcomeTarget
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT order_id, quantity, amount, currency, status FROM order_refunds
		WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, in.OrganizerID, in.RefundID).
		Scan(&l.order, &l.quantity, &l.amount, &l.currency, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil, ErrRefundOutcomeNotFound
	}
	if err != nil {
		return l, nil, err
	}

	var chargeKey string
	var sourceOrg uuid.UUID
	err = tx.QueryRowContext(ctx, outsideOrderLockSQL, l.order).Scan(&chargeKey, &sourceOrg, &l.sold)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil, ErrRefundOutcomeNotFound
	}
	if err != nil {
		return l, nil, err
	}
	// The source reservation is the organizer's, checked again under the lock: the
	// inspection's answer is not evidence about the row this transaction is about to write.
	if sourceOrg != in.OrganizerID {
		return l, nil, ErrRefundOutcomeNotFound
	}
	if l.order != inspected.OrderID || chargeKey != inspected.ChargeKey ||
		l.amount != inspected.Amount || l.currency != inspected.Currency {
		return l, nil, ErrRefundOutcomeStale
	}

	existing, found, err := lookupRefundOutcome(ctx, tx, in.OrganizerID, in.RefundID)
	if err != nil {
		return l, nil, err
	}
	if found {
		if !existing.matches(in) {
			return l, nil, ErrRefundOutcomeConflict
		}
		return l, &existing, nil
	}
	if status != "pending" {
		return l, nil, ErrRefundOutcomeIneligible
	}
	return l, nil, nil
}

// appendRefundOutcome inserts the outcome row. Amount and currency come from the locked refund.
func appendRefundOutcome(ctx context.Context, tx *sql.Tx, in RefundOutcomeRequest, l lockedOutcomeTarget, externalRef string) (RefundOutcome, error) {
	out := RefundOutcome{
		OrganizerID: in.OrganizerID, RefundID: in.RefundID, Kind: in.Kind,
		Amount: l.amount, Currency: l.currency, ExternalRef: externalRef,
		OperatorName: in.OperatorName, Reason: in.Reason,
	}
	// session_user is read by the database on THIS connection, never passed in: a caller
	// cannot supply it, and it records which login wrote the row separately from the typed name.
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,
		                            external_ref,operator_name,session_user_name,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,session_user,$9)
		RETURNING session_user_name, recorded_at`,
		in.OrganizerID, refundOutcomeSubject, in.RefundID, in.Kind, l.amount, l.currency,
		externalRef, in.OperatorName, in.Reason).Scan(&out.SessionUser, &out.RecordedAt); err != nil {
		return RefundOutcome{}, err
	}
	out.RecordedAt = out.RecordedAt.UTC()
	return out, nil
}
