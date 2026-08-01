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
	refund = s.voidRefundedTickets(ctx, refund)
	if !refund.TicketsVoided {
		return refund
	}
	return s.returnRefundedCapacity(ctx, refund)
}

// returnRefundedCapacity gives the seat back. A partial return of a SEATED claim is
// refused by inventory and stays outstanding forever: nothing associates an issued ticket
// with a seat identity, so no subset of seats can be derived (TKT-164). That is a
// deliberate under-sell — the buyer keeps their refund and the tickets stay void — rather
// than refusing the refund itself to protect a resale.
func (s *Service) returnRefundedCapacity(ctx context.Context, refund commercestore.Refund) commercestore.Refund {
	if refund.CapacityReturned || s.inventoryURL == "" || refund.HoldID == uuid.Nil {
		return refund
	}
	code, _, err := s.call(ctx, http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/refund-capacity", s.inventoryURL, refund.HoldID), "",
		map[string]any{"organizer_id": refund.OrganizerID, "refund_id": refund.ID, "quantity": refund.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "refund capacity not returned; left outstanding",
			"refund_id", refund.ID, "hold_id", refund.HoldID, "status", code, "err", err)
		return refund
	}
	if err := commercestore.MarkRefundCapacityReturned(ctx, s.db, refund.OrganizerID, refund.ID); err != nil {
		// Inventory returned it; only our record of it failed. A replay re-drives
		// inventory, which answers as a replay, and re-marks.
		slog.Default().ErrorContext(ctx, "record refund capacity return", "refund_id", refund.ID, "err", err)
		return refund
	}
	refund.CapacityReturned = true
	return refund
}

// voidRefundedTickets discharges the ticket-voiding half of a reversal, and never fails
// the call. The money has already moved and the refund row is durable, so the honest answer
// to a failure here is a successful refund reporting `tickets_voided:false` — visible, and
// retryable by replaying the same idempotency key.
//
// Access answers 503 when issuance has not caught up (its outbox/JetStream path is
// asynchronous, so a prompt refund genuinely can outrun it). That is a "not yet", not a
// "nothing to void", and it must leave the obligation outstanding.
func (s *Service) voidRefundedTickets(ctx context.Context, refund commercestore.Refund) commercestore.Refund {
	if refund.TicketsVoided || s.accessURL == "" {
		return refund
	}
	code, _, err := s.call(ctx, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/refunds", s.accessURL, refund.OrderID), "",
		map[string]any{"organizer_id": refund.OrganizerID, "refund_id": refund.ID, "quantity": refund.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "refund tickets not voided; left outstanding",
			"refund_id", refund.ID, "order_id", refund.OrderID, "status", code, "err", err)
		return refund
	}
	if err := commercestore.MarkRefundTicketsVoided(ctx, s.db, refund.OrganizerID, refund.ID); err != nil {
		// Access voided them; only our record of it failed. A replay re-drives access,
		// which answers as a replay, and re-marks — so this is recoverable, not lost.
		slog.Default().ErrorContext(ctx, "record refund ticket voiding", "refund_id", refund.ID, "err", err)
		return refund
	}
	refund.TicketsVoided = true
	return refund
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
