package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	// ConfirmedAmount/ConfirmedCurrency are what the PROVIDER reported returning for this
	// leg, as distinct from Amount, which is what the leg BOUND before the provider call
	// (TKT-257). nil on a leg still bound, and on any leg completed before migration 0006 —
	// a legacy completed leg has no provider confirmation and never can, so it must answer
	// absent rather than have its bound amount promoted to evidence.
	ConfirmedAmount   *int64
	ConfirmedCurrency string
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
	var providerRef, confCurrency sql.NullString
	var confAmount sql.NullInt64
	var factID uuid.NullUUID
	var boundAt time.Time
	err := q.QueryRowContext(ctx, `SELECT refund_idempotency_key,provider_idempotency_key,amount,currency,status,provider_ref,fact_id,bound_at,confirmed_amount,confirmed_currency FROM payment_refund_legs WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3`, org, sourceKey, refundKey).
		Scan(&leg.RefundKey, &leg.ProviderKey, &leg.Amount, &leg.Currency, &leg.Status, &providerRef, &factID, &boundAt, &confAmount, &confCurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundLeg{}, false, nil
	}
	if err != nil {
		return RefundLeg{}, false, err
	}
	leg.ProviderRef, leg.FactID = providerRef.String, factID.UUID
	leg.Completed = leg.Status == "refunded"
	leg.BoundAt = boundAt.UTC().Truncate(time.Microsecond)
	// NULL stays nil — a legacy completed leg answers "no confirmation", never zero.
	if confAmount.Valid {
		v := confAmount.Int64
		leg.ConfirmedAmount = &v
	}
	leg.ConfirmedCurrency = confCurrency.String
	return leg, true, nil
}

// CompleteRefundLeg records the provider result, the journalled fact, and the money the
// provider confirmed it returned. Only the first completion writes (the status guard) — a
// replay keeps the original result, which is what makes a crash between the provider call
// and this write harmless.
//
// confirmed is REQUIRED (TKT-257). This is where "a leg completed from now on carries
// provider confirmation" is enforced, rather than in the table's completion CHECK: that
// rule and "a pre-0006 row answers absent" are contradictory statements about legs already
// completed when the migration ran, and a CHECK strong enough for the first rejects the
// second. Enforced here, the two cases are distinguished by construction — a new completion
// must supply it, and a historical row is simply never rewritten.
//
// ErrRefundLegNotCompleted when the UPDATE matches nothing. The previous version returned
// only the driver error, so a no-op — the row already refunded, or gone — was
// indistinguishable from a completion that landed. Silence there means the caller has
// already appended the compensating fact to an append-only journal and believes the leg
// settled. RecordProviderState in this package has checked RowsAffected since TKT-114.
func (j *Journal) CompleteRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey, providerRef string, factID uuid.UUID, confirmed ConfirmedRefund) error {
	if confirmed.Amount <= 0 || confirmed.Currency == "" {
		return fmt.Errorf("%w: completing a leg requires the provider-confirmed money it returned, got %+v", ErrRefundLegNotCompleted, confirmed)
	}
	res, err := j.db.ExecContext(ctx, `
		UPDATE payment_refund_legs SET status='refunded',provider_ref=NULLIF($4,''),fact_id=$5,completed_at=now(),
		                              confirmed_amount=$6,confirmed_currency=$7
		WHERE organizer_id=$1 AND source_idempotency_key=$2 AND refund_idempotency_key=$3 AND status='bound'`,
		org, sourceKey, refundKey, providerRef, factID, confirmed.Amount, confirmed.Currency)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Zero rows has two causes needing opposite answers — the same split
		// CompleteCompensation documents (ai-review, third pass).
		//
		// Benign: a CONCURRENT duplicate. The handler short-circuits a completed leg before
		// calling this, but two requests can both pass that check, both append the
		// deterministic fact (the journal dedupes), and both arrive here. One UPDATE wins on
		// `status='bound'`; the loser's work is done under the same fact id, so failing it
		// would turn a successful duplicate into a 500.
		//
		// Dangerous: the leg is missing, or still bound. Then the caller has appended a
		// compensating fact to an append-only journal and believes the money came back while
		// nothing durable records it.
		existing, found, lookupErr := lookupRefundLegTx(ctx, j.db, org, sourceKey, refundKey)
		if lookupErr != nil {
			return lookupErr
		}
		if !found || !existing.Completed {
			return ErrRefundLegNotCompleted
		}
		if existing.FactID != factID {
			return fmt.Errorf("%w: already completed under fact %s", ErrRefundLegNotCompleted, existing.FactID)
		}
		return nil
	}
	return nil
}

// ConfirmedRefund is the provider's own figure for a settled refund leg.
type ConfirmedRefund struct {
	Amount   int64
	Currency string
}

// ErrRefundLegNotCompleted reports that a completion wrote no row.
var ErrRefundLegNotCompleted = errors.New("payments: refund leg completion wrote no row")
