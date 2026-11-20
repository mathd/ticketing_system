//go:build smoke

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
	"ticketing/shared/httpx"
)

// TKT-527. The store test proves which sentinel the writer returns. This test proves
// what Commerce receives through the validated router for the same real state: an
// exchange shortage is a definite 409 with its own body, and an issuance shortage stays
// a 503. A store test cannot prove the HTTP mapping (engineering rules, test placement).
func TestRefundRouteSeparatesExchangeShortageFromIssuanceShortage(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newSmokeServer(t, ctx)
	const token = "internal-token-for-refund-smoke-32b"
	router := newTestServer(st, nil, token).Router(nil, true)

	org, sourceOrder, ref, slot := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ticket := func(order uuid.UUID) store.Ticket {
		return store.Ticket{ID: uuid.New(), OrderID: order, GuestOrderRef: ref, OrganizerID: org, BuyerID: uuid.New(),
			SlotID: slot, TicketTypeID: uuid.New(), Payload: "credential", IssuedAt: time.Now().UTC()}
	}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{ticket(sourceOrder), ticket(sourceOrder)}}); err != nil {
		t.Fatal(err)
	}
	replacementOrder := uuid.New()
	if err := st.SwitchExchange(ctx, store.SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: sourceOrder, OrganizerID: org,
		Tickets: []store.Ticket{ticket(replacementOrder), ticket(replacementOrder)},
	}); err != nil {
		t.Fatal(err)
	}

	post := func(order uuid.UUID) (int, map[string]any) {
		t.Helper()
		body := `{"organizer_id":"` + org.String() + `","refund_id":"` + uuid.NewString() + `","quantity":1}`
		request := httptest.NewRequest(http.MethodPost, "/internal/orders/"+order.String()+"/refunds", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(httpx.InternalToken, token)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		var decoded map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %d %q: %v", recorder.Code, recorder.Body.String(), err)
		}
		return recorder.Code, decoded
	}

	status, body := post(sourceOrder)
	if want := map[string]any{"error": "not enough refundable tickets after exchange"}; status != http.StatusConflict || !reflect.DeepEqual(body, want) {
		t.Errorf("exchanged source order: %d %v, want 409 %v", status, body, want)
	}
	status, body = post(uuid.New())
	if want := map[string]any{"error": "tickets not issued yet"}; status != http.StatusServiceUnavailable || !reflect.DeepEqual(body, want) {
		t.Errorf("unissued order: %d %v, want 503 %v", status, body, want)
	}
}
