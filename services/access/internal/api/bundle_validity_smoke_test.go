//go:build smoke

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

// TKT-355. The bundle carries each ticket's verdict, and the verdict belongs to THAT
// ticket. One reference holds all three forms: two exchanged source tickets, and two
// replacement tickets of which a refund then voids one. The response goes through the
// validated router, so a missing or malformed verdict is already a 500 from the contract;
// what this test adds is the per-ticket mapping, keyed on ticket ids, which the contract
// cannot see.
func TestTicketBundleCarriesEachTicketsVerdict(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := newSmokeServer(t, ctx)
	org, ref, sourceOrder, replacementOrder := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ticket := func(order uuid.UUID) store.Ticket {
		return store.Ticket{ID: uuid.New(), OrderID: order, GuestOrderRef: ref, OrganizerID: org, BuyerID: uuid.New(),
			SlotID: uuid.New(), TicketTypeID: uuid.New(), Payload: "credential", IssuedAt: time.Now().UTC()}
	}
	sources := []store.Ticket{ticket(sourceOrder), ticket(sourceOrder)}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: sources}); err != nil {
		t.Fatal(err)
	}
	replacements := []store.Ticket{ticket(replacementOrder), ticket(replacementOrder)}
	if err := st.SwitchExchange(ctx, store.SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: sourceOrder, OrganizerID: org, Tickets: replacements,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := st.RefundOrderTickets(ctx, org, replacementOrder, uuid.New(), 1)
	if err != nil {
		t.Fatal(err)
	}
	refunded := batch.TicketIDs[0]
	valid := replacements[0].ID
	if valid == refunded {
		valid = replacements[1].ID
	}

	rec := httptest.NewRecorder()
	srv.Router(nil, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+ref.String()+"/tickets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle: %d %s", rec.Code, rec.Body.String())
	}
	var bundle struct {
		Tickets []struct {
			TicketID uuid.UUID      `json:"ticket_id"`
			Validity map[string]any `json:"validity"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]map[string]any{
		sources[0].ID: {"status": "voided", "reason": "exchanged"},
		sources[1].ID: {"status": "voided", "reason": "exchanged"},
		refunded:      {"status": "voided", "reason": "refunded"},
		valid:         {"status": "valid"},
	}
	if len(bundle.Tickets) != len(want) {
		t.Fatalf("bundle has %d tickets, want %d: %s", len(bundle.Tickets), len(want), rec.Body.String())
	}
	for _, tk := range bundle.Tickets {
		w, ok := want[tk.TicketID]
		if !ok {
			t.Fatalf("unexpected ticket %s in the bundle", tk.TicketID)
		}
		if !reflect.DeepEqual(tk.Validity, w) {
			t.Errorf("ticket %s validity = %v, want %v", tk.TicketID, tk.Validity, w)
		}
	}
}
