package api

// TKT-209: catalog's five public reads commit their ADR-004 tier as a
// single-valued, required response-header component, so a drifted header is a
// 500 with the payload withheld (ADR-028) rather than a wrong header served to a
// shared cache.
//
// Before this ticket the tier was declared through a free-form `CacheControl`
// component (`type: string`, no enum, not required), which committed nothing:
// the response validator had nothing to check, so any value — and no value at
// all — passed. TKT-204 recorded that as a bounded allowlist in
// shared/go/cachetier/spec_audit_test.go rather than closing it, because closing
// it is this behaviour change.
//
// THREE cases per read, because the declaration carries TWO independent
// predicates and neither implies the other:
//
//   - the handler's own constant satisfies the declaration (the drift pin — it
//     fails if either side moves alone);
//   - another REGISTERED tier fails closed (the enum pin — this is what stops a
//     bare `type: string` from satisfying case 1 vacuously);
//   - the header omitted entirely fails closed (the `required: true` pin —
//     delete `required` and keep the enum and case 2 still passes, so without
//     this case the suite stays green over half the mechanism).
//
// The negative cases assert the payload is WITHHELD, not merely that the status
// is 500: ADR-028's guarantee is that the drifted response never reaches the
// client, and a 500 that still carried the body would satisfy a status-only
// assertion.
//
// `Age: 0` on the four minutes reads is load-bearing, not decoration. Those four
// also declare a REQUIRED `PublicReadAge` header, which fails closed on its own.
// A stub that omitted it would 500 in every case — including the negative ones,
// which would then pass for a reason that has nothing to do with Cache-Control,
// and case 1 would fail outright. (Same trap the inventory template records for
// TKT-205's Age header.) `listPublicVenues` declares no Age and is given none.
//
// The seam is `contract.ResponseValidator` over the real embedded spec, matching
// catalog's production NewRouter, which wraps response validation OUTSIDE the
// generated handler (ADR-028 § TKT-110). That differs from inventory's template,
// which nests it inside RequestValidator — copying inventory's call here would
// test a middleware arrangement catalog does not run.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/catalog/api"
	"ticketing/shared/contract"
)

func TestPublicReadCacheTiersAreContractEnforced(t *testing.T) {
	// The other registered tier, used as each read's negative. Deliberately a tier
	// the cachetier registry KNOWS: an unregistered value ("banana") would also be
	// refused by the spec audit, so a test using one would be caught a tier below
	// and prove nothing about this declaration.
	const (
		minutes = "public, max-age=300, s-maxage=300"
		hours   = "public, max-age=3600, s-maxage=3600"
	)

	for _, read := range []struct {
		name     string
		route    string
		request  string
		body     string
		constant string
		other    string
		// marker is a substring present in body and absent from the ADR-028 error
		// body, used to prove the drifted payload was withheld.
		marker string
		// withAge is true for the four reads that also declare a required Age.
		withAge bool
	}{
		{
			name:     "listPublicEvents",
			route:    "/public/events",
			request:  "/public/events?locale=en&organizer_id=" + uuid.NewString(),
			body:     `{"events":[]}`,
			constant: CacheControlPublicReads,
			marker:   `"events"`,
			other:    hours,
			withAge:  true,
		},
		{
			name:     "getPublicEvent",
			route:    "/public/events/{eventId}",
			request:  "/public/events/" + uuid.NewString() + "?locale=en",
			body:     publicEventDetailJSON,
			constant: CacheControlPublicReads,
			marker:   `"performances"`,
			other:    hours,
			withAge:  true,
		},
		{
			name:     "getPublicSeason",
			route:    "/public/seasons/{seasonId}",
			request:  "/public/seasons/" + uuid.NewString() + "?locale=en",
			body:     `{"id":"` + uuid.Nil.String() + `","organizer_id":"` + uuid.Nil.String() + `","name":"s","events":[]}`,
			constant: CacheControlPublicReads,
			marker:   `"events"`,
			other:    hours,
			withAge:  true,
		},
		{
			name:     "getPublicFestival",
			route:    "/public/festivals/{festivalId}",
			request:  "/public/festivals/" + uuid.NewString() + "?locale=en",
			body:     `{"id":"` + uuid.Nil.String() + `","organizer_id":"` + uuid.Nil.String() + `","name":"f","days":[]}`,
			constant: CacheControlPublicReads,
			marker:   `"days"`,
			other:    hours,
			withAge:  true,
		},
		{
			name:     "listPublicVenues",
			route:    "/public/venues",
			request:  "/public/venues?organizer_id=" + uuid.NewString(),
			body:     `{"venues":[]}`,
			constant: CacheControlPublicVenueReads,
			marker:   `"venues"`,
			other:    minutes,
			withAge:  false,
		},
		// TKT-141: the three seat-map reads, which TKT-209 left out of this table
		// because their tier is status-driven rather than constant. The tier the
		// HANDLER picks per payload is proven at the handler tier by
		// TestSeatMapReadCacheTierByStatus; what these rows prove is the other
		// half — that the CONTRACT admits the tier the handler picks, and refuses
		// the other one. Collapse SeatMapListCacheControl and SeatMapCacheControl
		// back into one component and these go red; the handler-tier test does
		// not, because the header it asserts on never reaches a validator that
		// disagrees with it.
		//
		// `constant` is the tier each read emits for an ALL-PUBLISHED payload —
		// the only branch that differs between the two components. The no-store
		// branch is common to both and is not what this table is discriminating.
		{
			name:     "listVenueSeatMaps",
			route:    "/public/venues/{venueId}/seat-maps",
			request:  "/public/venues/" + uuid.NewString() + "/seat-maps",
			body:     `{"seat_maps":[]}`,
			constant: CacheControlPublicListReads,
			marker:   `"seat_maps"`,
			other:    hours,
			withAge:  false,
		},
		{
			name:     "listSeatMapVersions",
			route:    "/public/seat-maps/{seatMapId}/versions",
			request:  "/public/seat-maps/" + uuid.NewString() + "/versions",
			body:     `{"versions":[]}`,
			constant: CacheControlPublicListReads,
			marker:   `"versions"`,
			other:    hours,
			withAge:  false,
		},
		// The by-id read is in this table for the mutation the other two cannot
		// see: its negative is MINUTES, so merging the two components back into
		// one (which would admit minutes here) turns this row red. Without it the
		// geometry half of the split is unproven at the contract tier.
		{
			name:     "getPublicSeatMapGeometry",
			route:    "/public/seat-maps/{seatMapId}",
			request:  "/public/seat-maps/" + uuid.NewString(),
			body:     seatMapGeometryJSON,
			constant: CacheControlPublicVenueReads,
			marker:   `"sections"`,
			other:    minutes,
			withAge:  false,
		},
	} {
		for _, tc := range []struct {
			name    string
			emitted string
			omit    bool
			want    int
		}{
			{name: "handler constant satisfies the contract", emitted: read.constant, want: http.StatusOK},
			{name: "another registered tier fails closed", emitted: read.other, want: http.StatusInternalServerError},
			{name: "a missing header fails closed", omit: true, want: http.StatusInternalServerError},
		} {
			t.Run(read.name+"/"+tc.name, func(t *testing.T) {
				r := chi.NewRouter()
				r.Get(read.route, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if !tc.omit {
						w.Header().Set("Cache-Control", tc.emitted)
					}
					if read.withAge {
						w.Header().Set("Age", "0")
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(read.body))
				})
				// Response validation wrapped OUTSIDE the handler, as NewRouter does.
				h, err := contract.ResponseValidator(apispec.Spec, r, nil, true)
				if err != nil {
					t.Fatalf("ResponseValidator: %v", err)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://catalog.local"+read.request, nil))

				if rec.Code != tc.want {
					t.Fatalf("emitted Cache-Control %q (omitted=%v): got %d %s, want %d",
						tc.emitted, tc.omit, rec.Code, rec.Body.String(), tc.want)
				}
				if tc.want != http.StatusInternalServerError {
					return
				}
				// ADR-028 withholds the drifted payload. A status-only assertion would
				// pass on a 500 that still carried the body.
				// The marker is a substring unique to the stub's payload and absent
				// from ADR-028's generic error body, so this cannot pass by accident.
				if strings.Contains(rec.Body.String(), read.marker) {
					t.Fatalf("emitted Cache-Control %q (omitted=%v): the drifted payload reached the client: %s",
						tc.emitted, tc.omit, rec.Body.String())
				}
			})
		}
	}
}

func TestCatalogCacheTierDuplicateValuesAreRefused(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/public/seat-maps/{seatMapId}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Handler-Only", "secret")
		w.Header().Add("Cache-Control", "no-store")
		w.Header().Add("Cache-Control", CacheControlPublicVenueReads)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(seatMapGeometryJSON))
	})
	var logs bytes.Buffer
	h, err := contract.ResponseValidator(apispec.Spec, r, slog.New(slog.NewJSONHandler(&logs, nil)), true)
	if err != nil {
		t.Fatalf("ResponseValidator: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://catalog.local/public/seat-maps/"+uuid.NewString(), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("two individually permitted enum values: got %d %s, want 500", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "{\"error\":\"response violates OpenAPI contract\"}\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
		t.Fatalf("Cache-Control = %v", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Handler-Only") != "" {
		t.Fatalf("refusal headers = %v", rec.Header())
	}
	if strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("want exactly one structured error log, got %q", logs.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("decode drift log: %v", err)
	}
	if entry["level"] != "ERROR" || entry["msg"] != "response violates OpenAPI contract" || entry["status"] != float64(http.StatusOK) {
		t.Fatalf("drift log = %v", entry)
	}
	if detail, _ := entry["error"].(string); !strings.Contains(detail, "Cache-Control") || !strings.Contains(detail, "has 2 field values") {
		t.Fatalf("drift log does not identify header cardinality: %v", entry)
	}
}

// publicEventDetailJSON is a minimal schema-valid PublicEventDetail, so only the
// header is under test.
const publicEventDetailJSON = `{"id":"00000000-0000-0000-0000-000000000000",` +
	`"organizer_id":"00000000-0000-0000-0000-000000000000","name":"e",` +
	`"series":[],"performances":[]}`

// seatMapGeometryJSON is a minimal schema-valid SeatMapGeometry, so only the
// header is under test. `sections` is present and empty — it is this fixture's
// marker, and SeatMapGeometry requires it.
const seatMapGeometryJSON = `{"map":{"id":"00000000-0000-0000-0000-000000000000",` +
	`"organizer_id":"00000000-0000-0000-0000-000000000000",` +
	`"venue_id":"00000000-0000-0000-0000-000000000000","name":"m","version":1,` +
	`"status":"published","orphan_prevention_enabled":false,` +
	`"created_at":"2026-01-01T00:00:00Z"},"sections":[]}`
