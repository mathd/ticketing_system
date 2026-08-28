package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/access/internal/delivery"
	"ticketing/services/access/internal/store"
	"ticketing/shared/httpx"
)

// Staff-triggered redelivery (TKT-203, ADR-068). A box-office agent re-sends a
// completed order's tickets when the original mail was lost or spam-filed.
//
// Internal: the gateway denies /internal/* at the edge (ADR-002), and a missing or
// wrong credential reads as 404 — access's own convention on this surface, already
// used by the refund route, so an unauthenticated caller cannot even learn the route
// exists. Deliberately NOT the 401 inventory answers: copying another service's
// refusal here would assert nothing and would disclose that the route exists.
//
// THE DESTINATION IS NOT A FIELD ON THIS REQUEST, and cannot be. Access resolves the
// address per ticket from commerce at send time. There is nothing to validate because
// there is nothing to submit — the difference between "the client may not influence
// this" and "the client may influence this only by lying in a way I check for".

// staffWriteHeader carries the back office's access credential (ADR-068).
//
// Distinct from X-Internal-Token, which opens every service's internal surface and is
// deliberately withheld from an internet-facing SSR process. Distinct from
// X-Scanner-Token, which admits at a door. This one opens exactly one operation, and
// the router walk in staff_credential_test.go is what keeps that true.
//
// Why a NEW credential rather than reusing commerce's or catalog's: the back office
// holds nothing for access today, so any reuse would grant a new capability class
// across a service boundary and make a commerce-token compromise into the power to
// re-emit ticket capabilities. That is ADR-057's premise, and it transfers exactly.
const staffWriteHeader = "X-Access-Staff-Write-Token"

// WithStaffWriteCredential supplies the back office's credential. An option rather
// than a New parameter, for the reason inventory records: another positional string
// beside the internal token is one more thing a call site can pass in the wrong
// order — with a credential, silently.
func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

// staffWriteOperations is the allowlist ADR-068 grants the back office's credential,
// keyed by chi's route pattern. One entry.
//
// A TABLE rather than a check inlined in the one handler, because the property that
// matters is a statement about the whole internal surface — "this credential opens
// these operations and no others" — and a property about a set cannot be enforced from
// inside one member of it. ADR-053 recorded the failure this avoids: there, the
// allowance widened while every route-level test stayed green, because each
// hand-mounted handler carried its own check and went on refusing for its own reason.
// With the grant in one table, widening it means editing this line, and the
// enumeration test in staff_credential_test.go reads the same table.
var staffWriteOperations = map[string]bool{
	"POST /internal/orders/{id}/redeliveries": true,
}

// staffOrInternal reports whether the caller may drive the operation chi matched.
//
// The staff credential is accepted ONLY on an operation staffWriteOperations names;
// the shared internal token is accepted on any internal route, as it always was
// (ADR-068 adds a credential, it does not replace one). So a route added later gets
// the shared token's behaviour by default and the staff credential never — closed by
// default, opened by a visible edit.
//
// Both arms are evaluated before either is consulted, deliberately: a short-circuit
// would skip a constant-time comparison and make the refusal's timing depend on which
// credential was presented. Both fail closed on an unconfigured value — a service
// started without a credential must refuse everyone rather than admit anyone
// presenting nothing.
func (s *Server) staffOrInternal(r *http.Request) bool {
	internal := httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token)
	staff := httpx.HeaderCredentialMatches(r, staffWriteHeader, s.staffWriteToken)
	return internal || (staff && staffWriteOperations[routePattern(r)])
}

// routePattern is the chi pattern that matched, e.g.
// "POST /internal/orders/{id}/redeliveries". Empty when there is no route context,
// which makes the staff arm above fail closed.
func routePattern(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return ""
	}
	return rc.RouteMethod + " " + rc.RoutePattern()
}

type ticketRedeliveryRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
}

func (s *Server) redeliverOrderTickets(w http.ResponseWriter, r *http.Request) {
	if !s.staffOrInternal(r) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in ticketRedeliveryRequest
	if err := httpx.DecodeJSON(w, r, &in, 4<<10); err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if in.OrganizerID == uuid.Nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "organizer_id required"})
		return
	}

	out, err := s.redeliver(r.Context(), in.OrganizerID, order, key)
	switch {
	case errors.Is(err, store.ErrTicketsNotIssued):
		// 503, not 404: issuance is asynchronous, so a resend can outrun it and
		// "this order has no tickets" is usually "not yet". Telling an operator the
		// order does not exist, seconds after a real checkout, would send them
		// looking for a problem that is not there. Same reasoning as the refund
		// route, which reached it first.
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "tickets not issued yet"})
		return
	case errors.Is(err, store.ErrRedeliveryKeyConflict):
		write(w, http.StatusConflict, map[string]string{"error": "idempotency key reused with a different order"})
		return
	case errors.Is(err, store.ErrRedeliveryBoundExceeded):
		write(w, http.StatusTooManyRequests, map[string]string{"error": "this order has reached its redelivery limit"})
		return
	case err != nil:
		// Deliberately opaque. The failure may have come from the address lookup or
		// the transport, and naming which would tell a caller whose only legitimate
		// input is an order id something about the buyer's contact record.
		write(w, http.StatusInternalServerError, map[string]string{"error": "redeliver tickets"})
		return
	}
	// A count and a replay flag. NOT the recipient, NOT a capability link, NOT the
	// message ids: the caller has no use for any of them and every one of them is a
	// thing that must not cross this boundary (COS-6).
	write(w, http.StatusOK, map[string]any{
		"order_id": out.OrderID.String(), "ticket_count": out.TicketCount, "replay": out.Replay,
	})
}

// redeliveryStore is the two store operations the resend drives.
//
// An interface for the same reason scannerDeviceStore is one, and with the same caveat:
// it is NOT where the guarantee lives. The row-level guarantees — the attempt table's
// uniqueness, the per-order bound, the accepted_at read that makes a replay a resume,
// the signed chain — are SQL, and they are asserted against real PostgreSQL in the store
// tier. A fake enforcing them in Go would prove the fake and the handler agree.
//
// What it buys is the one thing no database assertion can make: a test that drives the
// REAL handler and watches the transport. Before this existed, the API tests reproduced
// the handler's send loop and asserted against their own copy — so reverting the shipped
// handler to return early on a replay left them green (ai-review pass 2). A test whose
// assertions are facts about the test is not a test of the contract.
type redeliveryStore interface {
	ClaimRedelivery(ctx context.Context, org, order uuid.UUID, key string) (store.RedeliveryClaim, error)
	MarkRedelivered(ctx context.Context, org uuid.UUID, key string, ticketID, messageID uuid.UUID) error
}

// WithRedeliveryStore replaces the redelivery store port. Tests only: production passes
// the *store.Postgres to New and gets it from there.
func (s *Server) WithRedeliveryStore(rs redeliveryStore) *Server {
	s.redeliveries = rs
	return s
}

// redeliveryResult is what the handler reports. A count, never the tickets.
type redeliveryResult struct {
	OrderID     uuid.UUID
	TicketCount int
	Replay      bool
}

// redeliver claims the request, sends each ticket, and records each acceptance.
//
// The ORDER of the three steps is the contract, not an implementation detail:
//
//  1. Claim first. It binds the idempotency key, enforces the per-order bound and
//     mints one message id per ticket, all under the order's ticket locks. Sending
//     before claiming would let a double-click send twice and argue about it after.
//  2. Send. On a replay the claim reports it and NOTHING is sent — that is what makes
//     the double-click one mail rather than two.
//  3. Mark each acceptance individually, AFTER the transport took it. Appending the
//     trail before the send would make it claim a delivery that had not happened
//     (ADR-021: the trail's claim must match what happened).
//
// The window between 2 and 3 is real and is not closable here: a crash after the
// transport accepted and before the mark leaves a sent mail with no lifecycle event.
// Recovery is a retry under the SAME idempotency key, which re-derives the SAME
// message id, so a transport that deduplicates on message id will not send twice.
// That is a requirement ON the transport, and this repo's only transport is a logger
// (see the delivery package) — so it is written down, not claimed as closed.
func (s *Server) redeliver(ctx context.Context, org, order uuid.UUID, key string) (redeliveryResult, error) {
	if s.addresses == nil || s.mailer == nil || s.redeliveries == nil {
		// Fail closed. A server built without a transport must refuse rather than
		// report success for mail nobody sent — the shape authenticateScannerDevice
		// already uses for a missing enrolment port.
		return redeliveryResult{}, errors.New("redelivery is not configured")
	}
	claim, err := s.redeliveries.ClaimRedelivery(ctx, org, order, key)
	if err != nil {
		return redeliveryResult{}, err
	}
	// A replay is a RESUME, not a no-op. Returning success here because the key was
	// seen before would report the whole order delivered on a request that died after
	// its first ticket — the claim commits before any send, so outstanding rows are a
	// state the system genuinely reaches (ai-review F2). Only the tickets the
	// transport has not accepted are sent, under their ORIGINAL message ids, so a
	// transport that deduplicates on message id cannot deliver twice.
	for _, tk := range claim.Outstanding() {
		email, err := s.addresses.DeliveryEmail(ctx, tk.BuyerID)
		if err != nil {
			return redeliveryResult{}, err
		}
		if err := s.mailer.Send(ctx, tk.MessageID, email, delivery.TicketLink(s.publicURL, tk.GuestOrderRef)); err != nil {
			return redeliveryResult{}, err
		}
		if err := s.redeliveries.MarkRedelivered(ctx, org, key, tk.TicketID, tk.MessageID); err != nil {
			return redeliveryResult{}, err
		}
	}
	return redeliveryResult{OrderID: claim.OrderID, TicketCount: len(claim.Tickets), Replay: claim.Replay}, nil
}
