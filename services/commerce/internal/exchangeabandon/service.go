// Package exchangeabandon abandons a SETTLED exchange whose ticket switch Access refused because
// a source ticket was already admitted (TKT-498, owner decision of 2026-10-12, ADR-039 §2).
//
// The buyer used the source tickets, so the replacement must never issue. The abandonment
// refunds an upgrade's delta, writes off a downgrade's delta, returns the full target claim, and
// journals one `order.exchange.abandoned` compensating fact. The exchange, both orders and the
// original gross facts stay as history.
//
// THE ORDER OF EFFECTS, and why each step comes where it does:
//
//  1. Prove, before anything durable: the source is admitted and fully issued (Access read 1),
//     THEN the replacement has no issued ticket (Access read 2). The order is the proof. Once
//     read 1 observes an admission, Access's locked switch check refuses every later switch, so
//     a switch can only have committed before read 1, and its replacement tickets, inserted in
//     the switch's own transaction and never deleted, are then visible to read 2. Reading the
//     replacement first would miss a switch that commits between the reads.
//  2. Read the original money evidence and the original gross facts, and ask payments about each
//     pending replacement refund. A found leg, a completed refund, or evidence that disagrees
//     with the exchange stops the command before the reservation.
//  3. Reserve in Commerce under the order and exchange locks. Only the first trimmed operator
//     and reason pair owns the reservation, and the same pair resumes it.
//  4. Refund an upgrade's delta under the derived refund key, then record the payments fact.
//  5. Return the target claim under the derived receipt, then record the instant.
//  6. Append the compensating fact to the payments journal, then finish in Commerce.
//
// No transaction spans an HTTP call. Each remote effect is keyed by the one derived id, so a
// retry converges on the effect it already caused. A terminal row answers a replay from
// Commerce alone, with no remote call.
package exchangeabandon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/accessadmission"
	"ticketing/services/commerce/internal/exchangeunwind"
	"ticketing/services/commerce/internal/store"
)

// Access answers the admission summary of one order.
type Access interface {
	Admission(ctx context.Context, org, order uuid.UUID) (accessadmission.Admission, error)
}

// Payments is the money and journal surface the command uses.
type Payments interface {
	ChargeOperation(ctx context.Context, org uuid.UUID, key string) (ChargeEvidence, error)
	RefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (LegEvidence, error)
	PartialRefund(ctx context.Context, r RefundRequest) (RefundAnswer, error)
	AppendFact(ctx context.Context, f store.ExchangeAbandonedFact) error
}

// Inventory returns the target claim's capacity.
type Inventory interface {
	ReturnCapacity(ctx context.Context, org, hold, receipt uuid.UUID, quantity int32) (CapacityReturn, error)
}

// ChargeEvidence is payments' operation read. Found is false for its 404. Status is the
// terminal result of a resolved operation.
type ChargeEvidence struct {
	Found, Resolved                 bool
	Status                          string
	CapturedAmount, ConfirmedAmount *int64
	Currency, ConfirmedCurrency     *string
}

// LegEvidence is payments' refund-leg read. Found is false for its 404.
type LegEvidence struct {
	Found, Completed  bool
	Amount            int64
	Currency          string
	ConfirmedAmount   *int64
	ConfirmedCurrency *string
}

// RefundRequest is one partial refund.
type RefundRequest struct {
	OrganizerID          uuid.UUID
	ChargeKey, RefundKey string
	Amount               int64
	Currency             string
}

// RefundAnswer is payments' `refunded` answer.
type RefundAnswer struct {
	FactID   uuid.UUID
	Amount   int64
	Currency string
}

// CapacityReturn is inventory's answer to a capacity return.
type CapacityReturn struct {
	HoldID               uuid.UUID
	Quantity, Unreturned int32
}

var (
	// ErrUnavailable means a dependency could not be asked or did not answer usably. Nothing was
	// concluded from it. Run the same command again.
	ErrUnavailable = errors.New("dependency answer unavailable")
	// ErrRefused means a dependency conclusively refused one request. That request changed
	// nothing; the abandonment keeps the progress it recorded.
	ErrRefused = errors.New("dependency refused the request")
	// ErrProviderRefundFailed is the provider's terminal failure of the delta refund.
	ErrProviderRefundFailed = errors.New("provider refund failed (provider_refund_failed)")
	// ErrSourceNotAdmitted refuses an exchange whose source Access does not report as admitted
	// and fully issued: the switch may still happen, and this is not the abandonment's case.
	ErrSourceNotAdmitted = errors.New("access does not report the source as admitted and fully issued")
	// ErrReplacementIssued refuses an exchange whose replacement has issued tickets: Access
	// switched it, and its callback is late. The exchange sweep owns it.
	ErrReplacementIssued = errors.New("access reports issued replacement tickets")
	// ErrDeltaEvidence refuses money evidence that disagrees with the exchange's stored delta.
	ErrDeltaEvidence = errors.New("payments evidence disagrees with the exchange's delta")
	// ErrReplacementRefundLeg refuses a pending replacement refund for which payments holds a
	// leg: money may move on the replacement, which needs reconciliation first.
	ErrReplacementRefundLeg = errors.New("payments holds a leg for a pending replacement refund")
	// ErrCapacityReturnMismatch reports an inventory answer for another hold, another quantity,
	// or with capacity still unreturned.
	ErrCapacityReturnMismatch = errors.New("inventory's capacity return does not match the target claim")
)

// ProviderRefundFailedError carries the provider's reference for the failed refund.
type ProviderRefundFailedError struct{ ProviderRef string }

func (e *ProviderRefundFailedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrProviderRefundFailed, e.ProviderRef)
}

func (e *ProviderRefundFailedError) Unwrap() error { return ErrProviderRefundFailed }

// maxInspections bounds the inspect-and-reserve loop. A replacement refund set that keeps
// changing under it is an incident, not a race to win. After the third changed set the command
// stops with store.ErrReplacementRefundSetChanged and nothing reserved; the same command run
// again starts the inspection from the first refund. An earlier absence answer is not reused:
// payments can bind a leg between two runs (R2-5).
const maxInspections = 3

// The command's remote-call budget (R2-5). The CLI sizes its deadline from it.
const (
	// maxReplacementQuantity is the order quantity bound of the checkout and exchange contracts
	// (openapi `quantity` 1-50). Each pending replacement refund takes at least one unit, and
	// the refund binder counts pending refunds against the replacement's quantity.
	maxReplacementQuantity = 50
	// maxLegReadsPerInspection is one leg read per pending refund, plus the one read that finds
	// a leg and stops the command.
	maxLegReadsPerInspection = maxReplacementQuantity + 1
	// MaxRemoteCalls is the longest command, a fresh upgrade: two Access reads, the delta
	// capture read, maxInspections inspections, then the abandonment leg read, the partial
	// refund, the capacity return and the journal append. One of them is the partial refund.
	MaxRemoteCalls = 2 + 1 + maxInspections*maxLegReadsPerInspection + 4
	// DatabaseAllowance is the time for the command's Commerce transactions: at most twelve
	// short ones (load, gross-fact check, a list and a reservation per inspection, two progress
	// records, the reload and the terminal write). It was the whole command's budget before.
	DatabaseAllowance = 2 * time.Minute
)

// CommandDeadline is the time one command run needs when each remote call takes its full
// timeout: the one partial refund at refund, every other call at perCall, plus
// DatabaseAllowance (R3-3).
func CommandDeadline(perCall, refund time.Duration) time.Duration {
	return time.Duration(MaxRemoteCalls-1)*perCall + refund + DatabaseAllowance
}

// Service is the operator path. It holds no lease and runs on no timer.
type Service struct {
	db        *sql.DB
	access    Access
	payments  Payments
	inventory Inventory
}

func New(db *sql.DB, a Access, p Payments, i Inventory) *Service {
	return &Service{db: db, access: a, payments: p, inventory: i}
}

// Result is the exchange as the command left it. Replay reports a terminal abandonment this
// command did not change; it carries the original times and attribution.
type Result struct {
	Exchange store.SettledExchange
	Replay   bool
}

// Abandon is the whole operator act for one (organizer, exchange) and one trimmed attribution.
func (s *Service) Abandon(ctx context.Context, org, exchangeID uuid.UUID, attribution store.AbandonmentAttribution) (Result, error) {
	e, err := store.LoadSettledExchange(ctx, s.db, org, exchangeID)
	if err != nil {
		return Result{}, err
	}
	if e.AbandonmentStarted {
		// The pair is compared BEFORE a terminal replay or a stored failure is reported: only
		// the first pair owns the abandonment, before and after completion (D9).
		if e.AbandonmentOperator != attribution.Operator || e.AbandonmentReason != attribution.Reason {
			return Result{}, fmt.Errorf("%w: exchange %s", store.ErrAbandonmentAttributionConflict, exchangeID)
		}
		return s.drive(ctx, e)
	}
	e, err = s.reserve(ctx, e, attribution)
	if err != nil {
		return Result{}, err
	}
	return s.drive(ctx, e)
}

// reserve proves the fresh exchange and writes the reservation. A concurrent same-pair command
// may win the write; its committed reservation is then returned and resumed.
func (s *Service) reserve(ctx context.Context, e store.SettledExchange, attribution store.AbandonmentAttribution) (store.SettledExchange, error) {
	if err := e.FreshEligibility(); err != nil {
		return store.SettledExchange{}, err
	}
	if err := store.VerifyExchangeGrossFacts(ctx, s.db, e); err != nil {
		return store.SettledExchange{}, err
	}
	if err := s.proveAdmission(ctx, e); err != nil {
		return store.SettledExchange{}, err
	}
	if err := s.verifyDeltaEvidence(ctx, e); err != nil {
		return store.SettledExchange{}, err
	}
	for attempt := 1; ; attempt++ {
		inspected, err := s.inspectReplacementRefunds(ctx, e)
		if err != nil {
			return store.SettledExchange{}, err
		}
		cur, _, err := store.ReserveExchangeAbandonment(ctx, s.db, store.AbandonmentReservation{
			Expected: e, Attribution: attribution, InspectedRefunds: inspected})
		if errors.Is(err, store.ErrReplacementRefundSetChanged) && attempt < maxInspections {
			continue
		}
		return cur, err
	}
}

// proveAdmission makes the two ordered Access reads (D8). The source read comes FIRST; see the
// package comment for why the order is the proof.
func (s *Service) proveAdmission(ctx context.Context, e store.SettledExchange) error {
	source, err := s.access.Admission(ctx, e.OrganizerID, e.SourceOrderID)
	if err != nil {
		return fmt.Errorf("%w: source admission: %v", ErrUnavailable, err)
	}
	if source.IssuedCount != e.Quantity || !source.Admitted {
		return fmt.Errorf("%w: exchange %s: source admitted=%t issued_count=%d, quantity %d",
			ErrSourceNotAdmitted, e.ID, source.Admitted, source.IssuedCount, e.Quantity)
	}
	replacement, err := s.access.Admission(ctx, e.OrganizerID, e.ReplacementOrderID)
	if err != nil {
		return fmt.Errorf("%w: replacement admission: %v", ErrUnavailable, err)
	}
	if replacement.IssuedCount != 0 {
		return fmt.Errorf("%w: exchange %s: replacement issued_count=%d", ErrReplacementIssued, e.ID, replacement.IssuedCount)
	}
	return nil
}

// verifyDeltaEvidence reads the money the settlement moved, by the delta's sign. An even
// exchange moved none.
func (s *Service) verifyDeltaEvidence(ctx context.Context, e store.SettledExchange) error {
	switch {
	case e.DeltaAmount > 0:
		key := exchangeunwind.ChargeKey(e.ID)
		op, err := s.payments.ChargeOperation(ctx, e.OrganizerID, key)
		if err != nil {
			return err
		}
		// Payments records a successful charge as `captured` (server.go charge).
		if !op.Found || !op.Resolved || op.Status != "captured" || op.CapturedAmount == nil || op.Currency == nil {
			return fmt.Errorf("%w: exchange %s: no resolved capture under %s", ErrDeltaEvidence, e.ID, key)
		}
		if *op.CapturedAmount != e.DeltaAmount || *op.Currency != e.Currency ||
			!confirmationAgrees(op.ConfirmedAmount, op.ConfirmedCurrency, e.DeltaAmount, e.Currency) {
			return fmt.Errorf("%w: exchange %s: the capture under %s is not %d %s", ErrDeltaEvidence, e.ID, key, e.DeltaAmount, e.Currency)
		}
	case e.DeltaAmount < 0:
		key := exchangeunwind.RefundKey(e.ID)
		leg, err := s.payments.RefundLeg(ctx, e.OrganizerID, e.PaymentSourceKey, key)
		if err != nil {
			return err
		}
		if !leg.Found || !leg.Completed {
			return fmt.Errorf("%w: exchange %s: the downgrade refund %s is not completed", ErrDeltaEvidence, e.ID, key)
		}
		if leg.Amount != -e.DeltaAmount || leg.Currency != e.Currency ||
			!confirmationAgrees(leg.ConfirmedAmount, leg.ConfirmedCurrency, -e.DeltaAmount, e.Currency) {
			return fmt.Errorf("%w: exchange %s: the downgrade refund %s is not %d %s", ErrDeltaEvidence, e.ID, key, -e.DeltaAmount, e.Currency)
		}
	}
	return nil
}

// confirmationAgrees accepts a provider confirmation that matches, or legacy evidence with no
// confirmation at all. A confirmation with only one of its two fields is contradictory.
func confirmationAgrees(amount *int64, currency *string, wantAmount int64, wantCurrency string) bool {
	switch {
	case amount == nil && currency == nil:
		return true
	case amount == nil || currency == nil:
		return false
	default:
		return *amount == wantAmount && *currency == wantCurrency
	}
}

// inspectReplacementRefunds reads the replacement's refunds and asks payments about each
// pending one (D10). A completed refund refuses. A pending refund continues only when payments
// conclusively holds no leg for it: a found leg, bound or abandoned, refuses, and an answer that
// is not payments' own leg-absence answer stops for a retry.
func (s *Service) inspectReplacementRefunds(ctx context.Context, e store.SettledExchange) ([]store.ReplacementRefund, error) {
	refunds, err := store.ListReplacementRefunds(ctx, s.db, e.OrganizerID, e.ReplacementOrderID)
	if err != nil {
		return nil, err
	}
	for _, r := range refunds {
		if r.Status != "pending" {
			return nil, fmt.Errorf("%w: refund %s of exchange %s", store.ErrReplacementRefundCompleted, r.ID, e.ID)
		}
		leg, err := s.payments.RefundLeg(ctx, e.OrganizerID, e.ReplacementPaymentKey, r.ID.String())
		if err != nil {
			return nil, err
		}
		if leg.Found {
			return nil, fmt.Errorf("%w: refund %s of exchange %s", ErrReplacementRefundLeg, r.ID, e.ID)
		}
	}
	return refunds, nil
}

// drive resumes a reservation from its durable progress.
func (s *Service) drive(ctx context.Context, e store.SettledExchange) (Result, error) {
	if e.Abandoned {
		return Result{Exchange: e, Replay: true}, nil
	}
	if e.AbandonmentRefundFailedRef != "" {
		return Result{Exchange: e}, fmt.Errorf("%w: exchange %s (%s)", store.ErrAbandonmentRefundFailed, e.ID, e.AbandonmentRefundFailedRef)
	}
	// Before any remote effect: a resume skips fresh eligibility, and the rows can have changed
	// since the reservation (R1-5).
	if err := e.ResumeConsistency(); err != nil {
		return Result{Exchange: e}, err
	}
	org, id := e.OrganizerID, e.ID
	receipt := store.ExchangeAbandonmentRefundID(org, id)

	if e.DeltaAmount > 0 && e.AbandonmentRefundFactID == uuid.Nil {
		factID, err := s.refundDelta(ctx, e, receipt)
		var failed *ProviderRefundFailedError
		if errors.As(err, &failed) {
			if recErr := store.RecordExchangeAbandonmentRefundFailure(ctx, s.db, org, id, failed.ProviderRef); recErr != nil {
				return Result{Exchange: e}, fmt.Errorf("record the provider failure %s: %w", failed.ProviderRef, recErr)
			}
			return s.reload(ctx, e, fmt.Errorf("%w: exchange %s (%s)", store.ErrAbandonmentRefundFailed, id, failed.ProviderRef))
		}
		if err != nil {
			return Result{Exchange: e}, err
		}
		if err := store.RecordExchangeAbandonmentRefund(ctx, s.db, org, id, factID); err != nil {
			return Result{Exchange: e}, err
		}
	}

	if !e.AbandonmentTargetReturned {
		if err := s.returnTarget(ctx, e, receipt); err != nil {
			return Result{Exchange: e}, err
		}
		if _, err := store.RecordExchangeAbandonmentTargetReturned(ctx, s.db, org, id); err != nil {
			return Result{Exchange: e}, err
		}
	}

	// Rebuilt from the committed row: the occurred_at is the recorded target-return instant
	// and the refund fact the recorded one, so every retry sends identical bytes.
	e, err := store.LoadSettledExchange(ctx, s.db, org, id)
	if err != nil {
		return Result{}, err
	}
	// An identical command finished while this one made progress: its record is the answer.
	if e.Abandoned {
		return Result{Exchange: e, Replay: true}, nil
	}
	fact, err := store.BuildExchangeAbandonedFact(e)
	if err != nil {
		return Result{Exchange: e}, err
	}
	if err := s.payments.AppendFact(ctx, fact); err != nil {
		return Result{Exchange: e}, err
	}
	done, replay, err := store.FinishExchangeAbandonment(ctx, s.db, fact)
	if err != nil {
		return Result{Exchange: e}, err
	}
	return Result{Exchange: done, Replay: replay}, nil
}

func (s *Service) reload(ctx context.Context, e store.SettledExchange, cause error) (Result, error) {
	cur, err := store.LoadSettledExchange(ctx, s.db, e.OrganizerID, e.ID)
	if err != nil {
		return Result{Exchange: e}, cause
	}
	return Result{Exchange: cur}, cause
}

// refundDelta refunds an upgrade's delta against `exchange-charge:<id>` under the derived
// refund key, and returns payments' refund fact.
//
// The leg is read first. Payments' binder returns an existing leg before it rechecks the
// amount and currency, so a found leg must already match before the POST replays it. Payments'
// leg-absence answer permits the first bind.
func (s *Service) refundDelta(ctx context.Context, e store.SettledExchange, receipt uuid.UUID) (uuid.UUID, error) {
	chargeKey, refundKey := exchangeunwind.ChargeKey(e.ID), receipt.String()
	leg, err := s.payments.RefundLeg(ctx, e.OrganizerID, chargeKey, refundKey)
	if err != nil {
		return uuid.Nil, err
	}
	if leg.Found && (leg.Amount != e.DeltaAmount || leg.Currency != e.Currency ||
		!confirmationAgrees(leg.ConfirmedAmount, leg.ConfirmedCurrency, e.DeltaAmount, e.Currency)) {
		return uuid.Nil, fmt.Errorf("%w: exchange %s: the abandonment leg %s is not %d %s", ErrDeltaEvidence, e.ID, refundKey, e.DeltaAmount, e.Currency)
	}
	answer, err := s.payments.PartialRefund(ctx, RefundRequest{
		OrganizerID: e.OrganizerID, ChargeKey: chargeKey, RefundKey: refundKey,
		Amount: e.DeltaAmount, Currency: e.Currency,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if answer.Amount != e.DeltaAmount || answer.Currency != e.Currency {
		return uuid.Nil, fmt.Errorf("%w: payments refunded %d %s, the delta is %d %s",
			ErrUnavailable, answer.Amount, answer.Currency, e.DeltaAmount, e.Currency)
	}
	return answer.FactID, nil
}

// returnTarget returns the whole target claim under the derived receipt. Only an answer for
// exactly that hold and quantity, with nothing left unreturned, counts.
func (s *Service) returnTarget(ctx context.Context, e store.SettledExchange, receipt uuid.UUID) error {
	out, err := s.inventory.ReturnCapacity(ctx, e.OrganizerID, e.TargetHoldID, receipt, e.Quantity)
	if err != nil {
		return err
	}
	if out.HoldID != e.TargetHoldID || out.Quantity != e.Quantity || out.Unreturned != 0 {
		return fmt.Errorf("%w: exchange %s: inventory answered hold %s quantity %d unreturned %d, want hold %s quantity %d unreturned 0",
			ErrCapacityReturnMismatch, e.ID, out.HoldID, out.Quantity, out.Unreturned, e.TargetHoldID, e.Quantity)
	}
	return nil
}
