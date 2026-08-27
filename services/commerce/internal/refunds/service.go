// Package refunds is the single unit of work for refunding one completed order.
//
// It exists because there are now TWO callers — the staff endpoint
// (POST /internal/orders/{id}/refunds) and the event-cancellation bulk runner (TKT-159) —
// and a second implementation of a money path is a second thing that can be wrong about
// idempotency, ordering or journalling. The bulk runner calls this; it does not re-compose
// the protocol, and it does not call commerce's own HTTP handler over the loopback either
// (that would add startup-ordering, token and response-decoding failure modes to a path
// that has none).
//
// The protocol, unchanged from TKT-156/157/161: bind under the order row lock → move the
// money through payments' partial-refund leg → append the compensating fact → record the
// completion → discharge the reversal, tickets first and capacity only after (ADR-038 §1:
// freeing the seat while the ticket still admits is the one ordering that can oversell).
package refunds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"database/sql"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Caller performs a service-to-service HTTP call. A function rather than an interface so
// the API server can pass its existing `call` method unchanged — the transport, its
// timeouts and its internal-token handling stay in one place.
type Caller func(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error)

// The money leg's failure modes, kept distinct because the HTTP handler answers each with
// a different status and the bulk runner records each with a different outcome code.
var (
	// ErrPaymentsRefused is a refusal payments is sure about — the money did not move.
	ErrPaymentsRefused = errors.New("payments refused the refund")
	// ErrPaymentsUnresolved is a transport or unexpected-status answer. Whether the money
	// moved is unknown, so the refund row stays pending and a replay resolves it.
	ErrPaymentsUnresolved = errors.New("payments refund unresolved")
	// ErrJournalUnavailable reports that the compensating fact could not be appended.
	ErrJournalUnavailable = errors.New("journal unavailable")
	// ErrPersist reports that the completion could not be recorded locally.
	ErrPersist = errors.New("persist refund")
)

// Service refunds one order at a time.
type Service struct {
	db                                   *sql.DB
	call                                 Caller
	paymentsURL, accessURL, inventoryURL string
}

func New(db *sql.DB, call Caller, payments, access, inventory string) *Service {
	return &Service{
		db: db, call: call,
		paymentsURL:  strings.TrimSuffix(payments, "/"),
		accessURL:    strings.TrimSuffix(access, "/"),
		inventoryURL: strings.TrimSuffix(inventory, "/"),
	}
}

// Result is one refund attempt's outcome. Replay reports that the refund's money leg had
// already completed before this call — the reversal may still have been driven, which is
// how an outstanding obligation gets retried.
type Result struct {
	Refund commercestore.Refund
	Replay bool
}

// Refund binds (or replays) a refund and drives the whole protocol.
//
// Money first, then the trail: the refund row is durable before the provider call, so a
// crash between them leaves a pending refund that a replay resolves — never money moved
// with no record that it was owed.
func (s *Service) Refund(ctx context.Context, in commercestore.RefundRequest) (Result, error) {
	refund, err := commercestore.BindOrderRefund(ctx, s.db, in)
	if err != nil {
		return Result{}, err
	}
	if refund.Completed {
		// The money is done, but the reversal may not be: a replay is how outstanding
		// ticket voiding gets retried (TKT-157). Drive it, then answer.
		return Result{Refund: s.DriveReversal(ctx, refund), Replay: true}, nil
	}

	factID, err := s.refundPayment(ctx, refund)
	if err != nil {
		return Result{}, err
	}
	if err := s.refundFact(ctx, refund); err != nil {
		return Result{}, err
	}
	if err := commercestore.CompleteOrderRefund(ctx, s.db, in.OrganizerID, refund.ID, factID); err != nil {
		slog.Default().ErrorContext(ctx, "complete order refund", "err", err)
		return Result{}, ErrPersist
	}
	// The money is durable from here on. The reversal is a SEPARATE obligation: if it
	// fails, the refund stays valid and outstanding rather than the whole request failing
	// and inviting a retry that would re-examine money that already moved.
	return Result{Refund: s.DriveReversal(ctx, refund)}, nil
}

// VoidResult is one comped-order void and whether it was a replay.
type VoidResult struct {
	Void   commercestore.OrderVoid
	Replay bool
}

// Void reverses a comped order: it voids the tickets and returns the capacity,
// and it moves no money (TKT-171).
//
// Compare Refund above and note what is MISSING here rather than what is present:
// no refundPayment, no refundFact, no CompleteOrderRefund. A void has no money
// leg to move, no compensating fact to append and no completion to record,
// because nothing was captured. ADR-003: the journal records what happened, not
// what didn't.
//
// That makes this method shorter than Refund by exactly the money protocol, which
// is the point — the two share the reversal and nothing else.
func (s *Service) Void(ctx context.Context, in commercestore.VoidRequest) (VoidResult, error) {
	existing, found, err := commercestore.LookupOrderVoid(ctx, s.db, in.OrganizerID, in.OrderID)
	if err != nil {
		return VoidResult{}, err
	}
	v, err := commercestore.BindOrderVoid(ctx, s.db, in)
	if err != nil {
		return VoidResult{}, err
	}
	// A replay is how an outstanding obligation gets retried, exactly as for a
	// refund: drive it again and answer with the progress that resulted.
	return VoidResult{Void: s.DriveVoid(ctx, v), Replay: found && existing.ID == v.ID}, nil
}

// DriveReversal discharges a refund's two downstream obligations, IN ORDER.
//
// The order is a safety property, not a preference (ADR-038 §1): freeing the seat while
// the original ticket still admits is the one sequence that can OVERSELL. Voiding first
// can only under-sell. So the capacity return is attempted only once voiding has actually
// happened — which is why this is a guard and not a comment, and why an access outage
// leaves BOTH obligations outstanding rather than letting the second run without the first.
//
// Neither leg fails the call. The money has moved and the refund row is durable, so a
// downstream failure answers with the obligation still outstanding — visible, and
// retryable by replaying the same idempotency key.
func (s *Service) DriveReversal(ctx context.Context, refund commercestore.Refund) commercestore.Refund {
	rev := s.driveOrderedReversal(ctx, reversal{
		OperationID: refund.ID, OrderID: refund.OrderID, OrganizerID: refund.OrganizerID,
		HoldID: refund.HoldID, Quantity: refund.Quantity,
		TicketsVoided: refund.TicketsVoided, CapacityReturned: refund.CapacityReturned,
		markVoided: func(ctx context.Context) error {
			return commercestore.MarkRefundTicketsVoided(ctx, s.db, refund.OrganizerID, refund.ID)
		},
		markReturned: func(ctx context.Context) error {
			return commercestore.MarkRefundCapacityReturned(ctx, s.db, refund.OrganizerID, refund.ID)
		},
	})
	refund.TicketsVoided, refund.CapacityReturned = rev.TicketsVoided, rev.CapacityReturned
	return refund
}

// reversal is what the ordered driver needs to know about an operation, so the
// ordering exists ONCE for both the refund and the comped-order void (TKT-171).
//
// A void is not a refund — it has no amount, currency or payment fact, and
// migration 0025 gives it nowhere to put one — but its two downstream obligations
// are the same two, in the same order, for the same oversell reason. Copying
// DriveReversal for it would copy ADR-038 §1's guarantee into a second place that
// can drift; this makes the guarantee singular and the operations plural.
//
// OperationID is what both downstream services receive as `refund_id`. That field
// is their idempotency/correlation key, not a claim that money moved: access uses
// it to derive a deterministic ticket selection and event id, inventory for claim
// history. Neither writes a money fact.
type reversal struct {
	OperationID uuid.UUID
	OrderID     uuid.UUID
	OrganizerID uuid.UUID
	HoldID      uuid.UUID
	Quantity    int32

	TicketsVoided    bool
	CapacityReturned bool

	// markVoided and markReturned persist each discharged obligation. Passed in
	// rather than switched on a type: the driver must not know which table it is
	// writing to, which is what stops it growing a money branch.
	markVoided   func(context.Context) error
	markReturned func(context.Context) error
}

// driveOrderedReversal discharges the two downstream obligations, IN ORDER.
//
// The order is a safety property, not a preference (ADR-038 §1): freeing the seat
// while the original ticket still admits is the one sequence that can OVERSELL.
// Voiding first can only under-sell. So the capacity return is attempted only
// once voiding has ACTUALLY happened — which is why the early return is a guard
// and not a comment, and why an access outage leaves BOTH obligations outstanding
// rather than letting the second run without the first.
//
// Neither leg fails the call: the caller's durable row already exists, so a
// downstream failure answers with the obligation still outstanding — visible, and
// retryable by replaying the same operation id.
func (s *Service) driveOrderedReversal(ctx context.Context, rev reversal) reversal {
	rev = s.discharge(ctx, rev, obligationTickets)
	if !rev.TicketsVoided {
		return rev
	}
	return s.discharge(ctx, rev, obligationCapacity)
}

type obligation int

const (
	obligationTickets obligation = iota
	obligationCapacity
)

// discharge performs one leg: the downstream call, then the durable marker.
//
// Two facts about the legs it replaced, kept because they are the reason each
// behaves as it does:
//
//   - ACCESS answers 503 when issuance has not caught up. Its outbox/JetStream
//     path is asynchronous, so a prompt reversal genuinely can outrun issuance.
//     That is a "not yet", not a "nothing to void", and it must leave the
//     obligation outstanding — which the non-200 branch below does, and which is
//     what stops the capacity leg from running against un-voided tickets.
//   - INVENTORY refuses a partial return of a SEATED claim, and that obligation
//     then stays outstanding forever: nothing associates an issued ticket with a
//     seat identity, so no subset of seats can be derived (TKT-164). That is a
//     deliberate under-sell — the buyer keeps their money and the tickets stay
//     void — rather than refusing the reversal itself to protect a resale.
//
// A failure to record a SUCCESSFUL downstream call is deliberately not fatal and
// not marked: the downstream did the work, only our record of it failed. A replay
// re-drives it, which answers as a replay, and re-marks. Marking optimistically
// instead would be the one way to lose the ordering guarantee — capacity could
// then be returned on the strength of a voiding that never happened.
func (s *Service) discharge(ctx context.Context, rev reversal, which obligation) reversal {
	var (
		url  string
		done bool
	)
	switch which {
	case obligationTickets:
		if rev.TicketsVoided || s.accessURL == "" {
			return rev
		}
		url = fmt.Sprintf("%s/internal/orders/%s/refunds", s.accessURL, rev.OrderID)
	case obligationCapacity:
		if rev.CapacityReturned || s.inventoryURL == "" || rev.HoldID == uuid.Nil {
			return rev
		}
		url = fmt.Sprintf("%s/internal/holds/%s/refund-capacity", s.inventoryURL, rev.HoldID)
	}

	body := map[string]any{
		"organizer_id": rev.OrganizerID,
		"refund_id":    rev.OperationID,
		"quantity":     rev.Quantity,
	}
	code, _, err := s.call(ctx, http.MethodPost, url, "", body, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "reversal obligation not discharged; left outstanding",
			"operation_id", rev.OperationID, "order_id", rev.OrderID, "obligation", which,
			"status", code, "err", err)
		return rev
	}

	mark := rev.markVoided
	if which == obligationCapacity {
		mark = rev.markReturned
	}
	if err := mark(ctx); err != nil {
		slog.Default().ErrorContext(ctx, "record discharged reversal obligation",
			"operation_id", rev.OperationID, "obligation", which, "err", err)
		return rev
	}
	done = true
	if which == obligationTickets {
		rev.TicketsVoided = done
	} else {
		rev.CapacityReturned = done
	}
	return rev
}

// DriveVoid is the comped-order adapter over the same ordered driver (TKT-171).
//
// It reaches no money code by construction: there is no provider call and no
// journal append on this path, and OrderVoid carries no amount to make one from.
func (s *Service) DriveVoid(ctx context.Context, v commercestore.OrderVoid) commercestore.OrderVoid {
	rev := s.driveOrderedReversal(ctx, reversal{
		OperationID: v.ID, OrderID: v.OrderID, OrganizerID: v.OrganizerID,
		HoldID: v.HoldID, Quantity: v.Quantity,
		TicketsVoided: v.TicketsVoided, CapacityReturned: v.CapacityReturned,
		markVoided: func(ctx context.Context) error {
			return commercestore.MarkVoidTicketsVoided(ctx, s.db, v.OrganizerID, v.ID)
		},
		markReturned: func(ctx context.Context) error {
			return commercestore.MarkVoidCapacityReturned(ctx, s.db, v.OrganizerID, v.ID)
		},
	})
	v.TicketsVoided, v.CapacityReturned = rev.TicketsVoided, rev.CapacityReturned
	return v
}

// refundPayment moves the money through payments' partial-refund leg. The source key is
// the ORDER's checkout idempotency key, which is the key payments bound the charge
// operation under; the leg key is the refund's own identity, so a retry converges on one
// provider refund.
func (s *Service) refundPayment(ctx context.Context, refund commercestore.Refund) (uuid.UUID, error) {
	body := map[string]any{
		"organizer_id":    refund.OrganizerID,
		"idempotency_key": refund.PaymentSourceKey,
		"refund_key":      refund.ID.String(),
		"amount":          refund.Amount,
		"currency":        refund.Currency,
	}
	code, out, err := s.call(ctx, http.MethodPost, s.paymentsURL+"/internal/psp/partial-refund", "", body, true)
	if err != nil {
		return uuid.Nil, ErrPaymentsUnresolved
	}
	if code == http.StatusConflict {
		return uuid.Nil, ErrPaymentsRefused
	}
	if code != http.StatusOK {
		return uuid.Nil, ErrPaymentsUnresolved
	}
	var decoded struct {
		FactID uuid.UUID `json:"fact_id"`
	}
	if json.Unmarshal(out, &decoded) != nil || decoded.FactID == uuid.Nil {
		return uuid.Nil, ErrPaymentsUnresolved
	}
	return decoded.FactID, nil
}

// refundFact appends commerce's own compensating fact. It does NOT reuse the checkout
// fact helper: that derives its id from (order, type) and always journals the
// reservation's full total, so a second partial refund of the same order would collide on
// the id and carry the wrong amount. occurred_at is the refund row's stable created_at,
// never the clock — the journal compares the whole canonical fact on replay.
func (s *Service) refundFact(ctx context.Context, refund commercestore.Refund) error {
	factID := commercestore.RefundFactID(refund.ID)
	occurred := refund.CreatedAt.UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
		VALUES($1,$2,$3,$4,'order.refunded',$5,$6,$7) ON CONFLICT DO NOTHING`,
		factID, refund.OrderID, refund.OrganizerID, refund.BuyerID, refund.Amount, refund.Currency, occurred); err != nil {
		return ErrJournalUnavailable
	}
	code, _, err := s.call(ctx, http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{
		"fact_id": factID, "organizer_id": refund.OrganizerID, "fact_type": "order.refunded",
		"buyer_id": refund.BuyerID, "amount": refund.Amount, "currency": refund.Currency,
		"occurred_at": occurred.Format(time.RFC3339Nano),
		// order_id only — the journal allows no other payload key (a deliberate PII
		// guard), and RefundFactID already ties this fact to its refund.
		"payload": map[string]string{"order_id": refund.OrderID.String()},
	}, true)
	if err != nil || code != 200 {
		return ErrJournalUnavailable
	}
	return nil
}
