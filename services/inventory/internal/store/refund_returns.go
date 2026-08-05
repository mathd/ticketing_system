package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Refund capacity return (TKT-161, ADR-038). The second and last obligation of a refund's
// reversal: TKT-156 returned the money, TKT-157 voided the tickets, this gives the seat
// back.
//
// Voiding goes first, always — freeing capacity while the original ticket still admits is
// the one ordering that can OVERSELL (ADR-038 §1). Commerce enforces that; inventory only
// has to be correct about the accounting.

var (
	// ErrRefundReturnExceedsClaim reports a return larger than the claim still owes.
	ErrRefundReturnExceedsClaim = errors.New("refund would return more than the claim's unreturned quantity")
	// ErrRefundReturnNotConfirmed reports a claim that is not a confirmed buyer claim.
	// Operational holds and group reservations transition through their own staff
	// endpoints, exactly as Transition already refuses them.
	ErrRefundReturnNotConfirmed = errors.New("only a confirmed buyer claim can return capacity")
	// ErrRefundReturnConflict reports the same refund identity used with a different
	// request.
	ErrRefundReturnConflict = errors.New("refund identity reused with a different request")
	// ErrPartialSeatedReturn reports a partial return of a SEATED claim, which has no
	// correct answer: nothing associates an issued ticket with a seat identity, so no
	// subset of seats can be derived (TKT-164). The refund itself still completes — the
	// buyer is repaid and the tickets are voided — and only the resale is lost, which
	// under-sells and cannot oversell.
	ErrPartialSeatedReturn = errors.New("a partial return of a seated claim cannot identify which seats to free")
)

// RefundCapacityReturn is one applied return.
type RefundCapacityReturn struct {
	ClaimID uuid.UUID `json:"hold_id"`
	// Quantity is what this return gave back.
	Quantity int32 `json:"quantity"`
	// UnreturnedQuantity is what the claim still owes after it.
	UnreturnedQuantity int32 `json:"unreturned_quantity"`
	Replay             bool  `json:"replay"`
}

// refundReturnKey is the claim_history idempotency key for one refund's return. Namespaced
// so it can never collide with a staff operation's key in the same registry.
func refundReturnKey(refundID uuid.UUID) string { return "refund:" + refundID.String() }

// ReturnRefundedCapacity gives q of a confirmed claim's quantity back to the pool.
//
// Pool lock FIRST, then the claim — the same order every other transition here takes, so
// a return can never deadlock against a confirm, a release or a capacity adjustment.
func (p *Postgres) ReturnRefundedCapacity(ctx context.Context, org, claimID, refundID uuid.UUID, quantity int32) (RefundCapacityReturn, error) {
	if quantity < 1 {
		return RefundCapacityReturn{}, errors.New("refund return quantity must be positive")
	}
	var pool uuid.UUID
	err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1 AND organizer_id=$2`, claimID, org).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return RefundCapacityReturn{}, ErrNotFound
	}
	if err != nil {
		return RefundCapacityReturn{}, err
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RefundCapacityReturn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return RefundCapacityReturn{}, err
	}

	var quantityTotal, returned int32
	var status, kind string
	if err = tx.QueryRowContext(ctx, `SELECT quantity,returned_quantity,status,claim_kind FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, claimID, org).
		Scan(&quantityTotal, &returned, &status, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RefundCapacityReturn{}, ErrNotFound
		}
		return RefundCapacityReturn{}, err
	}

	// The replay resolves BEFORE eligibility is re-derived, mirroring the money paths: a
	// return that already applied legitimately moved the numbers it would now be judged
	// against, and re-judging would refuse its own progress.
	fingerprint := refundReturnFingerprint(org, claimID, quantity)
	var storedFingerprint sql.NullString
	var storedAfter int32
	err = tx.QueryRowContext(ctx, `SELECT request_fingerprint,quantity_after FROM claim_history WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, refundReturnKey(refundID)).Scan(&storedFingerprint, &storedAfter)
	switch {
	case err == nil:
		if !storedFingerprint.Valid || storedFingerprint.String != fingerprint {
			return RefundCapacityReturn{}, ErrRefundReturnConflict
		}
		return RefundCapacityReturn{ClaimID: claimID, Quantity: quantity, UnreturnedQuantity: storedAfter, Replay: true}, p.commitAvailability(tx, pool)
	case !errors.Is(err, sql.ErrNoRows):
		return RefundCapacityReturn{}, err
	}

	if status != "confirmed" || kind != "buyer" {
		return RefundCapacityReturn{}, ErrRefundReturnNotConfirmed
	}
	if returned+quantity > quantityTotal {
		return RefundCapacityReturn{}, fmt.Errorf("%w: %d requested, %d unreturned", ErrRefundReturnExceedsClaim, quantity, quantityTotal-returned)
	}

	// Seats: a FULL return frees them all, a partial one has no answer. `claim_seats` is
	// per claim, not per ticket, and nothing associates an issued ticket with a seat
	// identity — so "which two of these three seats" is a question the model cannot
	// answer (TKT-164). releaseSeatsForTerminal does not help: it selects claims already
	// in ('expired','released'), and a returned claim stays confirmed.
	// Seatedness is ANY seat row, released or not — never the count of LIVE ones
	// (ai-review pass 2). `claims.status` and `claim_seats.released_at` are not coupled by
	// the schema, so a confirmed seated claim whose rows were already released by restore
	// skew or an earlier defect is representable; `classifySeatClaimsInPool` says exactly
	// that, a few lines from here. Inferring "not seated" from zero live rows would let a
	// partial return of such a claim through as if it were GA, and then unpin every one of
	// its seats — SeatPinRef reads released rows too — stripping catalog protection from
	// the seats its remaining live tickets still occupy.
	var seats int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1`, claimID).Scan(&seats); err != nil {
		return RefundCapacityReturn{}, err
	}
	if seats > 0 {
		if returned != 0 || quantity != quantityTotal {
			return RefundCapacityReturn{}, ErrPartialSeatedReturn
		}
		if _, err = tx.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1 AND released_at IS NULL`, claimID); err != nil {
			return RefundCapacityReturn{}, err
		}
	}

	// Guarded so the decrement can never drive the counter negative even if the read
	// above raced something this transaction does not hold.
	result, err := tx.ExecContext(ctx, `UPDATE inventory_pools SET confirmed_quantity=confirmed_quantity-$1,updated_at=now() WHERE slot_id=$2 AND confirmed_quantity>=$1`, quantity, pool)
	if err != nil {
		return RefundCapacityReturn{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return RefundCapacityReturn{}, fmt.Errorf("%w: pool holds less confirmed quantity than the claim", ErrRefundReturnExceedsClaim)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE claims SET returned_quantity=returned_quantity+$1,updated_at=now() WHERE id=$2`, quantity, claimID); err != nil {
		return RefundCapacityReturn{}, err
	}
	// A return lowers demand, which is exactly the condition a draining capacity cut is
	// waiting on (TKT-76). Without this the materialized capacity stays stale at the old
	// clamp floor even though the cut could now settle.
	if err = reconcileCapacity(ctx, tx, pool); err != nil {
		return RefundCapacityReturn{}, err
	}

	after := quantityTotal - returned - quantity
	key, fp := refundReturnKey(refundID), fingerprint
	if err = appendHistory(ctx, tx, org, claimID, nil, "refund_return", "commerce", "refund", quantity, after, "confirmed", &key, &fp); err != nil {
		return RefundCapacityReturn{}, err
	}
	return RefundCapacityReturn{ClaimID: claimID, Quantity: quantity, UnreturnedQuantity: after}, p.commitAvailability(tx, pool)
}

func refundReturnFingerprint(org, claimID uuid.UUID, quantity int32) string {
	return opFingerprint("refund-return", org, claimID, quantity)
}

// ClaimIsSeated reports whether a claim holds seats. ANY seat row counts, released or
// not — the same rule the return path uses, and for the same reason: `claims.status` and
// `claim_seats.released_at` are not schema-coupled, so a seated claim whose rows were
// already released is representable and is still a seated claim (TKT-161 ai-review).
func (p *Postgres) ClaimIsSeated(ctx context.Context, org, claimID uuid.UUID) (bool, error) {
	var one int
	if err := p.db.QueryRowContext(ctx, `SELECT 1 FROM claims WHERE id=$1 AND organizer_id=$2`, claimID, org).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	var seats int
	err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1`, claimID).Scan(&seats)
	return seats > 0, err
}
