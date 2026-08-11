package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Post-purchase partial refund legs (TKT-156, ADR-037).
//
// The whole-refund compensation path (payment_compensations, /internal/psp/refund) is
// untouched: it is the recovery runner's, it refunds the entire captured amount, and its
// primary key is what makes duplicate recovery refunds converge. A post-purchase refund
// is a different fact — repeatable, amount-parameterized — so it gets its own row.

// ErrRefundExceedsCapture reports that a leg would take the cumulative refunded amount
// past the money the provider actually captured. BOUND legs count: an unresolved leg may
// yet settle, and releasing its allowance is how a charge gets over-refunded.
var ErrRefundExceedsCapture = errors.New("refund legs would exceed the captured amount")

// ErrWholeRefundBound reports that the recovery path already owns this operation's
// refund. The two paths track their ceilings in different tables, so neither may run
// while the other has a claim.
var ErrWholeRefundBound = errors.New("a whole refund is already bound for this operation")

// ErrRefundLegsBound is the same exclusion seen from the other side: the recovery
// whole-refund path refusing a charge that post-purchase legs already have a claim on.
// Both are decided under the SAME payment_operations row lock, which is what makes the
// exclusion atomic instead of a check with a window (TKT-156 ai-review, critical).
var ErrRefundLegsBound = errors.New("partial refund legs exist for this operation")

// RefundLegKey derives the bounded, versioned provider idempotency key for one leg. It
// mirrors CompensationKey's construction (versioned prefix, NUL separators so
// concatenation cannot collide) and differs in namespace so a leg and a whole refund can
// never derive the same provider key.
func RefundLegKey(org uuid.UUID, sourceKey, refundKey string) string {
	sum := sha256.Sum256([]byte(org.String() + "\x00" + sourceKey + "\x00" + refundKey))
	return "psp-refund-leg-v1:" + hex.EncodeToString(sum[:])
}

// RefundLeg is one durable partial-refund attempt against a captured charge.
type RefundLeg struct {
	RefundKey   string
	ProviderKey string
	Amount      int64
	Currency    string
	Status      string // "bound" until completed, then "refunded"
	ProviderRef string
	FactID      uuid.UUID
	Completed   bool
	// BoundAt is the row's stable creation time. The compensating fact's OccurredAt MUST
	// come from here rather than the clock: the fact id is deterministic and the journal's
	// replay dedupe compares the whole canonical fact, so a retry across the
	// append/complete crash boundary has to rebuild byte-identical content.
	BoundAt time.Time
}

// BindRefundLeg inserts-or-loads the one leg for (org, sourceKey, refundKey) under the
// payment_operations row lock, enforcing the captured-money ceiling.
//
// The lock is on the operation, not the leg: the ceiling is a property of the charge, and
// two legs that individually fit can only be judged together by whoever holds the charge.
// No provider call happens inside this transaction — a lock held across HTTP is how a
// provider outage becomes a database outage.
func (j *Journal) BindRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string, amount int64, currency string) (RefundLeg, error) {
	if amount <= 0 {
		return RefundLeg{}, refuse("refund amount must be positive")
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return RefundLeg{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var captured sql.NullInt64
	var state, opCurrency sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT provider_state,captured_amount,request_currency FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`, org, sourceKey).
		Scan(&state, &captured, &opCurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundLeg{}, sql.ErrNoRows
	}
	if err != nil {
		return RefundLeg{}, err
	}

	// A replay resolves BEFORE eligibility is re-derived, exactly as the whole-refund path
	// does: a leg that already settled legitimately moved the evidence on, and re-deriving
	// eligibility would refuse the compensation's own progress.
	existing, found, err := lookupRefundLegTx(ctx, tx, org, sourceKey, refundKey)
	if err != nil {
		return RefundLeg{}, err
	}
	if found {
		return existing, tx.Commit()
	}

	if state.String != "captured" || captured.Int64 <= 0 {
		return RefundLeg{}, refuse("refund requires captured money")
	}
	if opCurrency.String != currency {
		return RefundLeg{}, refuse("refund currency does not match the captured currency")
	}
	var whole int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payment_compensations WHERE organizer_id=$1 AND source_idempotency_key=$2 AND kind='refund'`, org, sourceKey).Scan(&whole); err != nil {
		return RefundLeg{}, err
	}
	if whole > 0 {
		return RefundLeg{}, ErrWholeRefundBound
	}
	var bound sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT sum(amount) FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2`, org, sourceKey).Scan(&bound); err != nil {
		return RefundLeg{}, err
	}
	if bound.Int64+amount > captured.Int64 {
		return RefundLeg{}, ErrRefundExceedsCapture
	}

	providerKey := RefundLegKey(org, sourceKey, refundKey)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,provider_idempotency_key,amount,currency)
		VALUES($1,$2,$3,$4,$5,$6)`, org, sourceKey, refundKey, providerKey, amount, currency); err != nil {
		return RefundLeg{}, err
	}
	leg, found, err := lookupRefundLegTx(ctx, tx, org, sourceKey, refundKey)
	if err != nil {
		return RefundLeg{}, err
	}
	if !found {
		return RefundLeg{}, errors.New("refund leg missing after bind")
	}
	return leg, tx.Commit()
}

// LookupRefundLeg reads a leg without binding one — the read-only replay check.
func (j *Journal) LookupRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (RefundLeg, bool, error) {
	return lookupRefundLegTx(ctx, j.db, org, sourceKey, refundKey)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func lookupRefundLegTx(ctx context.Context, q rowQuerier, org uuid.UUID, sourceKey, refundKey string) (RefundLeg, bool, error) {
	var leg RefundLeg
	var providerRef sql.NullString
	var factID uuid.NullUUID
	var boundAt time.Time
	err := q.QueryRowContext(ctx, `SELECT refund_idempotency_key,provider_idempotency_key,amount,currency,status,provider_ref,fact_id,bound_at FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3`, org, sourceKey, refundKey).
		Scan(&leg.RefundKey, &leg.ProviderKey, &leg.Amount, &leg.Currency, &leg.Status, &providerRef, &factID, &boundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundLeg{}, false, nil
	}
	if err != nil {
		return RefundLeg{}, false, err
	}
	leg.ProviderRef, leg.FactID = providerRef.String, factID.UUID
	leg.Completed = leg.Status == "refunded"
	leg.BoundAt = boundAt.UTC().Truncate(time.Microsecond)
	return leg, true, nil
}

// CompleteRefundLeg records the provider result and the journalled fact. Only the first
// completion writes (the status guard) — a replay keeps the original result, which is what
// makes a crash between the provider call and this write harmless.
func (j *Journal) CompleteRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey, providerRef string, factID uuid.UUID) error {
	_, err := j.db.ExecContext(ctx, `
		UPDATE payment_refund_legs SET status='refunded',provider_ref=NULLIF($4,''),fact_id=$5,completed_at=now()
		WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3 AND status='bound'`,
		org, sourceKey, refundKey, providerRef, factID)
	return err
}
