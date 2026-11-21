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
	// ErrAbandonmentReserved reports an abandonment reservation for the refund, pending or
	// finalized. An operator started to abandon this refund; an outside completion of it is
	// refused, so the two dispositions cannot both commit (ADR-087).
	ErrAbandonmentReserved = errors.New("an abandonment is reserved for this refund")
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
	// Disposition is the abandonment reservation for this refund, or nil.
	Disposition *AbandonmentDisposition
}

// Abandonment disposition states. A reservation moves only from pending to finalized.
const (
	DispositionPending   = "pending"
	DispositionFinalized = "finalized"
)

// AbandonmentDisposition is the Commerce abandonment reservation for one refund (ADR-087). It
// is committed before payments is asked to release the leg's allowance, and it finalizes in the
// same transaction that appends the `abandoned` outcome.
type AbandonmentDisposition struct {
	State string
	// ChargeKey, Amount and Currency are the refund's stored charge key and money, copied when
	// the row was reserved. The payments call uses these.
	ChargeKey    string
	Amount       int64
	Currency     string
	OperatorName string
	Reason       string
	ReservedAt   time.Time
}

// resumes reports whether a repeated abandonment command is the command that reserved the row.
func (d AbandonmentDisposition) resumes(in RefundOutcomeRequest) bool {
	return in.Kind == OutcomeAbandoned && d.OperatorName == in.OperatorName && d.Reason == in.Reason
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
	d, found, err := lookupAbandonmentDisposition(ctx, db, org, refundID)
	if err != nil {
		return RefundOutcomeTarget{}, err
	}
	if found {
		t.Disposition = &d
	}
	return t, nil
}

func lookupAbandonmentDisposition(ctx context.Context, q rowQuerier, org, refundID uuid.UUID) (AbandonmentDisposition, bool, error) {
	var d AbandonmentDisposition
	err := q.QueryRowContext(ctx, `
		SELECT state, charge_key, amount, currency, operator_name, reason, reserved_at
		FROM order_refund_abandonment_dispositions WHERE organizer_id=$1 AND refund_id=$2`, org, refundID).
		Scan(&d.State, &d.ChargeKey, &d.Amount, &d.Currency, &d.OperatorName, &d.Reason, &d.ReservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AbandonmentDisposition{}, false, nil
	}
	if err != nil {
		return AbandonmentDisposition{}, false, err
	}
	d.ReservedAt = d.ReservedAt.UTC()
	return d, true, nil
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

	locked, err := lockOutcomeTarget(ctx, tx, in, inspected)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if existing, err := locked.replay(in); err != nil || existing != nil {
		if err != nil {
			return RefundOutcome{}, false, err
		}
		return *existing, true, tx.Commit()
	}
	// Under the refund lock, so a reservation committed while the proof was in flight is seen
	// here: the abandonment owns this refund (ADR-087).
	if locked.disposition != nil {
		return RefundOutcome{}, false, ErrAbandonmentReserved
	}
	if locked.status != "pending" {
		return RefundOutcome{}, false, ErrRefundOutcomeIneligible
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

// ReserveAbandonment commits the Commerce abandonment reservation for one refund (TKT-525 S2,
// ADR-087), in ONE short transaction, BEFORE any payments call. The caller must stop if this
// returns an error, including a commit error: an exact retry finds out whether the row committed.
//
// Under the refund-then-order locks it rechecks what the inspection read, then:
//
//   - an outcome already recorded answers first: an exact `abandoned` replay is returned in
//     `existing`, and anything else (an outside outcome, a different command) conflicts;
//   - a refund that is no longer pending is ineligible;
//   - with no reservation it inserts a pending one, with the refund's charge key and money and
//     the command's operator name and reason;
//   - a pending reservation is resumed when the operator name and reason match exactly, and
//     conflicts otherwise.
//
// The returned reservation carries the stored identities the payments call must use.
func ReserveAbandonment(ctx context.Context, db *sql.DB, in RefundOutcomeRequest, inspected RefundOutcomeTarget) (AbandonmentDisposition, *RefundOutcome, error) {
	if in.Kind != OutcomeAbandoned {
		return AbandonmentDisposition{}, nil, fmt.Errorf("ReserveAbandonment takes kind %s, got %q", OutcomeAbandoned, in.Kind)
	}
	if err := in.Validate(); err != nil {
		return AbandonmentDisposition{}, nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AbandonmentDisposition{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	locked, err := lockOutcomeTarget(ctx, tx, in, inspected)
	if err != nil {
		return AbandonmentDisposition{}, nil, err
	}
	if existing, err := locked.replay(in); err != nil || existing != nil {
		if err != nil {
			return AbandonmentDisposition{}, nil, err
		}
		return AbandonmentDisposition{}, existing, tx.Commit()
	}
	if locked.status != "pending" {
		return AbandonmentDisposition{}, nil, ErrRefundOutcomeIneligible
	}
	if d := locked.disposition; d != nil {
		// A finalized reservation always has its outcome (they commit together), which the
		// replay above answered. A pending one is resumed only by the same command.
		if d.State != DispositionPending || !d.resumes(in) {
			return AbandonmentDisposition{}, nil, ErrRefundOutcomeConflict
		}
		return *d, nil, tx.Commit()
	}
	d := AbandonmentDisposition{State: DispositionPending, ChargeKey: locked.chargeKey, Amount: locked.amount,
		Currency: locked.currency, OperatorName: in.OperatorName, Reason: in.Reason}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO order_refund_abandonment_dispositions(organizer_id,refund_id,state,charge_key,amount,currency,operator_name,reason)
		VALUES($1,$2,'pending',$3,$4,$5,$6,$7) RETURNING reserved_at`,
		in.OrganizerID, in.RefundID, d.ChargeKey, d.Amount, d.Currency, d.OperatorName, d.Reason).Scan(&d.ReservedAt); err != nil {
		return AbandonmentDisposition{}, nil, err
	}
	d.ReservedAt = d.ReservedAt.UTC()
	return d, nil, tx.Commit()
}

// FinishAbandonment appends the `abandoned` outcome and finalizes the reservation, in ONE short
// transaction, after payments committed its marker for the reserved leg and returned the
// provider's reference for the failed refund. That reference becomes the outcome's external
// reference; the operator never types it.
//
// Under the refund-then-order locks it rechecks the target, the reservation and the refund
// state. A concurrent finish of the same reservation is answered with its original outcome. The
// outcome carries the reserved operator name and reason. The refund stays pending; the order
// projection, the tickets and the capacity keep their state. The quantity allowance is released
// because BindOrderRefund's sum excludes a refund with an `abandoned` outcome, so the release
// happens exactly when this commits.
//
// The order row lock is part of the contract: a fresh cancellation bind holds it while it
// checks for abandoned outcomes and sums the ceiling, so it sees this commit entirely or not at
// all (TestFinishAbandonmentLocksTheRefundBeforeTheOrder).
func FinishAbandonment(ctx context.Context, db *sql.DB, in RefundOutcomeRequest, inspected RefundOutcomeTarget, providerRef string) (RefundOutcome, bool, error) {
	if in.Kind != OutcomeAbandoned {
		return RefundOutcome{}, false, fmt.Errorf("FinishAbandonment takes kind %s, got %q", OutcomeAbandoned, in.Kind)
	}
	if err := in.Validate(); err != nil {
		return RefundOutcome{}, false, err
	}
	if !strings.HasPrefix(providerRef, "re_") || len(providerRef) <= len("re_") || len(providerRef) > MaxProviderRefundRef {
		return RefundOutcome{}, false, fmt.Errorf("abandoned outcome needs the provider's refund reference, got %q", providerRef)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	locked, err := lockOutcomeTarget(ctx, tx, in, inspected)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if existing, err := locked.replay(in); err != nil || existing != nil {
		if err != nil {
			return RefundOutcome{}, false, err
		}
		return *existing, true, tx.Commit()
	}
	d := locked.disposition
	if d == nil || d.State != DispositionPending || !d.resumes(in) ||
		d.ChargeKey != locked.chargeKey || d.Amount != locked.amount || d.Currency != locked.currency {
		return RefundOutcome{}, false, ErrRefundOutcomeStale
	}
	if locked.status != "pending" {
		return RefundOutcome{}, false, ErrRefundOutcomeIneligible
	}
	out, err := appendRefundOutcome(ctx, tx, in, locked, providerRef)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE order_refund_abandonment_dispositions SET state='finalized'
		WHERE organizer_id=$1 AND refund_id=$2 AND state='pending'`, in.OrganizerID, in.RefundID); err != nil {
		return RefundOutcome{}, false, err
	}
	return out, false, tx.Commit()
}

// MaxProviderRefundRef is the longest provider refund reference payments accepts, and so the
// longest one an `abandoned` outcome stores. An outside reference is at most 200 bytes.
const MaxProviderRefundRef = 255

// lockedOutcomeTarget is the refund and order as read under their row locks, with the outcome
// and reservation read after both locks.
type lockedOutcomeTarget struct {
	order       uuid.UUID
	quantity    int32
	amount      int64
	currency    string
	sold        int32
	status      string
	chargeKey   string
	outcome     *RefundOutcome
	disposition *AbandonmentDisposition
}

// replay decides a repeated command against an outcome read under the locks: the original
// record for the same command, a conflict for any other, nil when there is no outcome.
func (l lockedOutcomeTarget) replay(in RefundOutcomeRequest) (*RefundOutcome, error) {
	if l.outcome == nil {
		return nil, nil
	}
	if !l.outcome.matches(in) {
		return nil, ErrRefundOutcomeConflict
	}
	return l.outcome, nil
}

// lockOutcomeTarget locks the refund row and then its order (CompleteOrderRefund's order),
// rechecks everything the scoped inspection read before the payments call, and reads the
// outcome and the reservation under those locks. The callers decide.
//
// Under those locks: the source reservation still belongs to the organizer, and the order's
// charge key and the refund's order, amount and currency are what was inspected.
func lockOutcomeTarget(ctx context.Context, tx *sql.Tx, in RefundOutcomeRequest, inspected RefundOutcomeTarget) (lockedOutcomeTarget, error) {
	var l lockedOutcomeTarget
	err := tx.QueryRowContext(ctx, `
		SELECT order_id, quantity, amount, currency, status FROM order_refunds
		WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, in.OrganizerID, in.RefundID).
		Scan(&l.order, &l.quantity, &l.amount, &l.currency, &l.status)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrRefundOutcomeNotFound
	}
	if err != nil {
		return l, err
	}

	var sourceOrg uuid.UUID
	err = tx.QueryRowContext(ctx, outsideOrderLockSQL, l.order).Scan(&l.chargeKey, &sourceOrg, &l.sold)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrRefundOutcomeNotFound
	}
	if err != nil {
		return l, err
	}
	// The source reservation is the organizer's, checked again under the lock: the
	// inspection's answer is not evidence about the row this transaction is about to write.
	if sourceOrg != in.OrganizerID {
		return l, ErrRefundOutcomeNotFound
	}
	if l.order != inspected.OrderID || l.chargeKey != inspected.ChargeKey ||
		l.amount != inspected.Amount || l.currency != inspected.Currency {
		return l, ErrRefundOutcomeStale
	}

	o, found, err := lookupRefundOutcome(ctx, tx, in.OrganizerID, in.RefundID)
	if err != nil {
		return l, err
	}
	if found {
		l.outcome = &o
	}
	d, found, err := lookupAbandonmentDisposition(ctx, tx, in.OrganizerID, in.RefundID)
	if err != nil {
		return l, err
	}
	if found {
		l.disposition = &d
	}
	return l, nil
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
