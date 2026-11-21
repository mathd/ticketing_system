// Package refundoutcome records an operator's outcome for a failed post-purchase refund
// (TKT-525, ADR-087).
//
// A staff or bulk refund whose provider refund failed stays pending: payments never re-submits
// a failed refund (ADR-032 §Refund rule 3), its leg stays bound, and both allowances stay
// reserved. Nothing in the service can finish it. An operator who has established what happened
// outside the platform records it here.
//
// S1 implements `refunded_outside`: the buyer received the full stored amount by an outside
// transfer. The refund completes with the outcome as its evidence, both allowances stay
// consumed, and the existing reversal reconciler voids the tickets and returns the capacity.
//
// THE PAYMENTS PORT IS ONE READ METHOD, and the narrowness is the guarantee, as in
// exchangeunwind: a unit that cannot express a write cannot perform one. The command makes no
// writing HTTP call — no refund, no fact forward, no marker.
package refundoutcome

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// FailureProof is payments' terminal-failure evidence for one refund leg.
type FailureProof struct {
	// ProviderRef is the provider's reference for the failed or canceled refund.
	ProviderRef string
	// Amount and Currency are the leg's stored money, corroborated by the provider object.
	Amount   int64
	Currency string
}

// Payments is the read-only proof surface.
type Payments interface {
	// ProveRefundFailure asks payments to prove that the leg addressed by (organizer, charge
	// key, refund key) is still bound, carries no abandonment marker, and that its stamped
	// provider refund terminally failed.
	ProveRefundFailure(ctx context.Context, org uuid.UUID, chargeKey, refundKey string) (FailureProof, error)
}

var (
	// ErrProofRefused is a conclusive refusal from payments: no such leg, or the leg completed,
	// carries an abandonment marker, or the provider evidence says the refund did not fail.
	ErrProofRefused = errors.New("payments refused the refund failure proof")
	// ErrProofUnavailable means payments could not be asked or could not establish a proof.
	// Nothing was changed. Fix the cause and run the same command again.
	ErrProofUnavailable = errors.New("refund failure proof unavailable")
	// ErrProofMismatch reports a proof for money other than the refund's stored amount and
	// currency.
	ErrProofMismatch = errors.New("refund failure proof does not match the refund's money")
)

// Service is the operator path. It holds no lease and runs on no timer.
type Service struct {
	db       *sql.DB
	payments Payments
}

func New(db *sql.DB, p Payments) *Service {
	return &Service{db: db, payments: p}
}

// Result is one recorded outcome. Replay reports that the outcome was already recorded by an
// identical earlier command; it carries that command's original time and attribution.
type Result struct {
	Outcome store.RefundOutcome
	Proof   FailureProof
	Replay  bool
}

// RecordRefundedOutside is the whole S1 operator act.
//
// The order is the correctness argument:
//
//  1. Inspect the refund in the organizer, with no lock. A missing or foreign refund, an exact
//     replay, a conflicting earlier outcome, and an ordinarily completed refund are all
//     answered here, from Commerce alone, with no payments call.
//  2. Ask payments for the read-only proof, with no database lock held (ADR-037 §4). The proof
//     uses the order's STORED charge key and the refund id as the leg's refund key, and must
//     come back for exactly the refund's stored amount and currency.
//  3. Write in one short transaction that locks the refund, then the order, and rechecks
//     everything step 1 read.
func (s *Service) RecordRefundedOutside(ctx context.Context, in store.RefundOutcomeRequest) (Result, error) {
	if in.Kind != store.OutcomeRefundedOutside {
		return Result{}, fmt.Errorf("outcome kind %q is not %s", in.Kind, store.OutcomeRefundedOutside)
	}
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	target, err := store.InspectRefundOutcomeTarget(ctx, s.db, in.OrganizerID, in.RefundID)
	if err != nil {
		return Result{}, err
	}
	if existing, found, err := target.Replay(in); found {
		if err != nil {
			return Result{}, err
		}
		return Result{Outcome: existing, Replay: true}, nil
	}
	if target.Completed() {
		return Result{}, store.ErrRefundOutcomeIneligible
	}

	proof, err := s.payments.ProveRefundFailure(ctx, in.OrganizerID, target.ChargeKey, in.RefundID.String())
	if err != nil {
		return Result{}, err
	}
	if proof.Amount != target.Amount || proof.Currency != target.Currency {
		return Result{}, fmt.Errorf("%w: payments proved %d %s, the refund is %d %s",
			ErrProofMismatch, proof.Amount, proof.Currency, target.Amount, target.Currency)
	}

	out, replay, err := store.RecordRefundedOutside(ctx, s.db, in, target)
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: out, Proof: proof, Replay: replay}, nil
}
