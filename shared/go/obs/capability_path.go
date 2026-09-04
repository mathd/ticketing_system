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
	literal string   // matched (case-insensitively) when kind == segLiteral
	oneOf   []string // the closed set, when kind == segOneOf
	kind    segmentKind
}

type segmentKind uint8

const (
	// segLiteral must match exactly — this is what distinguishes
	// /orders/{ref}/tickets from /orders/{id}/refunds.
	segLiteral segmentKind = iota
	// segAny is a variable segment that is NOT a capability and is preserved
	// verbatim: the ticket id on the QR route. Preserved deliberately — see
	// capabilityPlaceholder's doc for why.
	segAny
	// segCapability is the redeemable secret. This, and only this, is replaced.
	segCapability
	// segOneOf matches a closed set of literals. It exists because "any first
	// segment" is too wide for the storefront rule: the gateway also owns
	// /admin/ and /scanner/, so a bare variable there redacted the last segment
	// of /admin/tickets/123 — no capability, and the identifier an operator
	// needs to diagnose a misroute (ai-review F2).
	segOneOf
)

// capabilityPlaceholder replaces a capability segment in emitted telemetry.
//
// It is a fixed string, not a hash or a truncation: a hash of a capability is
// still a stable per-order identifier, so it would re-create the correlation
// handle this ticket exists to remove, and a truncated prefix narrows a brute
// force. Correlation is served by trace_id/span_id, which already stamp every
// line (obs.go) and are not derived from the capability.
const capabilityPlaceholder = ":capability"

func lit(s string) segment { return segment{literal: s, kind: segLiteral} }
func anySeg() segment      { return segment{kind: segAny} }
func capSeg() segment      { return segment{kind: segCapability} }

func oneOf(values ...string) segment { return segment{oneOf: values, kind: segOneOf} }

// storefrontLocales mirrors LOCALES in web/storefront/src/lib/locales.ts.
//
// Duplicated across a language boundary, and the drift FAILS OPEN: a locale
// added to the storefront but not here means /<new-locale>/tickets/{ref} stops
// being sanitised and the reference goes back into the logs, silently — no test
// in this package can notice, because the shape it would need to test is the one
// nobody declared. That is the worst failure direction, so it is pinned by
// TestStorefrontLocalesMatchTheStorefront, which reads locales.ts and fails when
// the two lists disagree.
var storefrontLocales = []string{"en", "fr"}

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
	//
	// The locale is a CLOSED SET, not a variable: the gateway also owns /admin/
	// and /scanner/ under the same catch-all, and a bare variable here redacted
	// the last segment of /admin/tickets/123 (ai-review F2).
	{oneOf(storefrontLocales...), lit("tickets"), capSeg()},
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

	// MATCH ON A CANONICAL FORM, REWRITE THE ORIGINAL.
	//
	// The logger is mounted OUTSIDE the mux, so it sees the path exactly as it
	// arrived — before any router normalises it, and whether or not the request
	// goes on to 404. So every spelling that a client can put on the wire has to
	// be matched, not just the one the router would settle on. Confirmed by
	// probing: "//api/access/orders/{ref}/tickets",
	// "/api/access/orders/{ref}/tickets//", "/API/ACCESS/..." and a "." segment
	// all reached the log with the reference intact before this was added.
	//
	// Two rules, both narrow:
	//   - empty and "." segments are dropped (a double slash is what a misjoined
	//     base URL produces; "." is inert in a path);
	//   - literal comparison is case-insensitive, because the routes this table
	//     declares are all lower-case ASCII literals.
	//
	// Case-folding applies ONLY to the literal comparison. The capability and
	// any other variable segment are never compared case-insensitively, and the
	// value returned is built from the ORIGINAL segments, so the logged line
	// still shows what was actually requested.
	indices, canonical, poppedAt := canonicalSegments(path)

	for _, route := range capabilityRoutes {
		if len(canonical) != len(route) {
			continue
		}
		if !matches(canonical, route) {
			continue
		}
		// Rewrite in place on the original segmentation, so a non-canonical
		// spelling is reported as sent, minus the capability.
		out := strings.Split(path, "/")
		for i, seg := range route {
			if seg.kind == segCapability {
				out[indices[i]] = capabilityPlaceholder
			}
		}
		// EVERY segment that ".." discarded is redacted too, whatever position
		// it was popped from (ai-review F9).
		//
		// The narrow version of this rule — redact only what was popped from a
		// CAPABILITY position — was not enough: in
		// "/api/<ref>/../access/orders/<ref-2>/tickets" the first reference is
		// popped from a LITERAL position, so nothing claimed it and it stayed in
		// the line. A sweep with live references in every junk slot found twelve
		// such spellings.
		//
		// The rule is deliberately blunt, because the alternative requires
		// knowing whether a discarded value was a credential, and it is not
		// knowable here. What IS known: this request resolves to a capability
		// route, and these segments were thrown away by path traversal on the
		// way. A discarded segment has no diagnostic value that justifies the
		// risk of it being a second live reference.
		for _, popped := range poppedAt {
			for _, idx := range popped {
				out[idx] = capabilityPlaceholder
			}
		}
		return strings.Join(out, "/")
	}
	return path
}

// canonicalSegments returns the significant segments of path, together with
// their index in strings.Split(path, "/") so the caller can rewrite the original
// spelling rather than a normalised one.
//
// Empty and "." segments are dropped, and ".." pops the previous segment — the
// same reduction net/http's ServeMux performs before matching. Doing less than
// the router does is a leak, not a simplification: ServeMux answers
// "/api/access/unused/../orders/{ref}/tickets" with a 307 to the canonical path,
// so it is a WORKING request, and RequestLogger — mounted outside the mux — sees
// the un-reduced spelling for both the redirect and the follow-up. Matching only
// "." and "" left that reference in the log (ai-review F5).
//
// The popped entry's index goes with it, so the surviving segments keep pointing
// at their real position in the original string and the rewrite still lands on
// the right one.
//
// popped records, per surviving position, the original indices that were popped
// AWAY from that position. Those segments are still present in the original
// string, and if the position turns out to be a capability then every value ever
// offered there is a live credential — not just the one that survived
// (ai-review F9).
func canonicalSegments(path string) (indices []int, segments []string, popped map[int][]int) {
	popped = map[int][]int{}
	for i, seg := range strings.Split(path, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			// Pop, or ignore at the root: "/../x" is "/x" to the router, not an
			// error. Never let it underflow into a negative index.
			if n := len(segments); n > 0 {
				slot := n - 1
				popped[slot] = append(popped[slot], indices[slot])
				indices = indices[:slot]
				segments = segments[:slot]
			}
			continue
		}
		indices = append(indices, i)
		segments = append(segments, seg)
	}
	return indices, segments, popped
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
			// EqualFold, not ==, and this is DELIBERATELY BROADER THAN THE
			// ROUTER (ai-review F8).
			//
			// The routers are case-sensitive: ServeMux answers 404 to
			// "/API/ACCESS/orders/{ref}/tickets", and the storefront's isLocale
			// is an exact match. So folding redacts the capability position of
			// some requests no handler will serve.
			//
			// That is the intended trade, stated rather than stumbled into. The
			// two errors are not symmetric: redacting a route-shaped 404 costs
			// an operator one segment of a request that did nothing, while NOT
			// redacting it writes a live, redeemable, unauthenticated credential
			// to the log — the 404 does not make the reference any less valid,
			// and RequestLogger records it either way because it runs before the
			// router. Sanitising follows the SHAPE OF THE SECRET, not route
			// identity.
			//
			// Folding cannot widen onto a different real route: every literal in
			// the table is lower-case ASCII, so a fold can only match spellings
			// that differ from a declared literal by case alone. If a
			// case-distinct sibling route is ever added, this assumption breaks
			// and the table needs explicit case handling.
			if !strings.EqualFold(parts[i], seg.literal) {
				return false
			}
		case segOneOf:
			var ok bool
			for _, v := range seg.oneOf {
				if strings.EqualFold(parts[i], v) {
					ok = true
					break
				}
			}
			if !ok {
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
