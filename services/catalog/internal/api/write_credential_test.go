package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// The assertion scheme must exist and name the header the server actually reads,
// for the same reason the staff-write scheme must (TKT-245).
func TestCatalogOrganizerAssertionSchemeIsADeclaredHeaderKey(t *testing.T) {
	doc := loadSpec(t)
	ss := doc.Components.SecuritySchemes[organizerAssertionSecurityScheme]
	if ss == nil || ss.Value == nil {
		t.Fatalf("securitySchemes.%s is not defined", organizerAssertionSecurityScheme)
	}
	if ss.Value.Type != "apiKey" || ss.Value.In != "header" {
		t.Fatalf("%s must be an apiKey in a header, got type=%q in=%q",
			organizerAssertionSecurityScheme, ss.Value.Type, ss.Value.In)
	}
	if ss.Value.Name != organizerAssertionHeader {
		t.Fatalf("the scheme names header %q but the server reads %q — the contract and the "+
			"enforcement must agree or the guard is undocumented", ss.Value.Name, organizerAssertionHeader)
	}
}

// ⚠️ THE AND-NESS TEST. Every operation carrying the organizer assertion requires
// it TOGETHER WITH the staff credential, never as an alternative to it.
//
// Why this exists as its own test, when the invariant above already checks that
// every unsafe operation requires the staff credential: `securityRequires` (and
// OpenAPI itself) treats the security LIST as OR and the keys WITHIN one
// requirement object as AND. So
//
//	security: [{Staff: []}, {Assertion: []}]     <- either alone suffices
//	security: [{Staff: [], Assertion: []}]       <- both required
//
// differ by one bracket, and the FIRST form passes both the existing invariant
// and any naive "the assertion is declared on these operations" coverage test —
// while silently meaning that presenting the assertion ALONE, with no staff
// credential, is enough. That is an auth downgrade in the exact property this
// ticket exists to add, and nothing else catches it: not the invariant, not the
// gate, and not mutation testing, whose mutants flip the mechanism rather than
// the declaration.
//
// Derived from the spec in both directions, like the invariant it sits beside:
// which operations carry the assertion is read out of the document, never listed
// here, so operation 16 is covered the day it is added.
func TestCatalogOrganizerAssertionIsRequiredTogetherWithTheStaffCredential(t *testing.T) {
	doc := loadSpec(t)

	var carriers int
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			reqs := doc.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			for _, req := range reqs {
				if _, wantsAssertion := req[organizerAssertionSecurityScheme]; !wantsAssertion {
					continue
				}
				carriers++
				if _, alsoStaff := req[staffWriteSecurityScheme]; !alsoStaff {
					t.Errorf("%s (%s %s) has a security requirement naming %s WITHOUT %s. "+
						"That makes the assertion an ALTERNATIVE to the staff credential rather than an "+
						"addition to it: a caller presenting only an assertion satisfies this operation. "+
						"Both schemes belong in ONE requirement object.",
						op.OperationID, method, path,
						organizerAssertionSecurityScheme, staffWriteSecurityScheme)
				}
			}
		}
	}

	// The assertion must actually be declared somewhere, or this test passes by
	// inspecting nothing — the shape of green test this repo has been bitten by.
	if carriers == 0 {
		t.Fatalf("no operation requires %s; the invariant inspected nothing",
			organizerAssertionSecurityScheme)
	}
}

// Every write that took organizer_id from the body now requires the assertion,
// and no such operation still declares the field.
//
// Both halves matter and neither implies the other: an operation could require
// the assertion and still accept a body organizer (the field would be dead but
// submittable, and a later reader would wire it back up), or drop the field
// without requiring the assertion (catalog would then have no organizer at all).
//
// Membership is derived from the REQUEST SCHEMA, not from a list of names: an
// operation is in scope precisely because it used to name an organizer, and the
// day someone adds a 16th, this test already covers it.
func TestCatalogWritesTakeTheOrganizerFromTheAssertionAndNotTheBody(t *testing.T) {
	doc := loadSpec(t)

	// The operations this ticket converted, derived from the contract rather than
	// listed: an unsafe operation that requires the assertion is one whose
	// organizer now comes from the credential.
	var converted int
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if isSafeMethod(method) {
				continue
			}
			if !securityRequires(doc, op, organizerAssertionSecurityScheme) {
				continue
			}
			converted++

			// Direction 1: the field must be gone. A dead-but-submittable field is
			// what a later reader wires back up.
			if op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			media := op.RequestBody.Value.Content.Get("application/json")
			if media == nil || media.Schema == nil || media.Schema.Value == nil {
				continue
			}
			if _, stillDeclared := media.Schema.Value.Properties["organizer_id"]; stillDeclared {
				t.Errorf("%s (%s %s) requires the assertion but STILL declares organizer_id in its request "+
					"body. The organizer must be UNSUBMITTABLE, not validated: a field the client can send "+
					"is a trust boundary that moved rather than closed (AGENTS.md, TKT-244).",
					op.OperationID, method, path)
			}
		}
	}

	// Direction 2: no unsafe operation still takes an organizer from its body. A
	// write left behind would keep the whole model unchanged for that endpoint,
	// which is precisely what the COS refuses ("a fix that scoped one endpoint
	// would leave the model unchanged and the claim still unearned").
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if isSafeMethod(method) || op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			media := op.RequestBody.Value.Content.Get("application/json")
			if media == nil || media.Schema == nil || media.Schema.Value == nil {
				continue
			}
			if _, declaresOrganizer := media.Schema.Value.Properties["organizer_id"]; !declaresOrganizer {
				continue
			}
			t.Errorf("%s (%s %s) takes organizer_id from the request body and does not require %s. "+
				"Either it was missed by the conversion, or it is a new write that inherited the old shape.",
				op.OperationID, method, path, organizerAssertionSecurityScheme)
		}
	}

	if converted == 0 {
		t.Fatal("no unsafe operation requires the organizer assertion; the invariant inspected nothing")
	}
}

// A handler reached with NO verified scope refuses, rather than writing for the
// nil organizer.
//
// This test exists because a mutation check found the gap it closes: deleting the
// refusal from `organizerFor` -- so it returns the zero scope and `true` -- left
// the ENTIRE package green. Every other test drives the router, where the
// validator fills the slot for all 15 operations before any handler runs, so
// nothing could ever reach the unfilled state. A fixture that cannot construct
// the failing state cannot fail (AGENTS.md), and the guard was resting on that.
//
// So this one calls the handler DIRECTLY, with a bare context: the construction
// path where the slot was never installed -- an operation that reaches a
// converted handler without declaring the assertion, or a future hand-mounted
// route. What must not happen is a write attributed to uuid.Nil.
func TestConvertedHandlerRefusesWhenNoScopeWasVerified(t *testing.T) {
	e := newEnv(t)
	before := len(e.store.venues)

	// No withOrganizerScopeSlot, no AuthenticationFunc: exactly the state the
	// router never produces and a mistake would.
	req := httptest.NewRequest(http.MethodPost, "http://catalog.local/venues",
		strings.NewReader(`{"name":"Nobody's Hall","ga_capacity":10}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv := NewServer(e.store, e.pub, slog.New(slog.NewTextHandler(io.Discard, nil)),
		testInternalToken, testStaffWriteToken).WithOrganizerAssertionKey(testOrganizerAssertionKey)
	srv.CreateVenue(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a handler with no verified scope answered %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if len(e.store.venues) != before {
		t.Fatalf("a venue was created with no verified organizer: %d -> %d", before, len(e.store.venues))
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
