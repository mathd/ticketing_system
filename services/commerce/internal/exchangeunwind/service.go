// Package exchangeunwind decides whether a wedged exchange may be unwound, and unwinds it.
//
// It is its own package rather than an arm of `internal/recovery`, and that follows the
// decision ADR-062 §1 and ADR-063 §1 each took for their own runner: different eligibility,
// different terminal states, and one state machine serving several lifecycles would be
// readable by nobody. `recovery`'s Payments port carries Void and Refund — compensation
// verbs this must never reach for — so borrowing it would hand the unwind the ability to
// move money in order to ask whether money moved.
//
// THE PORT IS TWO READ METHODS AND THE NARROWNESS IS THE GUARANTEE. Both bind nothing, call
// no provider, and append no fact; payments documents them as safe for exactly this use. A
// unit that cannot express a write cannot perform one by accident, which is the same
// structural argument ADR-063 §2 makes for `DriveExchange` refusing an unswitched row.
package exchangeunwind

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// MoneyEvidence is what payments answered about one deterministic key.
//
// THREE OUTCOMES, NOT TWO, and collapsing them is the defect this type exists to prevent.
// "Absent" is a positive proof that no provider operation was ever bound. "Present" is proof
// one was. "Indeterminate" is everything else — a transport failure, a 5xx, a malformed
// body, or an operation bound but not resolved. Only Absent permits an unwind; the other two
// refuse. But they refuse for opposite reasons and an operator needs to know which: Present
// means a charged buyer and a compensation decision nobody has taken, Indeterminate means
// payments could not be asked and the answer is simply not known yet.
type MoneyEvidence int

const (
	// Indeterminate is the ZERO VALUE on purpose. A caller that forgets to set the
	// evidence, or a future branch that falls through without deciding, refuses the unwind
	// rather than permitting it. The guard fails closed by construction, not by discipline.
	Indeterminate MoneyEvidence = iota
	Absent
	Present
)

func (m MoneyEvidence) String() string {
	switch m {
	case Absent:
		return "absent"
	case Present:
		return "present"
	default:
		return "indeterminate"
	}
}

// Payments is the read-only evidence surface. Two methods, deliberately.
//
// The DELTA'S SIGN selects which one to call, because an upgrade and a downgrade record
// their money in different payments tables reached by different endpoints. A check that
// consulted only the operation read would find nothing for every downgrade and unwind a
// buyer who had already been refunded — the failure this split exists to make impossible.
type Payments interface {
	// LookupChargeOperation answers for an upgrade's charge, keyed
	// `exchange-charge:<exchange id>`.
	LookupChargeOperation(ctx context.Context, org uuid.UUID, key string) (MoneyEvidence, error)
	// LookupRefundLeg answers for a downgrade's partial-refund leg, addressed by the SOURCE
	// order's checkout key together with `exchange-refund:<exchange id>`. All three
	// parameters are required by payments: (organizer, source key) identifies a charge that
	// may carry many legs, and only the refund key distinguishes them.
	LookupRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (MoneyEvidence, error)
}

// Service is the operator path. It holds no lease, runs on no timer, and is driven only by
// a human running a command.
type Service struct {
	db       *sql.DB
	payments Payments
}

func New(db *sql.DB, p Payments) *Service {
	return &Service{db: db, payments: p}
}

// ChargeKey and RefundKey derive the deterministic payments keys the exchange path used.
// They are the same expressions `settleExchangeDelta` builds, and they are here rather than
// inlined so the unwind and the settlement cannot drift: if these ever disagree with the
// handler, the unwind asks payments about an operation that does not exist and reads its
// absence as proof of safety.
func ChargeKey(exchangeID uuid.UUID) string { return "exchange-charge:" + exchangeID.String() }
func RefundKey(exchangeID uuid.UUID) string { return "exchange-refund:" + exchangeID.String() }

// Evidence asks payments whether this exchange's delta moved any money.
//
// The three branches mirror `settleExchangeDelta` exactly, and that correspondence is the
// correctness argument — the unwind asks about precisely the call the settlement would have
// made:
//
//   - no basis recorded → NOTHING was called. The basis is persisted before the provider
//     (ADR-039 §3c), so an exchange that never recorded one never reached payments. This is
//     proof from ordering, not from a read, and it is the only branch that answers Absent
//     without asking.
//   - delta > 0 → an upgrade charged `exchange-charge:<id>`.
//   - delta < 0 → a downgrade refunded through `exchange-refund:<id>`.
//   - delta == 0 → an even exchange calls nobody at all.
func (s *Service) Evidence(ctx context.Context, w store.WedgedExchange) (MoneyEvidence, error) {
	if !w.BasisRecorded {
		return Absent, nil
	}
	switch {
	case w.DeltaAmount > 0:
		return s.payments.LookupChargeOperation(ctx, w.OrganizerID, ChargeKey(w.ID))
	case w.DeltaAmount < 0:
		return s.payments.LookupRefundLeg(ctx, w.OrganizerID, w.PaymentSourceKey, RefundKey(w.ID))
	default:
		return Absent, nil
	}
}

// ErrMoneyIndeterminate reports that payments could not be asked, or answered something
// other than a clean absence or a clean presence. Distinct from
// store.ErrExchangeMoneyMoved: that one means the buyer WAS charged, this one means nobody
// knows yet. Retrying later may resolve it; unwinding must not.
var ErrMoneyIndeterminate = errors.New("payments evidence is indeterminate")

// Unwind is the whole operator act: read the exchange, establish the money fact against
// payments, and remove the binding if and only if the answer is a clean absence.
//
// The read happens BEFORE the transaction and the state is re-checked INSIDE it. Neither
// half is redundant. The payments call cannot happen under a database lock — it is another
// service, and holding the source order's row lock across a network round trip would block
// every checkout for that order on payments' latency. So the honest guarantee is the one
// stated in the store: the evidence was true when it was read, and the write window is one
// transaction wide.
func (s *Service) Unwind(ctx context.Context, org, exchangeID uuid.UUID, reason string) (store.WedgedExchange, MoneyEvidence, error) {
	w, err := store.LoadWedgedExchange(ctx, s.db, org, exchangeID)
	if err != nil {
		return store.WedgedExchange{}, Indeterminate, err
	}
	evidence, err := s.Evidence(ctx, w)
	if err != nil {
		return w, Indeterminate, fmt.Errorf("%w: %v", ErrMoneyIndeterminate, err)
	}
	switch evidence {
	case Present:
		return w, evidence, fmt.Errorf("%w: %s", store.ErrExchangeMoneyMoved, exchangeID)
	case Absent:
		// The only branch that proceeds. Written as an explicit case rather than as the
		// default so that a MoneyEvidence value added later refuses until somebody decides
		// what it means.
	default:
		return w, evidence, fmt.Errorf("%w for %s", ErrMoneyIndeterminate, exchangeID)
	}
	if err := store.UnwindWedgedExchange(ctx, s.db, org, exchangeID, reason, false); err != nil {
		return w, evidence, err
	}
	return w, evidence, nil
}
