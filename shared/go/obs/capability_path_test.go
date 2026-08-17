package obs_test

import (
	"strings"
	"testing"

	"ticketing/shared/obs"
)

// The reference used throughout: a real-shaped CSPRNG UUIDv4, the thing ADR-012
// says is never logged.
const capRef = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"

// Predicate 1 — the path matches a DECLARED capability route shape.
//
// Mutation this must catch: delete the corresponding entry from the route table.
// A near-miss must come back untouched, which is what stops the sanitiser from
// quietly reshaping ordinary routes (requestlog_test.go pins /healthz exactly).
func TestSanitizedPathMatchesOnlyDeclaredShapes(t *testing.T) {
	// The ticket id is deliberately a DIFFERENT value from capRef: on the QR
	// route it is preserved, so reusing capRef here would make this test fail on
	// correct behaviour (it did, first run) and pass only if the sanitiser
	// wrongly blanked the ticket segment too.
	const ticket = "9c8b7a65-4321-4def-8abc-000000000001"
	declared := []string{
		"/api/access/orders/" + capRef + "/tickets",
		"/orders/" + capRef + "/tickets",
		"/api/access/orders/" + capRef + "/tickets/" + ticket + "/qr.png",
		"/orders/" + capRef + "/tickets/" + ticket + "/qr.png",
		"/en/tickets/" + capRef,
		"/fr/tickets/" + capRef,
	}
	for _, in := range declared {
		if got := obs.SanitizedPath(in); strings.Contains(got, capRef) {
			t.Errorf("declared shape not sanitized:\n  in  = %s\n  got = %s", in, got)
		}
	}

	// Near misses: same segment count or similar literals, but NOT a declared
	// capability route. Each must survive byte-for-byte.
	nearMiss := []string{
		"/healthz",
		"/readyz",
		"/api/access/orders/" + capRef,               // too short: no /tickets
		"/api/access/orders/" + capRef + "/refunds",  // wrong trailing literal
		"/api/commerce/orders/" + capRef + "/tickets", // wrong service prefix
		"/internal/orders/" + capRef + "/refunds",
		"/en/account",
		"/en/events/" + capRef, // right locale shape, wrong literal
	}
	for _, in := range nearMiss {
		if got := obs.SanitizedPath(in); got != in {
			t.Errorf("near miss was altered:\n  in  = %s\n  got = %s", in, got)
		}
	}
}

// Predicate 2 — the capability segment is replaced AT THE RIGHT POSITION.
//
// This is the predicate a bare "the ref is absent" assertion cannot see: a
// sanitiser that blanks every variable segment, or the wrong one, also removes
// the ref. Distinct values per segment so a swap is visible.
func TestSanitizedPathReplacesOnlyTheCapabilitySegment(t *testing.T) {
	const ticket = "9c8b7a65-4321-4def-8abc-000000000001"

	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "bundle via gateway",
			in:   "/api/access/orders/" + capRef + "/tickets",
			want: "/api/access/orders/:capability/tickets",
		},
		{
			name: "bundle at the access service (prefix stripped)",
			in:   "/orders/" + capRef + "/tickets",
			want: "/orders/:capability/tickets",
		},
		{
			// The ticket id is NOT a capability: qrlink.go gates the image on a
			// live HMAC, so it stays readable for correlation. Only the order
			// reference is redeemable on its own.
			name: "qr image keeps the ticket id",
			in:   "/api/access/orders/" + capRef + "/tickets/" + ticket + "/qr.png",
			want: "/api/access/orders/:capability/tickets/" + ticket + "/qr.png",
		},
		{
			name: "storefront page keeps the locale",
			in:   "/fr/tickets/" + capRef,
			want: "/fr/tickets/:capability",
		},
	} {
		if got := obs.SanitizedPath(tc.in); got != tc.want {
			t.Errorf("%s:\n  in   = %s\n  got  = %s\n  want = %s", tc.name, tc.in, got, tc.want)
		}
	}
}

// Predicate 3 — every non-capability segment and the trailing shape are PRESERVED.
//
// Mutation this must catch: truncating the suffix, or collapsing the whole path
// to a constant. Both hide the reference and both destroy the route shape COS #1
// and COS #3 require for debugging and correlation.
func TestSanitizedPathPreservesTheRouteShape(t *testing.T) {
	const ticket = "9c8b7a65-4321-4def-8abc-000000000001"
	got := obs.SanitizedPath("/api/access/orders/" + capRef + "/tickets/" + ticket + "/qr.png")

	for _, keep := range []string{"/api", "access", "orders", "tickets", "qr.png", ticket} {
		if !strings.Contains(got, keep) {
			t.Errorf("shape lost %q: got %s", keep, got)
		}
	}
	if n := strings.Count(got, "/"); n != strings.Count("/api/access/orders/x/tickets/y/qr.png", "/") {
		t.Errorf("segment count changed: got %s", got)
	}
}

// A single trailing slash is a DIFFERENT string, so an exact segment-count table
// matches one spelling and misses the other — logging the reference. Verified
// empirically during plan-review: Go keeps the trailing slash in r.URL.Path.
//
// Mutation this must catch: delete the normalisation step in the matcher.
func TestSanitizedPathHandlesTrailingSlash(t *testing.T) {
	for _, in := range []string{
		"/api/access/orders/" + capRef + "/tickets/",
		"/orders/" + capRef + "/tickets/",
		"/en/tickets/" + capRef + "/",
	} {
		got := obs.SanitizedPath(in)
		if strings.Contains(got, capRef) {
			t.Errorf("trailing-slash spelling leaked the reference:\n  in  = %s\n  got = %s", in, got)
		}
		if !strings.HasSuffix(got, "/") {
			t.Errorf("trailing slash should be preserved in the logged shape: in=%s got=%s", in, got)
		}
	}
}

// Non-canonical spellings of a declared route must not evade the table.
//
// Found by probing rather than by reasoning, and every one of them was reaching
// the logger: RequestLogger is mounted OUTSIDE the mux, so it writes the path
// before any router normalises or rejects it — the request does not even have to
// succeed, a 404 logs the same line. The earlier tests missed this because their
// fixture only ever built canonical paths: a fixture that cannot reach the
// failing state (AGENTS.md).
//
// These are not exotic. A double slash is what a misjoined base URL produces,
// and case variance is what a hand-typed or proxied URL produces. Confirmed
// end-to-end through the real logger before this test was written.
//
// Mutation this must catch: delete the canonicalisation in the matcher.
func TestSanitizedPathIsNotEvadedByNonCanonicalSpellings(t *testing.T) {
	for _, in := range []string{
		"/API/ACCESS/orders/" + capRef + "/tickets",   // upper-case literals
		"/Api/Access/Orders/" + capRef + "/Tickets",   // mixed case
		"//api/access/orders/" + capRef + "/tickets",  // leading double slash
		"/api/access//orders/" + capRef + "/tickets",  // interior double slash
		"/api/access/orders/" + capRef + "/tickets//", // double trailing slash
		"/api/access/./orders/" + capRef + "/tickets", // dot segment
		"/EN/tickets/" + capRef,                       // storefront, upper locale
		"//en/tickets/" + capRef,
	} {
		if got := obs.SanitizedPath(in); strings.Contains(got, capRef) {
			t.Errorf("non-canonical spelling leaked the reference:\n  in  = %s\n  got = %s", in, got)
		}
	}
}

// Sanitising is DELIBERATELY broader than routing (ai-review F8).
//
// The routers are case-sensitive — ServeMux 404s "/API/ACCESS/..." and the
// storefront's isLocale is exact — so folding case redacts the capability
// position of requests no handler will serve. That is the intended trade and it
// is pinned here so a later reader cannot mistake it for an oversight and
// "correct" it: the correction would write a live credential to the log, because
// a 404 does not make the reference less redeemable and the logger runs before
// the router.
//
// If this test is ever failing because the policy changed, the ADR-012 amendment
// must change with it.
func TestSanitizedPathRedactsRouteShaped404sOnPurpose(t *testing.T) {
	for _, in := range []string{
		"/API/ACCESS/orders/" + capRef + "/tickets", // ServeMux: 404
		"/EN/tickets/" + capRef,                     // isLocale: exact, so no page
	} {
		if got := obs.SanitizedPath(in); strings.Contains(got, capRef) {
			t.Errorf("a route-shaped request that no handler serves still leaked the reference:\n"+
				"  in  = %s\n  got = %s\nThe 404 does not make the capability less valid.", in, got)
		}
	}
}

// A ".." spelling is a WORKING request, and it must not leak (ai-review F5).
//
// net/http's ServeMux reduces dot-dot segments before matching and answers
// "/api/access/unused/../orders/{ref}/tickets" with a 307 to the canonical path
// — verified, not assumed. RequestLogger runs outside the mux, so it sees the
// un-reduced spelling on the redirect AND on the follow-up request. Dropping
// only "" and "." left the reference in both lines.
//
// Mutation this must catch: remove the ".." case from canonicalSegments.
func TestSanitizedPathReducesDotDotLikeTheRouter(t *testing.T) {
	for _, in := range []string{
		"/api/access/unused/../orders/" + capRef + "/tickets",
		"/api/access/orders/x/../" + capRef + "/tickets",
		"/en/x/../tickets/" + capRef,
		"/a/b/../../api/access/orders/" + capRef + "/tickets",
		"/../api/access/orders/" + capRef + "/tickets", // underflow at the root
	} {
		if got := obs.SanitizedPath(in); strings.Contains(got, capRef) {
			t.Errorf("dot-dot spelling leaked the reference:\n  in  = %s\n  got = %s", in, got)
		}
	}

	// Reduction must not invent a match: these do NOT reduce to a capability
	// route and must come back untouched.
	for _, in := range []string{
		"/api/access/orders/" + capRef + "/../tickets", // ref is popped away
		"/en/tickets/" + capRef + "/..",                // capability popped
		"/admin/../scanner/tickets/abc",
	} {
		if got := obs.SanitizedPath(in); got != in {
			t.Errorf("non-capability dot-dot path was rewritten:\n  in  = %s\n  got = %s", in, got)
		}
	}
}

// The storefront rule matches a LOCALE, not "any first segment" (ai-review F2).
//
// The gateway owns /admin/ and /scanner/ alongside the "/" catch-all, so a rule
// keyed on "any first segment" blanks the last segment of /admin/tickets/123 and
// /scanner/tickets/abc — destroying exactly the identifiers an operator needs to
// diagnose a misroute or a 404, in namespaces that carry no capability at all.
//
// Mutation this must catch: widen the locale position back to a bare variable
// segment.
func TestSanitizedPathStorefrontRuleIsScopedToRealLocales(t *testing.T) {
	// Gateway-owned namespaces and arbitrary first segments: never a capability.
	for _, in := range []string{
		"/admin/tickets/123",
		"/scanner/tickets/abc",
		"/foo/tickets/bar",
		"/api/tickets/xyz",
	} {
		if got := obs.SanitizedPath(in); got != in {
			t.Errorf("non-locale namespace was rewritten:\n  in  = %s\n  got = %s", in, got)
		}
	}
	// The real locales still sanitize.
	for _, in := range []string{"/en/tickets/" + capRef, "/fr/tickets/" + capRef} {
		if got := obs.SanitizedPath(in); strings.Contains(got, capRef) {
			t.Errorf("locale route not sanitized:\n  in  = %s\n  got = %s", in, got)
		}
	}
}

// Canonicalisation must not become a licence to reshape ordinary routes: the
// logged path still has to describe what was actually requested.
func TestSanitizedPathLeavesNonCanonicalOrdinaryRoutesAlone(t *testing.T) {
	for _, in := range []string{
		"/healthz",
		"//healthz",
		"/HEALTHZ",
		"/api/catalog//events",
		"/en/account",
		"/admin/tickets/123",
		"/scanner/tickets/abc",
	} {
		if got := obs.SanitizedPath(in); got != in {
			t.Errorf("ordinary route was altered:\n  in  = %s\n  got = %s", in, got)
		}
	}
}

// Percent-encoding cannot evade the table: Go decodes r.URL.Path before any
// handler sees it, so "%32f1e..." arrives as "2f1e...". Documented as a test so
// the next reader does not have to re-derive it — denyEncodedSeparators running
// INSIDE RequestLogger makes it look like escaped spellings reach the logger raw.
func TestSanitizedPathIsNotEvadedByPercentEncoding(t *testing.T) {
	// This is the DECODED form the logger actually receives for a request
	// spelled "/api/access/orders/%32f1e3d4c-.../tickets".
	if got := obs.SanitizedPath("/api/access/orders/" + capRef + "/tickets"); strings.Contains(got, capRef) {
		t.Errorf("decoded spelling leaked: %s", got)
	}
}

// An empty or root path must not panic or be reshaped.
func TestSanitizedPathEdgeCases(t *testing.T) {
	for _, in := range []string{"", "/", "//", "/api/access/orders//tickets"} {
		if got := obs.SanitizedPath(in); got != in {
			t.Errorf("edge case altered: in=%q got=%q", in, got)
		}
	}
}
