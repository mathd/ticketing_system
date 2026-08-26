package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

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

			// Does ANY alternative name the assertion? If so this operation is
			// assertion-protected, and EVERY alternative must then carry both
			// schemes — see below for why checking only the naming ones is not
			// enough.
			var assertionProtected bool
			for _, req := range reqs {
				if _, ok := req[organizerAssertionSecurityScheme]; ok {
					assertionProtected = true
					break
				}
			}
			if !assertionProtected {
				continue
			}
			carriers++

			// ai-review [medium], CONFIRMED by execution: an earlier version of
			// this test inspected only the requirement objects that CONTAIN the
			// assertion, and confirmed those also contained the staff credential.
			// A sibling object without the assertion was invisible to it, so
			//
			//   security: [{Staff: [], Assertion: []}, {Staff: []}]
			//
			// passed this test, passed the staff-credential invariant, and passed
			// the coverage test — while declaring that the staff credential ALONE
			// is an accepted alternative. Verified by adding exactly that to one
			// operation and watching every test stay green.
			//
			// That is the same defect this test exists to catch, one level up: the
			// fix reproduced the shape of the bug it was fixing. So the rule is
			// now stated over ALL alternatives, not the naming ones.
			//
			// (What saved the runtime meanwhile: kin-openapi requires every
			// declared alternative to pass rather than any one of them, so the
			// staff-only request was still refused 401 — the reviewer's claim that
			// runtime would admit it does not hold for this validator. The CONTRACT
			// would still have said otherwise, and the contract is what the next
			// reader and any other client believe.)
			for i, req := range reqs {
				_, hasStaff := req[staffWriteSecurityScheme]
				_, hasAssertion := req[organizerAssertionSecurityScheme]
				if hasStaff && hasAssertion {
					continue
				}
				t.Errorf("%s (%s %s) is assertion-protected but its security alternative #%d names "+
					"{staff:%v assertion:%v}. EVERY alternative must carry BOTH, or the operation "+
					"declares a weaker way in: a list of requirement objects is an OR, and one that "+
					"omits a scheme says that scheme is optional.",
					op.OperationID, method, path, i, hasStaff, hasAssertion)
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

// Every handler that READS the verified organizer sits behind an operation that
// REQUIRES the assertion.
//
// ai-review pass 2 [high], confirmed by execution. The test above grounds
// membership in the security declaration itself — it inspects an operation only
// after finding an alternative that names the assertion. So deleting the
// assertion ENTIRELY from one operation does not fail it; the operation simply
// drops out of scope. Verified: with one converted operation changed to
// `security: [{CatalogStaffWriteCredential: []}]`, every contract invariant in
// this file stayed green.
//
// The mutant was caught only INCIDENTALLY, by functional tests failing because
// the handler calls organizerFor and gets no scope. That is luck, not coverage:
// it depends on the handler's fallback, the contract meanwhile advertises
// staff-only access, and a future converted operation whose handler does not go
// through organizerFor would be silently unguarded with nothing to say so.
//
// So membership is grounded in something the DECLARATION CANNOT MOVE: the Go
// source itself, parsed here rather than transcribed.
//
// ai-review pass 3 [high], confirmed: an earlier version of this test hardcoded
// the 15 operationIds and claimed in this very comment to be "grounded in the Go
// source" — while the list was a literal someone had typed after grepping once.
// That is worse than deriving, for the failure mode that matters: add a handler
// that calls organizerFor, omit its assertion requirement, and omit it from the
// list, and BOTH loops skip it. The forward loop iterates the list; the reverse
// loop only inspects operations that already require the assertion. A new
// unguarded write would be invisible to the test written to prevent exactly that.
//
// It also could not check its own converse — an id could stay listed long after
// its handler stopped reading the verified scope.
//
// Now the set comes from the AST: every method whose body mentions organizerFor.
// A handler is in scope because of what it DOES, and the equivalence is asserted
// in both directions, so all four regressions fail loudly — a new organizer-
// scoped handler without its requirement, a requirement removed from an existing
// one, an assertion declared on an operation whose handler no longer reads the
// scope, and the OR-alternative shapes the tests above cover.
func TestEveryHandlerReadingTheVerifiedOrganizerRequiresTheAssertion(t *testing.T) {
	doc := loadSpec(t)

	readsVerifiedOrganizer := handlersReadingTheVerifiedOrganizer(t)
	if len(readsVerifiedOrganizer) == 0 {
		t.Fatal("no handler was found calling organizerFor — the AST scan matched nothing, so this " +
			"invariant would pass while inspecting an empty set")
	}

	byID := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				continue
			}
			if prior, dup := byID[op.OperationID]; dup && prior != op {
				t.Fatalf("operationId %q appears twice (%s %s); the id->operation map cannot be "+
					"trusted and this invariant would silently inspect only one of them",
					op.OperationID, method, path)
			}
			byID[op.OperationID] = op
		}
	}

	// Handler -> operation. oapi-codegen names the method after the operationId
	// with an upper-case first letter, so the mapping is exact rather than fuzzy.
	operationFor := func(handler string) (string, *openapi3.Operation, bool) {
		id := strings.ToLower(handler[:1]) + handler[1:]
		op, ok := byID[id]
		return id, op, ok
	}

	// Direction 1: everything that reads the verified organizer requires it.
	requiredIDs := map[string]bool{}
	for _, handler := range readsVerifiedOrganizer {
		id, op, ok := operationFor(handler)
		if !ok {
			// A hand-mounted route (listChannels) reads the scope inline and is not
			// in the contract at all. That is ADR-043's line, not a defect — but it
			// must be named here, or an operation that genuinely vanished from the
			// contract would look like one of these.
			if handler == "listChannels" {
				continue
			}
			t.Errorf("handler %s reads the verified organizer but no contract operation is named %q; "+
				"if it is deliberately hand-mounted, name it in this test's exception", handler, id)
			continue
		}
		requiredIDs[id] = true
		if !securityRequires(doc, op, organizerAssertionSecurityScheme) {
			t.Errorf("%s reads the verified organizer in its handler but its operation does NOT "+
				"require %s. The contract would advertise staff-only access while the handler "+
				"refuses at runtime — a boundary that exists only as a fallback.",
				id, organizerAssertionSecurityScheme)
		}
		if !securityRequires(doc, op, staffWriteSecurityScheme) {
			t.Errorf("%s does not require %s", id, staffWriteSecurityScheme)
		}
	}

	// Direction 2: nothing requires the assertion whose handler does not read it.
	// A declaration without a reader is a boundary nobody enforces.
	for id, op := range byID {
		if !securityRequires(doc, op, organizerAssertionSecurityScheme) {
			continue
		}
		if !requiredIDs[id] {
			t.Errorf("%s requires %s but no handler reading the verified organizer maps to it. "+
				"Either the handler stopped taking the organizer from the verified scope — in which "+
				"case it is taking it from somewhere else — or the requirement is decoration.",
				id, organizerAssertionSecurityScheme)
		}
	}
}

// handlersReadingTheVerifiedOrganizer parses this package and returns the names
// of every method whose body calls organizerFor.
//
// Parsed rather than listed, because a list is a fact a human has to keep right
// and this one is load-bearing: it decides which operations the invariant above
// even looks at. The same argument the staff-write invariant makes for deriving
// its operation set from the spec (see the top of this file), applied to the
// other side of the boundary.
func handlersReadingTheVerifiedOrganizer(t *testing.T) []string {
	t.Helper()

	// Files walked and parsed individually rather than with parser.ParseDir,
	// which staticcheck flags as deprecated (SA1019). Nothing here needs package
	// assembly or build-tag resolution — the question is only "which methods
	// mention organizerFor" — so the simpler API is also the correct one.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var handlers []string
	var parsed int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil || sel.Sel.Name != "organizerFor" {
					return true
				}
				handlers = append(handlers, fn.Name.Name)
				return false
			})
		}
	}
	// A scan that read no files would report "no handlers" — indistinguishable
	// from a package where the guard had been deleted everywhere.
	if parsed == 0 {
		t.Fatal("the AST scan parsed no source files; it would report an empty handler set")
	}
	slices.Sort(handlers)
	return slices.Compact(handlers)
}

// And the runtime agrees with the contract: neither credential alone opens a
// converted write.
//
// The contract test above is a statement about the DOCUMENT. This one drives the
// real router, because "the declaration is right" and "the guard refuses" are two
// claims and the ticket needs both. ai-review's finding included a prediction
// about runtime behaviour that turned out to be wrong for this validator — which
// is exactly the kind of thing that should be settled by executing it rather than
// by reading either the reviewer's argument or mine.
func TestConvertedWriteRefusesEitherCredentialAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"staff credential alone", map[string]string{staffWriteHeader: testStaffWriteToken}},
		{"assertion alone", nil}, // filled below: needs the env's key
		{"neither", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			headers := tc.headers
			if tc.name == "assertion alone" {
				headers = map[string]string{organizerAssertionHeader: e.assertionFor(e.organizer)}
			}
			// TKT-200: a VALID idempotency key, so this test still reaches the
			// guard it is about. The generated wrapper binds the required header
			// before HandlerMiddlewares run, so a keyless request answers 400 and
			// the 401 assertion below would be measuring the wrong refusal — an
			// authorization suite silently re-pointed at parameter binding. The
			// key is deliberately present and valid; nothing here is testing it.
			if headers == nil {
				headers = map[string]string{}
			}
			headers["Idempotency-Key"] = "credential-guard-" + uuid.NewString()
			before := len(e.store.events)
			rec := e.doWithHeaders(http.MethodPost, "/events", validEventCreate(), headers)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s = %d, want 401: %s", tc.name, rec.Code, rec.Body.String())
			}
			if len(e.store.events) != before {
				t.Fatalf("%s still created an event: %d -> %d", tc.name, before, len(e.store.events))
			}
		})
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

// Deleting the field is not enough: the schema must REFUSE a submitted one.
//
// The difference is the whole distance between "unsubmittable" and "ignored",
// and it is invisible unless you look. A schema without `additionalProperties:
// false` silently DROPS an extra `organizer_id` — so a client can still send it,
// gets a 201, and is told nothing. The write lands under the assertion's
// organizer, which is safe; but "safe because nothing reads it" is a property of
// today's handler, and the next reader sees a field the API appears to accept.
//
// Found by the gate rather than by review: a smoke test still sending a forged
// organizer expected a refusal and got 201, because 13 of the 15 schemas were
// lax. The security outcome was already correct; the contract was lying.
func TestConvertedWriteSchemasRefuseASubmittedOrganizer(t *testing.T) {
	doc := loadSpec(t)

	var checked int
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if isSafeMethod(method) || !securityRequires(doc, op, organizerAssertionSecurityScheme) {
				continue
			}
			if op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			media := op.RequestBody.Value.Content.Get("application/json")
			if media == nil || media.Schema == nil || media.Schema.Value == nil {
				continue
			}
			checked++
			if schema := media.Schema.Value; schema.AdditionalProperties.Has == nil || *schema.AdditionalProperties.Has {
				t.Errorf("%s (%s %s) does not set additionalProperties: false, so a submitted "+
					"organizer_id is silently ignored rather than refused. Unsubmittable means the "+
					"client cannot send it, not that the server drops it quietly.",
					op.OperationID, method, path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no converted write schema was inspected")
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

			// TKT-200: valid key, so the 401 below is the credential guard's
			// refusal and not the wrapper's missing-header 400. See
			// TestConvertedWriteRefusesEitherCredentialAlone for the full reason.
			hdr := map[string]string{"Idempotency-Key": "credential-guard-" + uuid.NewString()}
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
	// TKT-200: both requests carry a valid key, so both reach the credential
	// guard. Without one they would BOTH answer 400 and still be identical —
	// the assertion would pass while comparing two parameter-binding errors and
	// saying nothing about credentials.
	absent := e.doWithHeaders("POST", "/events", validEventCreate(),
		map[string]string{"Idempotency-Key": "refusal-" + uuid.NewString()})
	wrong := e.doWithHeaders("POST", "/events", validEventCreate(),
		map[string]string{staffWriteHeader: "wrong", "Idempotency-Key": "refusal-" + uuid.NewString()})
	if absent.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("both must be credential refusals, got %d and %d", absent.Code, wrong.Code)
	}
	if absent.Body.String() != wrong.Body.String() {
		t.Fatalf("absent and wrong credentials differ:\n absent=%s\n wrong=%s",
			absent.Body.String(), wrong.Body.String())
	}
}

// TestMissingIdempotencyKeyRefusalPrecedesTheCredentialGuard pins an ordering
// that is easy to assume the other way round and is NOT what happens (TKT-200).
//
// oapi-codegen's wrapper binds and validates declared parameters BEFORE applying
// HandlerMiddlewares, and catalog's security check is one of those middlewares
// (the validator in NewRouter). So an unauthenticated request that also omits
// the key answers 400 — naming the header — rather than 401.
//
// Recorded as a test rather than a comment because it has a real consequence:
// every authorization test on these three operations MUST send a valid key, or
// it silently stops testing authorization. Two such tests in this file were
// re-pointed at parameter binding by this ticket before this was understood.
//
// It is not a disclosure problem: the 400 names only a header the OpenAPI
// document already declares publicly, and it reveals nothing about whether a
// credential exists or is correct. If that judgement ever changes, the fix is
// a pre-router guard like guardInternalSurface — not a handler-level check,
// which by this same ordering could never run first.
// TestIdempotencyKeyBoundsAreEnforced answers an ai-review [high] that read the
// generated binder in isolation and concluded the declared 1..200 bounds were
// documentation only — that an empty header would reach the store and create an
// unprotected keyless row, and an over-long one would violate the column CHECK
// and surface as a 500.
//
// Refuted by executing it, which is the only thing that settles this class of
// claim. Catalog's request path is binder THEN kin-openapi request validator,
// and between them both bounds are enforced before any handler runs. This test
// is the standing proof, so the question is not re-litigated from the generated
// code again.
func TestIdempotencyKeyBoundsAreEnforced(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"empty", ""},
		{"one over the maximum", strings.Repeat("k", 201)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.doWithHeaders(http.MethodPost, "/events", validEventCreate(), map[string]string{
				staffWriteHeader:         testStaffWriteToken,
				organizerAssertionHeader: e.assertionFor(e.organizer),
				"Idempotency-Key":        tc.key,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s key = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
			}
			// The refusal must happen BEFORE the store, or an empty key becomes
			// a keyless row that the partial index does not protect.
			if len(e.store.events) != 0 {
				t.Fatalf("%s key still created %d events", tc.name, len(e.store.events))
			}
		})
	}

	// The bound is inclusive at the top: exactly 200 is valid. Present so a
	// future "fix" cannot satisfy the two cases above by refusing everything.
	t.Run("exactly the maximum is accepted", func(t *testing.T) {
		e := newEnv(t)
		rec := e.doWithHeaders(http.MethodPost, "/events", validEventCreate(), map[string]string{
			staffWriteHeader:         testStaffWriteToken,
			organizerAssertionHeader: e.assertionFor(e.organizer),
			"Idempotency-Key":        strings.Repeat("k", 200),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("a 200-character key = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestMissingIdempotencyKeyRefusalPrecedesTheCredentialGuard(t *testing.T) {
	e := newEnv(t)
	rec := e.doWithHeaders("POST", "/events", validEventCreate(), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no key and no credential = %d, want 400 (binding precedes the guard): %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Idempotency-Key") {
		t.Fatalf("the 400 must name the missing header, got %s", rec.Body.String())
	}
	// And it must still not have written anything.
	if len(e.store.events) != 0 {
		t.Fatalf("a request refused at binding created %d events", len(e.store.events))
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
