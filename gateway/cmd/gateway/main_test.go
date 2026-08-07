package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	// A REAL upstream behind the REAL proxy: what the service receives is the only
	// thing that settles whether an edge refusal held. A stub that ignores the request
	// path would report "200, proxied" without ever showing what got proxied — which
	// is how a rewrite that decodes a separator would slip past this test.
	var gotPath, gotRawPath, gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawPath, gotRequestURI = r.URL.Path, r.URL.RawPath, r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/inventory/internal/", http.HandlerFunc(edgeDenied))
	mux.Handle("/api/inventory/", apiProxy(upstreamURL, "/api/inventory/", true))
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
		// to pass it on. The assertion below on what the upstream RECEIVED is what makes
		// this case worth having: it fails if a future rewrite ever turns it into a "/".
		"double-encoded is text, not a separator": {"/api/inventory/internal%252Fholds/" + id + "/confirm", http.StatusOK,
			"double-encoding yields no separator, so no boundary is crossed at the edge"},
	} {
		t.Run(name, func(t *testing.T) {
			gotPath, gotRawPath, gotRequestURI = "", "", ""
			res := httptest.NewRecorder()
			guarded.ServeHTTP(res, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if res.Code != tc.want {
				t.Fatalf("%s: status=%d want=%d — %s", tc.path, res.Code, tc.want, tc.why)
			}
			// Whatever reached the upstream must not contain an internal route with a
			// real separator. This is the assertion that survives a rewrite change: a
			// status code says the edge answered, not what the service was asked to do.
			if strings.HasPrefix(gotPath, "/internal/") {
				t.Fatalf("%s: upstream received internal path %q (rawpath=%q requesturi=%q) — the boundary leaked",
					tc.path, gotPath, gotRawPath, gotRequestURI)
			}
		})
	}
}

// The gateway's refusal must be distinguishable from every other layer's 404, or a test
// asserting "the edge refused" is asserting nothing (ai-review pass 2, F1).
func TestEdgeDenialIsDistinguishableFromAGenericNotFound(t *testing.T) {
	res := httptest.NewRecorder()
	edgeDenied(res, httptest.NewRequest(http.MethodGet, "/api/inventory/internal/whatever", nil))

	if res.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=404", res.Code)
	}
	if got := strings.TrimSpace(res.Body.String()); got != edgeDeniedBody {
		t.Fatalf("body=%q want=%q", got, edgeDeniedBody)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q want=application/json", got)
	}
	// The specific thing that must never be true again: matching what net/http, chi and
	// the services all emit.
	generic := httptest.NewRecorder()
	http.NotFound(generic, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.TrimSpace(generic.Body.String()) == strings.TrimSpace(res.Body.String()) {
		t.Fatal("the edge refusal is byte-identical to http.NotFound — provenance assertions built on it prove nothing")
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

// A caller cannot forge the client IP that commerce rate-limits on (TKT-224).
//
// commerce keys its per-source budget on X-Forwarded-For (services/commerce/internal/api/ratelimit.go),
// which is only safe because of what this proxy does with the header: the Rewrite
// hook receives an outbound request with the inbound X-Forwarded-* already
// STRIPPED, and SetXForwarded then writes the connecting peer. A forged chain is
// discarded, not appended to — so there is no attacker-controlled prefix for the
// limiter to mistake for the client.
//
// This is asserted against the REAL proxy rather than reasoned about from the
// httputil docs, because the difference between "replaces" and "appends" is the
// difference between a limiter and a bypass, and Director (the older hook) does
// append. If a future change swaps Rewrite for Director, this test is what says
// so — and commerce's "take the last element" would then become load-bearing
// rather than belt-and-braces.
func TestAForgedXForwardedForDoesNotReachTheUpstream(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header["X-Forwarded-For"]
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/commerce/", apiProxy(upstreamURL, "/api/commerce/", true))

	const peer = "203.0.113.9"
	for name, forged := range map[string]string{
		"no header at all":     "",
		"one forged hop":       "1.2.3.4",
		"a whole forged chain": "1.2.3.4, 5.6.7.8",
		"a forged private hop": "10.0.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			got = nil
			req := httptest.NewRequest(http.MethodPost, "/api/commerce/customers", nil)
			req.RemoteAddr = peer + ":44444"
			if forged != "" {
				req.Header.Set("X-Forwarded-For", forged)
			}
			mux.ServeHTTP(httptest.NewRecorder(), req)

			if len(got) != 1 || got[0] != peer {
				t.Fatalf("upstream saw X-Forwarded-For %#v, want exactly [%q] — a forged hop survived, "+
					"so commerce's per-source rate limit is forgeable", got, peer)
			}
		})
	}
}
