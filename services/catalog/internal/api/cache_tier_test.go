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
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestPublicReadCacheTierDuplicateHeaderIsNotCaught pins a KNOWN, OPEN gap so the
// claim above cannot quietly drift from the behaviour (ADR-021's rule: a gap that
// is not this ticket's to close is pinned as a test asserting it is PRESENT).
//
// `required: true` plus a one-value enum constrains the FIRST field value only.
// kin-openapi decodes a primitive header from raw[0] and never looks at the rest,
// while the response wrapper forwards every value — so a handler that emits the
// declared tier and then APPENDS a second value passes validation, and both values
// reach the client. A shared cache reading that response gets a conflicting
// directive the contract says cannot happen.
//
// Scope, established by running it: this is NOT introduced by TKT-209 and is not
// specific to these five reads. It is a property of the shared response validator
// (shared/go/contract) and applies to every enum-declared response header in every
// service — `PriceResolutionCacheControl` and `NeverCacheControl`, catalog's
// `SeatMapCacheControl`, and inventory's, all of which predate this branch and
// behave identically. Closing it means rejecting a multi-valued declared header in
// the shared validator, which changes behaviour for all five services and belongs
// in its own ticket rather than smuggled into a catalog contract change.
//
// This test asserts the gap is STILL THERE. If it ever fails, the validator has
// been fixed — update the ADR-004 amendment and delete this test; do not "repair"
// it. Found by TKT-209's adversarial ai-review and verified by execution.
func TestPublicReadCacheTierDuplicateHeaderIsNotCaught(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/public/venues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The declared tier FIRST, so raw[0] satisfies the enum...
		w.Header().Add("Cache-Control", CacheControlPublicVenueReads)
		// ...and a value the enum does not permit second.
		w.Header().Add("Cache-Control", "public, max-age=300, s-maxage=300")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"venues":[]}`))
	})
	h, err := contract.ResponseValidator(apispec.Spec, r, nil, true)
	if err != nil {
		t.Fatalf("ResponseValidator: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://catalog.local/public/venues?organizer_id="+uuid.NewString(), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("the duplicate-header gap appears to be CLOSED (got %d, want 200). "+
			"If the shared response validator now rejects a multi-valued declared header, "+
			"that is good news: update ADR-004's TKT-209 amendment and delete this test.", rec.Code)
	}
	// The second, undeclared value really does reach the client — the part that
	// makes this a gap rather than a curiosity.
	//
	// Asserted as the EXACT ordered pair, not as a count: a length check stays
	// green if something downstream drops the forbidden value and duplicates the
	// allowed one, which is the shape in which this gap would actually get closed.
	// The test would then still claim the gap is open while it was shut.
	got := rec.Result().Header.Values("Cache-Control")
	want := []string{CacheControlPublicVenueReads, "public, max-age=300, s-maxage=300"}
	if !slices.Equal(got, want) {
		t.Fatalf("the forbidden second value must still reach the client verbatim.\n got: %v\nwant: %v\n"+
			"If the forbidden value is gone, the shared validator has been fixed: update ADR-004's "+
			"TKT-209 amendment and delete this test rather than loosening this assertion.", got, want)
	}
}

// publicEventDetailJSON is a minimal schema-valid PublicEventDetail, so only the
// header is under test.
const publicEventDetailJSON = `{"id":"00000000-0000-0000-0000-000000000000",` +
	`"organizer_id":"00000000-0000-0000-0000-000000000000","name":"e",` +
	`"series":[],"performances":[]}`
