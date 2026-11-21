// Package refundoutcome records an operator's outcome for a failed post-purchase refund
// (TKT-525). ADR-087 records the full protocol.
//
// `refunded_outside` (Service) completes the refund with the outcome as evidence. Its payments
// port is one READ method: a unit that cannot express a write cannot perform one.
//
// `abandoned` (AbandonService) has its own one-method port, the payments abandon call. It
// reserves in Commerce, lets payments commit its marker, then appends the outcome; the refund
// stays pending.
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

// RecordRefundedOutside is the whole S1 operator act: inspect with no lock (local answers make
// no payments call), ask payments for the read-only proof with no lock held, then write in one
// short transaction that locks the refund, then the order, and rechecks what was inspected.
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

// RecordAbandoned is the whole S2 operator act: inspect, reserve, call, finish (ADR-087).
//
// The reservation must commit before any HTTP call; a failure there stops the command. The
// payments call holds no Commerce lock. Only an answer for exactly the reserved money appends
// the outcome and finalizes the reservation, and that commit releases the quantity. Any failure
// after the reservation leaves it pending, and the same command resumes it.
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
