//go:build smoke

package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TKT-341 / ADR-082, the PERSISTED-REPLAY call site. A reserve whose reservation row
// already exists replays inventory with the pinned terms instead of pricing again.
//
// Real inventory cannot answer `sold_out` to a replay today: it replays an existing
// claim before any capacity arithmetic and never deletes a claim. That depends on
// there being no claim purge, so commerce keeps the case, and only this tier, with a
// fake inventory, can reach it. The gateway smoke test cannot.
func TestAPersistedReplayForwardsAPublicGASellout(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	for name, tc := range map[string]struct {
		inventory, want string
	}{
		"sellout":            {inventorySoldOut, commerceSoldOut},
		"code-less refusal":  {`{"error":"conflicting terminal state"}`, commerceCodeless},
		"another coded body": {`{"error":"slot closed","code":"slot_closed"}`, commerceCodeless},
	} {
		t.Run(name, func(t *testing.T) {
			org, ticketType := uuid.New(), uuid.New()
			key := "sold-out-replay-" + uuid.NewString()
			id := reservationID(org, key, nil)
			if _, err := db.ExecContext(ctx, `
				INSERT INTO reservations(id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
				                         unit_amount, total_amount, face_value_amount, currency, status)
				VALUES($1, $2, $3, $4, $5, $6, 2, 2500, 5000, 5000, 'EUR', 'held')`,
				id, org, uuid.New(), uuid.New(), ticketType, uuid.New()); err != nil {
				t.Fatalf("seed reservation: %v", err)
			}
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, id) })

			var replayed int
			inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/holds" {
					replayed++
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.inventory))
			}))
			defer inventory.Close()
			// A replay never prices again; a catalog call would be a different path.
			catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("a persisted replay called catalog: %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer catalog.Close()

			s := newTestServer(db, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
			body := `{"organizer_id":"` + org.String() + `","ticket_type_id":"` + ticketType.String() + `","quantity":2}`
			req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)

			if replayed != 1 {
				t.Fatalf("inventory replay calls = %d, want 1: the fixture must reach the replay path", replayed)
			}
			if got := strings.TrimSpace(res.Body.String()); res.Code != http.StatusConflict || got != tc.want {
				t.Fatalf("replay answered %d %s, want 409 %s", res.Code, got, tc.want)
			}
		})
	}
}
