package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An encoded separator must not be able to walk past the /internal/ boundary.
//
// ServeMux matches escaped paths segment by segment, so "internal%2Fholds" is ONE
// segment and does not match the literal "internal" child — the request falls through
// to the broader /api/<svc>/ proxy registration. The proxy's Rewrite then works from
// r.URL.Path, which is already decoded, so the upstream service receives a perfectly
// ordinary /internal/holds/{id}/confirm and the edge refusal never happened.
//
// Found by the TKT-124 adversarial review. It is not specific to inventory or to the
// hold transitions: every /api/<svc>/internal/* route was reachable this way, which is
// why the guard sits in front of the whole mux rather than on one prefix.
func TestEncodedSeparatorCannotReachInternalRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/inventory/internal/", http.NotFoundHandler())
	mux.Handle("/api/inventory/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // stands in for the reverse proxy
	}))
	guarded := denyEncodedSeparators(mux)

	const id = "00000000-0000-0000-0000-000000000001"
	for name, tc := range map[string]struct {
		path string
		want int
		why  string
	}{
		"encoded separator, lowercase": {"/api/inventory/internal%2fholds/" + id + "/confirm", http.StatusNotFound,
			"the bypass this guard exists for"},
		"encoded separator, uppercase": {"/api/inventory/internal%2Fholds/" + id + "/confirm", http.StatusNotFound,
			"case in the escape must not matter"},
		"encoded separator, another internal route": {"/api/inventory/internal%2Fslots/" + id + "/capacity-adjustments", http.StatusNotFound,
			"the bypass was never specific to the hold transitions"},
		"plain internal route": {"/api/inventory/internal/holds/" + id + "/confirm", http.StatusNotFound,
			"the ordinary edge refusal still works"},
		"public route, no escaping": {"/api/inventory/holds", http.StatusOK,
			"the guard must not break ordinary traffic"},
		// %252F decodes to the literal text "%2F" — a character sequence inside one
		// segment, not a separator. It cannot walk the boundary, so the gateway is right
		// to pass it on; the upstream service then 404s it as an unknown path. Pinned so
		// a future "just reject anything that looks escaped" edit has to argue with a test.
		"double-encoded is text, not a separator": {"/api/inventory/internal%252Fholds/" + id + "/confirm", http.StatusOK,
			"double-encoding yields no separator, so no boundary is crossed at the edge"},
	} {
		t.Run(name, func(t *testing.T) {
			res := httptest.NewRecorder()
			guarded.ServeHTTP(res, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if res.Code != tc.want {
				t.Fatalf("%s: status=%d want=%d — %s", tc.path, res.Code, tc.want, tc.why)
			}
		})
	}
}

// The back-office (US-018) is a web shell like the scanner: registered at a
// non-/api/ prefix, NOT path-stripped (it serves under /admin/). This pins the
// route table so a careless edit can't drop the admin route or promote the
// storefront catch-all above it.
func TestBackofficeRoute(t *testing.T) {
	if routes["/admin/"] != "BACKOFFICE_URL" {
		t.Fatalf("gateway must proxy /admin/ to BACKOFFICE_URL, got %q", routes["/admin/"])
	}
	// The catch-all stays the storefront; longest-prefix (ServeMux) resolves
	// /admin/ ahead of / regardless of map order. The strip logic keys on the
	// /api/ prefix, so a non-/api/ web shell like /admin/ is passed through
	// intact — that behavior is exercised end-to-end by the smoke suite.
	if routes["/"] != "STOREFRONT_URL" {
		t.Fatalf("/ must remain the storefront catch-all, got %q", routes["/"])
	}
}
