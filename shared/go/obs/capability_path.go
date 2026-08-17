package obs

import "strings"

// Capability segments, and why this file exists (TKT-202, ADR-012).
//
// # The rule
//
// A `guest_order_ref` is a CSPRNG UUIDv4 that ADR-012 calls "a no-store guest
// retrieval capability", and it is redeemable by whoever holds it with no
// authentication at all. Since TKT-223 shipped guest-order claiming, holding one
// is not "can read someone's tickets" but "can TAKE the order" — ADR-049 § TKT-223
// records a claim as destructive, exclusive, permanent and unrecoverable.
//
// Anything that writes such a segment to a log or a span hands that capability to
// everyone who can read them, for as long as they are retained. ADR-012 says, in
// as many words, that it is "not logged". This file is what makes that true.
//
// # Why a declared table and not a regex on one route
//
// COS #2: the rule is "which segments are capabilities", not "this path is
// special". A regex bolted onto the bundle route would have to be re-discovered
// for the next capability URL. A table of route SHAPES, consulted by every
// emitter, is inherited by construction — adding a capability URL means adding a
// row here, and both the log sink and the span sink pick it up.
//
// # Why segment counts are a sound way to match
//
// Nothing this platform fronts takes a path parameter containing a slash — they
// are UUIDs and fixed words. The gateway already depends on that and enforces it:
// denyEncodedSeparators (gateway/cmd/gateway/main.go) refuses any request whose
// path carries an encoded "/" rather than normalising it. So a declared shape has
// a fixed segment count, and matching on that cannot be widened by a crafted path.
//
// # Percent-encoding is not a hole
//
// Go decodes r.URL.Path before any handler runs, so a request spelled
// "/api/access/orders/%32f1e.../tickets" arrives here as
// "/api/access/orders/2f1e.../tickets" and matches normally. Worth stating
// because denyEncodedSeparators sits INSIDE RequestLogger, which makes it look
// like escaped spellings reach the logger unprocessed. They reach it — decoded.
//
// # Both spellings of every route
//
// The gateway strips "/api/<svc>/" before proxying (stripAPIPrefix), so one
// request is logged as "/api/access/orders/{ref}/tickets" at the gateway and as
// "/orders/{ref}/tickets" inside access. Both are declared. A table carrying only
// one form silently misses half the lines — and the half it misses belongs to the
// service that owns the data.

// segment describes one position in a declared route shape.
type segment struct {
	literal string // matched exactly when kind == segLiteral
	kind    segmentKind
}

type segmentKind uint8

const (
	// segLiteral must match exactly — this is what distinguishes
	// /orders/{ref}/tickets from /orders/{id}/refunds.
	segLiteral segmentKind = iota
	// segAny is a variable segment that is NOT a capability and is preserved
	// verbatim: the locale, and the ticket id on the QR route. Preserved
	// deliberately — see capabilityPlaceholder's doc for why.
	segAny
	// segCapability is the redeemable secret. This, and only this, is replaced.
	segCapability
)

// capabilityPlaceholder replaces a capability segment in emitted telemetry.
//
// It is a fixed string, not a hash or a truncation: a hash of a capability is
// still a stable per-order identifier, so it would re-create the correlation
// handle this ticket exists to remove, and a truncated prefix narrows a brute
// force. Correlation is served by trace_id/span_id, which already stamp every
// line (obs.go) and are not derived from the capability.
const capabilityPlaceholder = ":capability"

func lit(s string) segment  { return segment{literal: s, kind: segLiteral} }
func anySeg() segment       { return segment{kind: segAny} }
func capSeg() segment       { return segment{kind: segCapability} }

// capabilityRoutes is the declared set. Adding a capability-bearing URL means
// adding a row here — that is the whole extension point (COS #2).
//
// Both the gateway spelling and the prefix-stripped service spelling appear for
// each access route, for the reason in the file header.
var capabilityRoutes = [][]segment{
	// Guest ticket bundle — the order reference IS the credential.
	{lit("api"), lit("access"), lit("orders"), capSeg(), lit("tickets")},
	{lit("orders"), capSeg(), lit("tickets")},

	// QR image. The ticket id stays readable: it is not redeemable on its own
	// (qrlink.go gates the image on a live short-lived HMAC), and keeping it is
	// what lets an operator correlate a specific image fetch with a report.
	{lit("api"), lit("access"), lit("orders"), capSeg(), lit("tickets"), anySeg(), lit("qr.png")},
	{lit("orders"), capSeg(), lit("tickets"), anySeg(), lit("qr.png")},

	// Storefront bundle page, served through the gateway's "/" catch-all. This
	// is the highest-volume spelling: a buyer hits it before any /api/access
	// call is made, so a fix scoped to the API routes would leave the reference
	// in the logs on every visit.
	{anySeg(), lit("tickets"), capSeg()},
}

// SanitizedPath returns path with any declared capability segment replaced by
// capabilityPlaceholder, and everything else — including ordinary routes —
// returned byte-for-byte unchanged.
//
// Unchanged-on-no-match is load-bearing, not a convenience: /healthz and every
// other route must keep their exact current spelling, which is what the existing
// requestlog_test.go assertion pins.
func SanitizedPath(path string) string {
	if path == "" || path == "/" {
		return path
	}

	// A single trailing slash is a different string, so "/orders/{ref}/tickets/"
	// would miss a table matched on "/orders/{ref}/tickets" and the reference
	// would be logged. Normalise for MATCHING, then restore for OUTPUT, so the
	// logged shape still reflects what was actually requested.
	trimmed := path
	trailing := ""
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = strings.TrimSuffix(trimmed, "/")
		trailing = "/"
	}

	parts := strings.Split(strings.TrimPrefix(trimmed, "/"), "/")

	for _, route := range capabilityRoutes {
		if len(parts) != len(route) {
			continue
		}
		if !matches(parts, route) {
			continue
		}
		out := make([]string, len(parts))
		copy(out, parts)
		for i, seg := range route {
			if seg.kind == segCapability {
				out[i] = capabilityPlaceholder
			}
		}
		return "/" + strings.Join(out, "/") + trailing
	}
	return path
}

// matches reports whether parts satisfies every literal position in route.
//
// An empty segment never matches a variable position: "/api/access/orders//tickets"
// carries no capability, and treating it as one would rewrite a path that leaks
// nothing — a small thing, but it keeps "the output differs" a reliable signal
// that a capability was actually present.
func matches(parts []string, route []segment) bool {
	for i, seg := range route {
		switch seg.kind {
		case segLiteral:
			if parts[i] != seg.literal {
				return false
			}
		case segAny, segCapability:
			if parts[i] == "" {
				return false
			}
		}
	}
	return true
}
