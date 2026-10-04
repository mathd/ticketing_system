package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// recordingFactDB is the commerce-side fact table: it hands back a fixed identity and the
// STORED timestamp, which is what the journal must carry on a replay.
type recordingFactDB struct {
	id       uuid.UUID
	occurred time.Time
	err      error
	types    []string
}

func (f *recordingFactDB) RecordOrderFact(_ context.Context, _ store.StuckOrder, factType string) (uuid.UUID, time.Time, error) {
	f.types = append(f.types, factType)
	return f.id, f.occurred, f.err
}

// TKT-285. The zero-total completion submits order.created and order.completed through the
// same protocol as order.failed (ADR-011): record locally, then submit the recorded identity
// and the STORED timestamp to payments' /internal/facts. What the request carries is the
// contract, so it is asserted from the wire, not from the helper's own arguments.
func TestJournalFactSubmitsOrderFactsWithTheirStoredIdentityAndTime(t *testing.T) {
	for _, tc := range []struct {
		factType string
		submit   func(JournalFact, context.Context, store.StuckOrder) error
	}{
		{"order.created", JournalFact.OrderCreated},
		{"order.completed", JournalFact.OrderCompleted},
		{"order.failed", JournalFact.OrderFailed},
	} {
		t.Run(tc.factType, func(t *testing.T) {
			var got map[string]any
			var path, token string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, token = r.URL.Path, r.Header.Get("X-Internal-Token")
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			stored := time.Date(2026, 9, 25, 10, 11, 12, 345678000, time.UTC)
			db := &recordingFactDB{id: uuid.New(), occurred: stored}
			order := zeroStuck("created")
			j := JournalFact{Client: server.Client(), PaymentsURL: server.URL, Token: "payments-token", DB: db}

			if err := tc.submit(j, context.Background(), order); err != nil {
				t.Fatal(err)
			}
			if path != "/internal/facts" || token != "payments-token" {
				t.Errorf("request to %q with token %q, want /internal/facts with the payments token", path, token)
			}
			if len(db.types) != 1 || db.types[0] != tc.factType {
				t.Errorf("recorded fact types %v, want exactly [%s]", db.types, tc.factType)
			}
			want := map[string]any{
				"fact_id": db.id.String(), "organizer_id": order.OrganizerID.String(), "fact_type": tc.factType,
				"buyer_id": order.BuyerID.String(), "amount": float64(0), "currency": "EUR",
				"occurred_at": stored.Format(time.RFC3339Nano),
				"payload":     map[string]any{"order_id": order.OrderID.String()},
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("submitted\n%s\nwant\n%s", gotJSON, wantJSON)
			}
		})
	}
}

// A failure on either side is an error the runner retries: a local write that failed means no
// request goes out (the journal must never learn of a fact commerce did not record), and a
// non-200 from payments is not success.
func TestJournalFactReportsEitherSideFailing(t *testing.T) {
	t.Run("local record fails, nothing is sent", func(t *testing.T) {
		sent := 0
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent++ }))
		defer server.Close()
		j := JournalFact{Client: server.Client(), PaymentsURL: server.URL, Token: "t",
			DB: &recordingFactDB{err: errors.New("db down")}}
		if err := j.OrderCompleted(context.Background(), zeroStuck("created")); err == nil {
			t.Fatal("want an error")
		}
		if sent != 0 {
			t.Fatalf("sent %d request(s), want 0", sent)
		}
	})
	t.Run("payments refuses", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer server.Close()
		j := JournalFact{Client: server.Client(), PaymentsURL: server.URL, Token: "t",
			DB: &recordingFactDB{id: uuid.New(), occurred: time.Now()}}
		for name, submit := range map[string]func(context.Context, store.StuckOrder) error{
			"order.created": j.OrderCreated, "order.completed": j.OrderCompleted,
		} {
			if err := submit(context.Background(), zeroStuck("created")); err == nil {
				t.Errorf("%s: a 400 from payments was reported as success", name)
			}
		}
	})
}
