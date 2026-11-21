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
//
// S2 implements `abandoned` in its own type, AbandonService, with its own one-method port: the
// payments abandon call. Commerce first commits an exclusive abandonment reservation, then
// payments commits its marker and returns the provider's reference, then Commerce appends the
// outcome and finalizes the reservation. The refund stays pending, and each service releases
// only that refund's allowance in its own ceiling.
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
//     An abandonment reservation for the refund refuses here too, with no payments call.
//  2. Ask payments for the read-only proof, with no database lock held (ADR-037 §4). The proof
//     uses the order's STORED charge key and the refund id as the leg's refund key, and must
//     come back for exactly the refund's stored amount and currency.
//  3. Write in one short transaction that locks the refund, then the order, and rechecks
//     everything step 1 read, including a reservation committed while the proof was in flight.
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
	// An abandonment reservation, pending or finalized, owns the refund. Refused before the
	// proof read; the locked write checks again for one committed while the proof was in flight.
	if target.Disposition != nil {
		return Result{}, store.ErrAbandonmentReserved
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

// Abandoner is the one payments write the `abandoned` path makes (TKT-525 S2).
type Abandoner interface {
	// AbandonRefundLeg asks payments to prove the leg's stamped provider refund terminally
	// failed and to commit its abandonment marker, or to answer from a marker it already
	// committed. It returns the provider's reference for the failed refund.
	AbandonRefundLeg(ctx context.Context, org uuid.UUID, chargeKey, refundKey string, amount int64, currency string) (FailureProof, error)
}

var (
	// ErrAbandonRefused is a conclusive refusal of ONE abandon request: no such leg, the leg
	// completed, its money differs, or the provider evidence says the refund did not fail. That
	// request wrote no marker. The Commerce reservation stays pending, because an overlapping
	// request can still commit the marker; the same command run again finishes from it.
	ErrAbandonRefused = errors.New("payments refused the abandonment")
	// ErrAbandonUncertain means payments' answer is unknown or unusable: it may have committed
	// its marker. Commerce keeps the reservation pending and the quantity reserved. Run the SAME
	// command again; payments answers a committed marker without asking the provider.
	ErrAbandonUncertain = errors.New("payments abandonment answer unknown")
)

// AbandonService is the `abandoned` operator path. Its own type, so the outside-completion
// path (Service) keeps a port that can only read.
type AbandonService struct {
	db       *sql.DB
	payments Abandoner
}

func NewAbandon(db *sql.DB, a Abandoner) *AbandonService {
	return &AbandonService{db: db, payments: a}
}

// RecordAbandoned is the whole S2 operator act: reserve, call, finish.
//
//  1. Inspect the refund in the organizer, with no lock. A missing or foreign refund, an exact
//     replay of a finalized abandonment, a conflicting outcome, and a completed refund are
//     answered here, with no payments call.
//  2. Commit the Commerce reservation (store.ReserveAbandonment). From here an outside
//     completion of this refund is refused. If this fails, including at commit, stop before any
//     HTTP call; an exact retry finds out whether the row committed.
//  3. Ask payments to abandon the exact leg, with the reservation's stored charge key and money
//     and the refund id as the leg's refund key, with no Commerce lock held. Payments proves the
//     terminal failure, commits its marker (which releases the leg's money allowance) and
//     returns the provider's reference, or answers from a marker it already committed.
//  4. Only after a valid answer for exactly the reserved money, append the outcome and finalize
//     the reservation in one short transaction (store.FinishAbandonment). That commit is what
//     releases the quantity.
//
// Any failure after step 2 leaves the reservation pending: the quantity stays reserved, and the
// same command run again resumes it with the original operator name and reason.
func (s *AbandonService) RecordAbandoned(ctx context.Context, in store.RefundOutcomeRequest) (Result, error) {
	if in.Kind != store.OutcomeAbandoned {
		return Result{}, fmt.Errorf("outcome kind %q is not %s", in.Kind, store.OutcomeAbandoned)
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

	reserved, existing, err := store.ReserveAbandonment(ctx, s.db, in, target)
	if err != nil {
		return Result{}, err
	}
	if existing != nil {
		return Result{Outcome: *existing, Replay: true}, nil
	}

	evidence, err := s.payments.AbandonRefundLeg(ctx, in.OrganizerID, reserved.ChargeKey, in.RefundID.String(), reserved.Amount, reserved.Currency)
	if err != nil {
		return Result{}, err
	}
	if evidence.Amount != reserved.Amount || evidence.Currency != reserved.Currency {
		return Result{}, fmt.Errorf("%w: payments answered for %d %s, the refund is %d %s",
			ErrAbandonUncertain, evidence.Amount, evidence.Currency, reserved.Amount, reserved.Currency)
	}

	out, replay, err := store.FinishAbandonment(ctx, s.db, in, target, evidence.ProviderRef)
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: out, Proof: evidence, Replay: replay}, nil
}
