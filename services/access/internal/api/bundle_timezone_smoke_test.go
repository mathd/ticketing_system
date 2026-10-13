//go:build smoke

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
)

// recordingZones is a fake Catalog zone reader. It answers from `zones` and records the
// ids it was asked for, so a test can see that the handler asks once per bundle.
type recordingZones struct {
	mu    sync.Mutex
	zones map[uuid.UUID]string
	asked [][]uuid.UUID
}

func (z *recordingZones) PerformanceZones(_ context.Context, ids []uuid.UUID) map[uuid.UUID]string {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.asked = append(z.asked, slices.Clone(ids))
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if zone, ok := z.zones[id]; ok {
			out[id] = zone
		}
	}
	return out
}

// bundleZones serves the bundle through the validated router and returns each ticket's
// performance_timezone, keyed by ticket id. A missing key fails the test: the contract
// makes the field required, and the validator would already answer 500 without it.
func bundleZones(t *testing.T, srv *Server, ref uuid.UUID) map[uuid.UUID]*string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Router(nil, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+ref.String()+"/tickets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle: %d %s", rec.Code, rec.Body.String())
	}
	var bundle struct {
		Tickets []map[string]json.RawMessage `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]*string{}
	for _, tk := range bundle.Tickets {
		var id uuid.UUID
		if err := json.Unmarshal(tk["ticket_id"], &id); err != nil {
			t.Fatal(err)
		}
		raw, ok := tk["performance_timezone"]
		if !ok {
			t.Fatalf("ticket %s has no performance_timezone: %s", id, rec.Body.String())
		}
		var zone *string
		if err := json.Unmarshal(raw, &zone); err != nil {
			t.Fatal(err)
		}
		out[id] = zone
	}
	return out
}

// TKT-365 (ADR-083). Each ticket carries the zone of ITS performance. One reference holds
// tickets for three slots: an exchange source, its replacement in another zone, and a
// slot Catalog does not know. A handler that applied one zone to every ticket, swapped
// them, or invented a zone for the unknown slot fails here.
func TestTicketBundleCarriesEachTicketsPerformanceZone(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := newSmokeServer(t, ctx)
	org, ref, sourceOrder, replacementOrder := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	kolkata, paris, unknown := uuid.New(), uuid.New(), uuid.New()
	ticket := func(order, slot uuid.UUID) store.Ticket {
		return store.Ticket{ID: uuid.New(), OrderID: order, GuestOrderRef: ref, OrganizerID: org, BuyerID: uuid.New(),
			SlotID: slot, TicketTypeID: uuid.New(), Payload: "credential", IssuedAt: time.Now().UTC()}
	}
	sources := []store.Ticket{ticket(sourceOrder, kolkata), ticket(sourceOrder, kolkata), ticket(sourceOrder, unknown)}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: sources}); err != nil {
		t.Fatal(err)
	}
	replacement := ticket(replacementOrder, paris)
	if err := st.SwitchExchange(ctx, store.SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: sourceOrder, OrganizerID: org,
		Tickets: []store.Ticket{replacement},
	}); err != nil {
		t.Fatal(err)
	}
	zones := &recordingZones{zones: map[uuid.UUID]string{kolkata: "Asia/Kolkata", paris: "Europe/Paris"}}
	srv.zones = zones

	got := bundleZones(t, srv, ref)

	want := map[uuid.UUID]string{sources[0].ID: "Asia/Kolkata", sources[1].ID: "Asia/Kolkata", replacement.ID: "Europe/Paris"}
	if len(got) != 4 {
		t.Fatalf("bundle has %d tickets, want 4", len(got))
	}
	for id, zone := range got {
		w, known := want[id]
		switch {
		case !known && zone != nil:
			t.Errorf("ticket %s for an unknown slot has zone %q, want null", id, *zone)
		case known && (zone == nil || *zone != w):
			t.Errorf("ticket %s zone = %v, want %q", id, zone, w)
		}
	}
	// One question per bundle, not one per ticket.
	if len(zones.asked) != 1 {
		t.Fatalf("asked Catalog %d times, want 1", len(zones.asked))
	}
}

// With no zone reader, and with a reader that knows nothing, the bundle still loads and
// every zone is null. A time zone is presentation; it must never cost the buyer the QR.
func TestTicketBundleLoadsWithNullZonesWhenCatalogCannotAnswer(t *testing.T) {
	ctx := context.Background()
	srv, st, _ := newSmokeServer(t, ctx)
	ref := uuid.New()
	tk := store.Ticket{ID: uuid.New(), OrderID: uuid.New(), GuestOrderRef: ref, OrganizerID: uuid.New(), BuyerID: uuid.New(),
		SlotID: uuid.New(), TicketTypeID: uuid.New(), Payload: "credential", IssuedAt: time.Now().UTC()}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{tk}}); err != nil {
		t.Fatal(err)
	}

	for name, zones := range map[string]performanceZones{"unwired": nil, "empty answer": &recordingZones{}} {
		t.Run(name, func(t *testing.T) {
			srv.zones = zones
			got := bundleZones(t, srv, ref)
			if zone, ok := got[tk.ID]; !ok || zone != nil {
				t.Fatalf("zone = %v (present %v), want null", zone, ok)
			}
		})
	}
}
