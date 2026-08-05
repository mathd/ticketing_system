package api

// TKT-110: the availability read emitted ADR-004's seconds tier from the handler
// alone — real behaviour with no declaration, the inverse of the defect TKT-128
// recorded (and the gap that ADR's amendment routed here). Declaring the header
// makes the tier a reviewed contract artifact instead of an incidental header.
//
// Two cases, because they pin different things and neither implies the other:
// case 1 feeds the handler's own constant and expects it to satisfy the
// declaration (the drift pin — it fails, stack-free, if either side moves alone),
// case 2 feeds a different tier and expects ADR-028 fail-closed (the enforcement
// pin — it is what stops a bare `type: string` declaration from satisfying case 1
// vacuously). The positive path through the real stack is already asserted by
// smoke/inventory_contention_test.go.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/inventory/api"
	"ticketing/shared/contract"
)

func TestAvailabilityCacheTierIsContractEnforced(t *testing.T) {
	for _, tc := range []struct {
		name, emitted string
		want          int
	}{
		{"handler constant satisfies the contract", CacheControlPublicAvailability, http.StatusOK},
		{"any other tier fails closed", "public, max-age=60, s-maxage=60", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Get("/slots/{id}/availability", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", tc.emitted)
				// TKT-205 added a required Age header to this response. The stub has to
				// emit it or every case here 500s on the Age check before reaching the
				// Cache-Control one — which would make this test pass for the wrong
				// reason in the negative case and fail outright in the positive one.
				w.Header().Set("Age", "0")
				w.WriteHeader(http.StatusOK)
				// Minimal schema-valid Availability, so only the header is under test.
				_, _ = w.Write([]byte(`{"slot_id":"` + uuid.Nil.String() + `","capacity":0,` +
					`"held":0,"confirmed":0,"available":0,"offering_status":"open"}`))
			})
			// Same middleware order as (*Server).Router: response validation nested
			// inside request validation.
			h, err := contract.RequestValidator(apispec.Spec, r, nil, true)
			if err != nil {
				t.Fatalf("RequestValidator: %v", err)
			}
			id, org := uuid.NewString(), uuid.NewString()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"http://inventory.local/slots/"+id+"/availability?organizer_id="+org, nil))
			if rec.Code != tc.want {
				t.Fatalf("emitted Cache-Control %q: got %d %s, want %d",
					tc.emitted, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

// TestSeatOccupancyCacheTierIsContractEnforced is the same pin for TKT-172's seat
// occupancy read. It gets its own constant and its own header declaration rather
// than sharing availability's: ADR-028 fails closed per declaration, so one shared
// component would let one operation's drift hide behind the other still emitting
// the tier. The third case is what makes that separation real — it proves the
// occupancy declaration is enforced on its own path, not merely inherited.
func TestSeatOccupancyCacheTierIsContractEnforced(t *testing.T) {
	for _, tc := range []struct {
		name, emitted string
		omit          bool
		want          int
	}{
		{name: "handler constant satisfies the contract", emitted: CacheControlPublicSeatOccupancy, want: http.StatusOK},
		{name: "any other tier fails closed", emitted: "public, max-age=3600, s-maxage=3600", want: http.StatusInternalServerError},
		{name: "a missing header fails closed", omit: true, want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Get("/slots/{id}/seat-occupancy", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !tc.omit {
					w.Header().Set("Cache-Control", tc.emitted)
				}
				w.WriteHeader(http.StatusOK)
				// Minimal schema-valid SeatOccupancy, so only the header is under test.
				_, _ = w.Write([]byte(`{"slot_id":"` + uuid.Nil.String() + `","seat_map_id":"` +
					uuid.Nil.String() + `","offering_status":"open","remaining_capacity":0,` +
					`"unavailable_seat_identities":[]}`))
			})
			h, err := contract.RequestValidator(apispec.Spec, r, nil, true)
			if err != nil {
				t.Fatalf("RequestValidator: %v", err)
			}
			id, org := uuid.NewString(), uuid.NewString()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"http://inventory.local/slots/"+id+"/seat-occupancy?organizer_id="+org, nil))
			if rec.Code != tc.want {
				t.Fatalf("emitted Cache-Control %q (omitted=%v): got %d %s, want %d",
					tc.emitted, tc.omit, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
