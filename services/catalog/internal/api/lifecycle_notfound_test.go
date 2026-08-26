package api

// TKT-178: a handler answers 404 for an entity that does not exist, the
// operation does not declare '404', and ADR-028's fail-closed response validator
// rewrites it into a 500 carrying "response violates OpenAPI contract". The
// caller cannot tell "you referenced something that is not there" from "we
// broke". Observed on POST /venues with an organizer the database does not have.
//
// The handlers are RIGHT: writeStoreError maps store.ErrNotFound to 404
// (server.go:563-566). The contract is wrong. The fix is to declare the status —
// never to relax the validator, which is behaving exactly as ADR-028 decided.
//
// This file is the sibling of lifecycle_badrequest_test.go (TKT-110/TKT-142),
// and copies its single-scan discipline deliberately. It is a SEPARATE file
// because the two invariants are driven from opposite ends, which is the whole
// difficulty of this one:
//
//	400-reachability is visible IN THE DOCUMENT — a format:uuid path parameter, a
//	request body, a query or header parameter. So TKT-142's invariant is
//	spec-only.
//
//	404-reachability is NOT. It depends on whether the HANDLER can return
//	store.ErrNotFound, which is a property of the Go code that the OpenAPI
//	document cannot see. A spec-only predicate for 404 is impossible.
//
// So discovery here runs over the Go source and the OpenAPI document is only
// ever CHECKED, never used to derive the expected set. Deriving the set from the
// document would make the test restate the spec back to itself and pass forever.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	apispec "ticketing/services/catalog/api"
)

// The rule this file enforces:
//
//	Every generated catalog operation whose handler calls writeStoreError must
//	declare a 404-compatible response.
//
// It is COARSER than "every operation that can actually return ErrNotFound".
// That was deliberate. A precise interprocedural reachability analysis over the
// store is brittle, and the alternatives fail the ticket's condition outright: a
// hand-maintained list does not cover the operation nobody has written yet, and
// runtime fixtures cannot cover it either. writeStoreError is the single
// boundary where ErrNotFound becomes a 404, so "calls it" is the mechanically
// checkable over-approximation of "can produce one".
//
// The five operations this admits that cannot currently produce ErrNotFound
// (the list/display reads) gain a contract ALLOWANCE and nothing else —
// declaring a status a handler never emits cannot make it emit one.
//
// Honest statement of what it discriminates TODAY, in the spirit of the note
// TKT-142 left on its own predicate: the rule selects 36 of the document's 42
// operations, and after TKT-178 all 36 declare 404, so it separates nothing at
// HEAD. Its value is PROSPECTIVE — it fires on the next exported handler that
// routes errors through writeStoreError and forgets the declaration, which is
// exactly how the eight this ticket fixed came to exist.
func TestCatalogStoreErrorOperationsDeclareNotFound(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	ops := discoverStoreErrorOperations(t, ".", doc)
	if missing := operationsMissingNotFound(doc, ops); len(missing) > 0 {
		t.Fatalf("these operations route errors through writeStoreError, which maps "+
			"store.ErrNotFound to 404 (server.go:563-566); ADR-028 turns an undeclared 404 into a "+
			"500 carrying \"response violates OpenAPI contract\", so each must declare '404' (or a "+
			"'4XX' range, or a `default:` response). Missing on %d:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// The ONE scan both the live assertion above and the synthetic cases below run,
// shared for the same reason lifecycle_badrequest_test.go shares its own: when
// the live predicate and the edge cases each had a copy, an edit to the live one
// left every edge case green while the real invariant regressed. The edge tests
// would have been pinning their own copy of the logic rather than the contract.
func operationsMissingNotFound(doc *openapi3.T, ops []discoveredOp) []string {
	var missing []string
	for _, op := range ops {
		item := doc.Paths.Find(op.path)
		if item == nil {
			continue // discovery already Fatal'd on this; unreachable here
		}
		spec := item.Operations()[op.method]
		if spec == nil {
			continue
		}
		// Mirror the matcher the response validator itself uses:
		// openapi3filter.ValidateResponse resolves the declaration with
		// Responses.Status(status) and falls back to Default(). Status() accepts
		// the exact key AND the patterned '4XX' range, so all three spellings —
		// '404', '4XX', `default:` — satisfy the contract and must satisfy this
		// test. Identical reasoning to the 400 invariant's.
		if spec.Responses.Status(http.StatusNotFound) == nil && spec.Responses.Default() == nil {
			missing = append(missing, op.method+" "+op.path+" ("+spec.OperationID+") — handler "+op.handler)
		}
	}
	sort.Strings(missing)
	return missing
}

// discoveredOp is one generated operation whose handler calls writeStoreError.
type discoveredOp struct {
	handler string // the Go method name, for the failure message
	method  string // HTTP method, as the generated interface comment spells it
	path    string // OpenAPI path template
}

// discoverStoreErrorOperations is the half of this invariant the OpenAPI
// document cannot supply. It parses the package's non-test Go source for
// EXPORTED (s *Server) methods whose body calls writeStoreError, then maps each
// to its HTTP method and path through the generated ServerInterface's
// "// (METHOD /path)" doc comments.
//
// Why EXPORTED only, and why that is a structural exclusion rather than a name
// list — the distinction lifecycle_badrequest_test.go insists on for
// getOpenAPISpec, and it matters just as much here:
//
//	An exported (s *Server) method is one that implements the generated
//	ServerInterface; an unexported one CANNOT be, because oapi-codegen only ever
//	generates exported method names. So "unexported" means "not a document
//	operation", by construction rather than by convention.
//
// At HEAD that removes exactly ten methods, and every one of them is genuinely
// not an operation: eight are the hand-mounted /internal/* chi routes (NewRouter
// in server.go), which are absent from the document and mounted OUTSIDE the
// response validator — the same exclusion the 400 invariant documents — and two
// (writeSeriesTransitionError, writeFestivalTransitionError) are error-writing
// helpers rather than handlers at all.
//
// The payoff is that with zero legitimate unmapped handlers remaining, the
// fail-hard rules below cost nothing and stay load-bearing forever: any future
// unmapped exported handler is a real defect, so this can Fatal rather than warn.
//
// Every failure mode below is fatal ON PURPOSE. A discovery step that degrades
// quietly turns the whole invariant into a test that cannot fail, which is the
// defect class this ticket exists to close.
func discoverStoreErrorOperations(t *testing.T, dir string, doc *openapi3.T) []discoveredOp {
	t.Helper()

	routes := generatedRoutes(t, dir)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		// Non-test source only: a _test.go file has no generated operation, and
		// this very file mentions writeStoreError in prose.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	var out []discoveredOp
	seen := map[string]string{} // "METHOD path" -> handler, to catch duplicates
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if filepath.Base(name) == "openapi_gen.go" {
				continue // generated; its wrappers are not handlers
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil {
					continue
				}
				if !isServerMethod(fn) || !fn.Name.IsExported() {
					continue
				}
				if !callsWriteStoreError(fn.Body) {
					continue
				}
				route, ok := routes[fn.Name.Name]
				if !ok {
					t.Fatalf("exported handler %s calls writeStoreError but does not appear in the "+
						"generated ServerInterface's \"// (METHOD /path)\" comments in openapi_gen.go. "+
						"Either the generator's comment format changed — in which case fix this mapping, "+
						"do NOT fall back to guessing from the method name — or this method is not a "+
						"generated operation and should not be exported on *Server.", fn.Name.Name)
				}
				if item := doc.Paths.Find(route.path); item == nil || item.Operations()[route.method] == nil {
					t.Fatalf("handler %s maps to %s %s via openapi_gen.go, but the OpenAPI document has "+
						"no such operation — the generated code and the document have drifted",
						fn.Name.Name, route.method, route.path)
				}
				key := route.method + " " + route.path
				if prev, dup := seen[key]; dup {
					t.Fatalf("handlers %s and %s both map to %s — the mapping is not one-to-one, so "+
						"the invariant would silently check one operation twice and another never",
						prev, fn.Name.Name, key)
				}
				seen[key] = fn.Name.Name
				out = append(out, discoveredOp{handler: fn.Name.Name, method: route.method, path: route.path})
			}
		}
	}

	// A FLOOR, not a zero-check. `len(out) == 0` is satisfied by a discovery that
	// finds one operation after the AST walk or the comment regex quietly breaks,
	// and a one-operation invariant looks exactly like a working one from the
	// outside. 30 is comfortably below the 36 at HEAD and far above anything a
	// broken scan produces.
	const floor = 30
	if len(out) < floor {
		t.Fatalf("discovery found only %d handlers calling writeStoreError, want at least %d. "+
			"This invariant is worthless if discovery under-collects, so it fails loudly rather "+
			"than passing on a set it never built. Check the AST walk and the ServerInterface "+
			"comment parsing before lowering this floor.", len(out), floor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].handler < out[j].handler })
	return out
}

// isServerMethod reports whether fn is declared on *Server.
func isServerMethod(fn *ast.FuncDecl) bool {
	if len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Server"
}

// callsWriteStoreError reports whether the body contains a call to
// s.writeStoreError. A call anywhere in the body counts: the question is whether
// the operation can reach the 404 branch at all, not on which path.
func callsWriteStoreError(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "writeStoreError" {
			found = true
			return false
		}
		return true
	})
	return found
}

type generatedRoute struct {
	method string
	path   string
}

// routeComment matches the doc comment oapi-codegen writes above every
// ServerInterface method: "// (POST /venues)".
var routeComment = regexp.MustCompile(`^// \(([A-Z]+) (\S+)\)$`)

// generatedRoutes reads openapi_gen.go's ServerInterface and returns
// method name -> HTTP method + path. The generated interface is the ONLY
// trustworthy mapping: it is produced from the same document the handlers serve,
// so it cannot drift from the routes without the generator drifting too.
func generatedRoutes(t *testing.T, dir string) map[string]generatedRoute {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(dir, "openapi_gen.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse openapi_gen.go: %v", err)
	}

	var iface *ast.InterfaceType
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "ServerInterface" {
			return true
		}
		if it, ok := spec.Type.(*ast.InterfaceType); ok {
			iface = it
		}
		return false
	})
	if iface == nil {
		t.Fatal("openapi_gen.go declares no ServerInterface — the generator's output shape changed, " +
			"and this invariant's handler-to-route mapping depends on it")
	}

	routes := map[string]generatedRoute{}
	for _, m := range iface.Methods.List {
		if len(m.Names) != 1 || m.Doc == nil {
			continue
		}
		for _, c := range m.Doc.List {
			if g := routeComment.FindStringSubmatch(strings.TrimSpace(c.Text)); g != nil {
				routes[m.Names[0].Name] = generatedRoute{method: g[1], path: g[2]}
			}
		}
	}
	// Same reasoning as the floor above: an empty or near-empty mapping makes
	// every lookup fail, and "every lookup failed" must not be able to look like
	// "nothing needed checking".
	if len(routes) < 30 {
		t.Fatalf("parsed only %d \"// (METHOD /path)\" comments from ServerInterface; the generator's "+
			"comment format has probably changed. Fix the parsing — a partial mapping makes the "+
			"invariant check a subset it never announces.", len(routes))
	}
	return routes
}

// The predicate has arms the live document and the live source cannot exercise —
// no catalog operation declares '4XX' or `default:` — so they would ship as
// unfalsifiable claims if only the live spec tested them. Same lesson TKT-142's
// ai-review taught on the 400 invariant: a fix with no test that can fail is how
// the same defect returns.
//
// These build synthetic documents and run them through the SAME
// operationsMissingNotFound the live assertion uses, so the two cannot drift.
// Every document is checked with Validate first — NewRouter validates the
// document before serving it, so a fixture Validate rejects could never reach
// the router, and an invariant "proved" on one would be proved about nothing.
func TestNotFoundDeclarationEdgeCases(t *testing.T) {
	missingIn := func(t *testing.T, doc string) []string {
		t.Helper()
		loader := openapi3.NewLoader()
		d, err := loader.LoadFromData([]byte(doc))
		if err != nil {
			t.Fatalf("load synthetic spec: %v", err)
		}
		if err := d.Validate(loader.Context); err != nil {
			t.Fatalf("synthetic spec is not a valid OpenAPI document, so proving anything on it "+
				"would prove nothing about a contract the router can serve: %v", err)
		}
		// A fixed, handler-derived reference — NOT a set extracted from the
		// document. Deriving it from the document under test is exactly the
		// mistake this whole file is built to avoid.
		ops := []discoveredOp{{handler: "Probe", method: "GET", path: "/p"}}
		return operationsMissingNotFound(d, ops)
	}

	const head = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /p:\n    get:\n      operationId: probe\n"

	t.Run("an exact 404 satisfies the invariant", func(t *testing.T) {
		doc := head + "      responses:\n        '404': {description: gone}\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("an operation declaring '404' must satisfy the invariant, got %v", got)
		}
	})

	// The complement, so the arms above cannot pass by admitting everything.
	// Without this the test stays green if the declaration check is deleted.
	t.Run("no 404, 4XX or default is caught", func(t *testing.T) {
		doc := head + "      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("an operation with no 404/4XX/default must be reported missing, got %v", got)
		}
	})

	// ValidateResponse resolves '4XX' via Responses.Status(404), so the range is
	// contract-correct and must be accepted. Nothing in the live document
	// declares one.
	t.Run("4XX range satisfies the invariant", func(t *testing.T) {
		doc := head + "      responses:\n        '4XX': {description: bad}\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("an operation declaring '4XX' is contract-correct and must satisfy the "+
				"invariant, but it was reported missing: %v", got)
		}
	})

	// The Default() fallback. The live document declares no `default:`, and the
	// 4XX case above only reaches Status(404), so deleting the fallback would
	// leave the whole suite green without this case.
	t.Run("default response alone satisfies the invariant", func(t *testing.T) {
		doc := head + "      responses:\n        default: {description: whatever}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("a `default:` response covers 404 under kin-openapi's status matching, so it "+
				"satisfies the invariant, but the operation was reported missing: %v", got)
		}
	})
}

// The runtime half, and the ticket's actual bug report. The three operations
// below are the ones that genuinely reach store.ErrNotFound today: each maps an
// unknown organizer FK violation to it (postgres.go:70, postgres.go:121,
// channels_postgres.go:61).
//
// Before the '404' declarations landed, every case here failed on
// env.validateResponse's mask check — the handler wrote 404, ADR-028's validator
// rewrote it to a 500 carrying "response violates OpenAPI contract", and that is
// the production defect, observed at the tier it occurs.
//
// The organizer comes from the VERIFIED ASSERTION, not the request body
// (ADR-058/TKT-245). Anyone reproducing this from the original ticket text —
// which posts an "organizer_id" field — will fail to reproduce it and conclude
// the bug is fixed. It is not: the path to it moved.
//
// The five list/display operations are deliberately absent. Their store
// contracts return empty collections, not ErrNotFound, so an "unknown entity"
// fixture for them would assert a 200 and prove nothing about 404.
func TestUnknownOrganizerIsNotFoundRatherThanContractFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   any
		insert func(*fakeStore) int
	}{
		{
			name: "POST /venues", path: "/venues",
			body:   VenueCreate{Name: "Halle A", GaCapacity: 500},
			insert: func(f *fakeStore) int { return len(f.venues) },
		},
		{
			name: "POST /events", path: "/events",
			body:   validEventCreate(),
			insert: func(f *fakeStore) int { return len(f.events) },
		},
		{
			name: "POST /channels", path: "/channels",
			body:   createChannelBody("pos", "Box office", "pos", nil),
			insert: func(f *fakeStore) int { return len(f.channels) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			// The organizer the request's assertion names is the one the store
			// does not have — the production shape exactly: a credential is
			// verified, and the tenant it names is absent from the database.
			e.store.unknownOrganizer = e.organizer

			before := tc.insert(e.store)
			rec := e.do("POST", tc.path, tc.body)

			// Derived from the requirement, not from a run: writeStoreError maps
			// store.ErrNotFound to 404, and ADR-028 says an UNDECLARED status
			// becomes a 500. So the contract owes a 404 here and the client must
			// see one.
			if rec.Code != http.StatusNotFound {
				t.Fatalf("naming an organizer that does not exist must answer 404, got %d %s",
					rec.Code, rec.Body.String())
			}
			// The caller must be able to tell "you referenced something absent"
			// from "we broke" — which is the entire point of the ticket, and is
			// not established by the status alone.
			if got := rec.Body.String(); !strings.Contains(got, "referenced entity not found") {
				t.Fatalf("the 404 must carry the store's actionable message, got %s", got)
			}
			if after := tc.insert(e.store); after != before {
				t.Fatalf("a refused create must not insert: %d rows before, %d after", before, after)
			}
		})
	}
}

// The seam the table above depends on must itself be reachable — if
// unknownOrganizer were never consulted, every case would get a 201 and the
// status assertion would fail loudly, so this is belt-and-braces rather than the
// primary guard. What it adds is the OTHER direction: an UNSET seam must change
// nothing, which is what lets every other test in this package keep its meaning.
func TestUnknownOrganizerSeamIsInertWhenUnset(t *testing.T) {
	e := newEnv(t)
	if e.store.unknownOrganizer != uuid.Nil {
		t.Fatalf("the seam must default to uuid.Nil, got %s", e.store.unknownOrganizer)
	}
	if rec := e.do("POST", "/venues", VenueCreate{Name: "Halle A", GaCapacity: 500}); rec.Code != http.StatusCreated {
		t.Fatalf("with the seam unset a valid create must still succeed, got %d %s", rec.Code, rec.Body.String())
	}
	// And the sentinel really is compared against the organizer, not ignored:
	// an unrelated unknown organizer must not affect this request's tenant.
	e.store.unknownOrganizer = uuid.New()
	if rec := e.do("POST", "/venues", VenueCreate{Name: "Halle B", GaCapacity: 500}); rec.Code != http.StatusCreated {
		t.Fatalf("a seam naming a DIFFERENT organizer must not refuse this one, got %d %s",
			rec.Code, rec.Body.String())
	}
}
