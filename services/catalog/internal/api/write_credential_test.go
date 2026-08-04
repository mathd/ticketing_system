package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/catalog/api"
)

// TKT-191 COS-2/COS-3. The contract invariant, derived from the spec in both
// directions — never from a list of operation names.
//
// A test that enumerates today's 26 writes proves nothing about the 27th, which
// is the operation that will actually ship unguarded. So this reads the spec the
// RUNTIME reads (`apispec.Spec`, the same bytes `NewRouter` loads) and asserts a
// property over every operation in it:
//
//	unsafe  -> requires CatalogStaffWriteCredential AND declares 401
//	safe    -> opts out explicitly with `security: []`
//
// Both directions matter, and the second is not a formality. The opt-out set is
// the public read surface; miss one and a storefront read starts demanding a
// credential (loud), while a future GET that copies `security: []` without
// anyone asking why is silent. Deriving both from the spec means neither the
// count nor the membership is a fact a human has to keep right — the plan draft
// miscounted the safe operations (11 vs the real 10) while writing them out by
// hand, which is precisely the failure this replaces.
func TestCatalogSpecGuardsEveryUnsafeOperationAndOnlyThose(t *testing.T) {
	doc := loadSpec(t)

	var unsafeSeen, safeSeen int
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			id := op.OperationID
			if id == "" {
				t.Fatalf("%s %s has no operationId; the invariant cannot name it", method, path)
			}
			required, declares401 := securityRequires(doc, op, staffWriteSecurityScheme), declaresStatus(op, "401")

			if isSafeMethod(method) {
				safeSeen++
				if required {
					t.Errorf("%s (%s %s) is a safe read but requires %s — public reads must opt out with `security: []`",
						id, method, path, staffWriteSecurityScheme)
				}
				continue
			}

			unsafeSeen++
			if !required {
				t.Errorf("%s (%s %s) is an unsafe operation that does NOT require %s. "+
					"Either add the credential requirement, or if it is deliberately public, say so "+
					"in the operation AND here — an unguarded write is exactly what TKT-191 closed",
					id, method, path, staffWriteSecurityScheme)
			}
			if !declares401 {
				t.Errorf("%s (%s %s) requires the credential but declares no 401. ADR-028 fails closed: "+
					"the refusal would be rejected as contract drift and surface as a 500",
					id, method, path)
			}
		}
	}

	// Guard against the invariant silently covering nothing — a spec that failed
	// to load, or a filter that matched no operation, would otherwise "pass".
	if unsafeSeen == 0 || safeSeen == 0 {
		t.Fatalf("the invariant inspected %d unsafe and %d safe operations; it must see both",
			unsafeSeen, safeSeen)
	}
}

// The scheme itself must exist and be the header the server actually reads. A
// requirement naming a scheme the document does not define is accepted by some
// validators and enforced by none.
func TestCatalogStaffWriteSchemeIsADeclaredHeaderKey(t *testing.T) {
	doc := loadSpec(t)
	ss := doc.Components.SecuritySchemes[staffWriteSecurityScheme]
	if ss == nil || ss.Value == nil {
		t.Fatalf("securitySchemes.%s is not defined", staffWriteSecurityScheme)
	}
	if ss.Value.Type != "apiKey" || ss.Value.In != "header" {
		t.Fatalf("%s must be an apiKey in a header, got type=%q in=%q",
			staffWriteSecurityScheme, ss.Value.Type, ss.Value.In)
	}
	if ss.Value.Name != staffWriteHeader {
		t.Fatalf("the scheme names header %q but the server reads %q — the contract and the "+
			"enforcement must agree or the guard is undocumented", ss.Value.Name, staffWriteHeader)
	}
}

// --- runtime: the guard actually refuses, and says nothing while doing it ---

func TestCatalogRefusesUnsafeRequestWithoutCredential(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"no credential", ""},
		{"wrong credential", "not-the-right-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := len(e.store.events)

			hdr := map[string]string{}
			if tc.header != "" {
				hdr[staffWriteHeader] = tc.header
			}
			rec := e.doWithHeaders("POST", "/events", validEventCreate(), hdr)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("a refusal must be no-store, got %q", cc)
			}
			// The refusal must not describe the credential: not the header name,
			// not the scheme, not whether it was absent versus wrong, and never
			// the submitted value.
			body := rec.Body.String()
			for _, leak := range []string{staffWriteHeader, staffWriteSecurityScheme, "not-the-right-value", "missing", "absent"} {
				if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
					t.Fatalf("the refusal discloses %q: %s", leak, body)
				}
			}
			// And nothing was written. A 401 emitted after the handler ran would
			// look identical from here.
			if len(e.store.events) != before {
				t.Fatalf("the refused request still created an event: %d -> %d", before, len(e.store.events))
			}
		})
	}
}

// Both refusals must be byte-identical, or the difference tells a caller whether
// a credential is configured at all.
func TestCatalogRefusalsAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	absent := e.doWithHeaders("POST", "/events", validEventCreate(), nil)
	wrong := e.doWithHeaders("POST", "/events", validEventCreate(),
		map[string]string{staffWriteHeader: "wrong"})
	if absent.Body.String() != wrong.Body.String() {
		t.Fatalf("absent and wrong credentials differ:\n absent=%s\n wrong=%s",
			absent.Body.String(), wrong.Body.String())
	}
}

// Public reads stay open. This is the storefront's whole relationship with
// catalog, and the ticket must not touch it.
func TestCatalogPublicReadsStayOpenWithoutCredential(t *testing.T) {
	e := newEnv(t)
	rec := e.doWithHeaders("GET", "/public/events", nil, nil)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a public read must not require the staff-write credential: %s", rec.Body.String())
	}
}

// --- helpers ---

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return doc
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// securityRequires reports whether an operation ends up requiring the named
// scheme, honouring the document-level default and an operation-level override
// (including the empty `security: []` opt-out).
func securityRequires(doc *openapi3.T, op *openapi3.Operation, scheme string) bool {
	reqs := doc.Security
	if op.Security != nil {
		reqs = *op.Security
	}
	for _, req := range reqs {
		if _, ok := req[scheme]; ok {
			return true
		}
	}
	return false
}

func declaresStatus(op *openapi3.Operation, status string) bool {
	if op.Responses == nil {
		return false
	}
	return op.Responses.Value(status) != nil
}
