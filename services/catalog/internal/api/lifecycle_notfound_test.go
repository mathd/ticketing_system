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
// TKT-142 left on its own predicate: the rule selects 40 of the document's 42
// operations, and after TKT-178 all 40 declare 404, so it separates nothing at
// HEAD. The two it does not select are authenticateStaff (unknown credentials
// are a 401, not a 404) and getOpenAPISpec (a static document); neither can
// reach writeStoreError, so both drop out of the predicate rather than out of a
// list. Its value is PROSPECTIVE — it fires on the next exported handler that
// can reach writeStoreError and forgets the declaration, which is exactly how
// the eight this ticket fixed came to exist.
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
// document cannot supply. It builds the package's (s *Server) call graph from
// the non-test Go source, takes the transitive closure of methods that can reach
// writeStoreError, and maps the EXPORTED members of that closure to their HTTP
// method and path through the generated ServerInterface's "// (METHOD /path)"
// doc comments.
//
// Two properties do the work, and each was arrived at by being wrong first.
//
// TRANSITIVE, not one hop. See reachesWriteStoreError: matching only a direct
// call in the handler body missed four operations that reach the boundary
// through a helper, and the invariant stayed green with one of their
// declarations deleted.
//
// EXPORTED for the RESULT, everything for the GRAPH. The closure is computed
// over unexported methods too — they are the edges — but only exported members
// are reported as operations:
//
//	An exported (s *Server) method is one that implements the generated
//	ServerInterface; an unexported one CANNOT be, because oapi-codegen only ever
//	generates exported method names. So "unexported" means "not a document
//	operation", by construction rather than by convention — the same kind of
//	structural exclusion lifecycle_badrequest_test.go insists on for
//	getOpenAPISpec, rather than a name list someone must maintain.
//
// At HEAD that leaves out the eight hand-mounted /internal/* chi routes
// (NewRouter in server.go), which are absent from the document and mounted
// OUTSIDE the response validator — the same exclusion the 400 invariant
// documents — and the two write*TransitionError helpers, which are now edges in
// the graph rather than things dropped from it.
//
// Every failure mode below is fatal ON PURPOSE, and none of them is a threshold.
// A discovery step that degrades quietly turns the whole invariant into a test
// that cannot fail, which is the defect class this ticket exists to close.
func discoverStoreErrorOperations(t *testing.T, dir string, doc *openapi3.T) []discoveredOp {
	t.Helper()

	routes := generatedRoutes(t, dir, doc)

	// Pass 1: the whole *Server call graph, exported and unexported alike. The
	// unexported methods are not operations, but they are the EDGES — the four
	// series/festival transitions reach writeStoreError only through one — so a
	// graph that omitted them would reproduce the very hole this pass fixes.
	//
	// parser.ParseFile per source file rather than parser.ParseDir: the latter is
	// deprecated (SA1019) for ignoring build tags, and a per-file walk is what
	// this needs anyway — the filtering is by file NAME, not by package.
	fset := token.NewFileSet()
	callGraph := map[string][]string{}
	exported := map[string]bool{}
	for _, path := range sourceFiles(t, dir) {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !isServerMethod(fn) {
				continue
			}
			callGraph[fn.Name.Name] = calledServerMethods(fn.Body)
			exported[fn.Name.Name] = fn.Name.IsExported()
		}
	}

	// Pass 2: the transitive closure, then the exported members of it.
	reaches := reachesWriteStoreError(callGraph)

	var out []discoveredOp
	seen := map[string]string{} // "METHOD path" -> handler, to catch duplicates
	for method := range callGraph {
		if !exported[method] || !reaches[method] {
			continue
		}
		route, ok := routes[method]
		if !ok {
			t.Fatalf("exported handler %s can reach writeStoreError but does not appear in the "+
				"generated ServerInterface's \"// (METHOD /path)\" comments in openapi_gen.go. "+
				"Either the generator's comment format changed — in which case fix this mapping, "+
				"do NOT fall back to guessing from the method name — or this method is not a "+
				"generated operation and should not be exported on *Server.", method)
		}
		if item := doc.Paths.Find(route.path); item == nil || item.Operations()[route.method] == nil {
			t.Fatalf("handler %s maps to %s %s via openapi_gen.go, but the OpenAPI document has "+
				"no such operation — the generated code and the document have drifted",
				method, route.method, route.path)
		}
		key := route.method + " " + route.path
		if prev, dup := seen[key]; dup {
			t.Fatalf("handlers %s and %s both map to %s — the mapping is not one-to-one, so "+
				"the invariant would silently check one operation twice and another never",
				prev, method, key)
		}
		seen[key] = method
		out = append(out, discoveredOp{handler: method, method: route.method, path: route.path})
	}

	// Two guards, because they fail in different directions and neither implies
	// the other. Both are here because a mutation proved each necessary.
	//
	// (1) COMPLETENESS — every generated operation must be CLASSIFIED, either as
	// able to reach writeStoreError (and so owing a 404) or as demonstrably
	// unable to. An operation the scan never VISITED is neither. This replaced an
	// earlier `len(out) >= 30`, and the ai-review pass was right that a floor
	// permits a materially partial scan: a dropped source file could lose several
	// operations and still clear the threshold, hiding a missing declaration in
	// the subset the scan never saw. Completeness has no number in it and cannot
	// drift as the service grows.
	var unvisited []string
	for handler := range routes {
		if _, walked := callGraph[handler]; !walked {
			unvisited = append(unvisited, handler)
		}
	}
	if len(unvisited) > 0 {
		sort.Strings(unvisited)
		t.Fatalf("the scan never visited %d of the %d handlers the generated ServerInterface "+
			"declares, so it classified neither as owing a 404 nor as unable to produce one — a "+
			"partial scan reports closure over a class it did not see. Check sourceFiles and the "+
			"AST walk. Unvisited:\n  %s", len(unvisited), len(routes), strings.Join(unvisited, "\n  "))
	}

	// (2) NON-EMPTINESS — completeness alone does not imply the classification
	// SEPARATED anything. Every handler can be visited and every one classified
	// "cannot reach the boundary", which is a complete, self-consistent answer
	// that checks nothing. Mutating the boundary's name to one nothing calls
	// makes the closure empty, and the completeness check above stays perfectly
	// happy — the scan visited all 42 and found 0 owing a 404. That mutation was
	// run, it passed, and this assertion is what closes it.
	//
	// It is deliberately not a tuned threshold: 1 is the weakest claim that
	// distinguishes "classified" from "classified as nothing", and it needs no
	// maintenance as the operation count moves.
	if len(out) == 0 {
		t.Fatalf("the scan visited all %d generated handlers and concluded that NONE of them can "+
			"reach writeStoreError, so this invariant checked no operation at all. A complete "+
			"classification that separates nothing is not a passing invariant — it is a broken "+
			"boundary predicate. Check reachesWriteStoreError and calledServerMethods.", len(routes))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].handler < out[j].handler })
	return out
}

// sourceFiles lists the package's hand-written .go files: no _test.go (a test
// file has no generated operation, and this very file mentions writeStoreError
// in prose) and not openapi_gen.go (generated; its wrappers are not handlers).
func sourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "openapi_gen.go" {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	// No floor on the count here on purpose. An under-collected listing is caught
	// strictly better downstream: any file this misses takes its handlers with
	// it, and the completeness check in discoverStoreErrorOperations names every
	// generated handler the scan never visited. A threshold beside a check that
	// already subsumes it reads as load-bearing and is not.
	sort.Strings(out)
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

// calledServerMethods returns the names of every (s *Server) method the body
// calls, by selector name. It over-collects — any `x.Foo()` whose selector
// matches a *Server method name is counted, even if x is not the receiver — and
// that direction is the safe one: this feeds a "can this operation reach the 404
// branch" question, where a false yes costs one contract declaration the handler
// never uses and a false no costs an undetected 500 in production.
func calledServerMethods(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

// reachesWriteStoreError computes, over the whole package's *Server call graph,
// the set of methods that can reach writeStoreError — DIRECTLY or through a
// helper.
//
// The transitive step is not a refinement; it is the difference between an
// invariant and a decoration. A first cut of this file matched only a direct
// `s.writeStoreError(...)` in the handler body, and the ai-review pass on
// TKT-178 found four exported operations it therefore missed: PublishSeries and
// ArchiveSeries route errors through writeSeriesTransitionError, PublishFestival
// and ArchiveFestival through writeFestivalTransitionError, and both helpers end
// in writeStoreError (server_transitions.go:167,246). All four already declared
// 404, so the invariant was green — and stayed green with publishSeries's
// declaration DELETED. It was reporting closure over a class it could not see
// four members of. Executed, not argued: that mutation was run.
//
// A fixed point rather than one hop, because a helper chain can grow.
func reachesWriteStoreError(callGraph map[string][]string) map[string]bool {
	reaches := map[string]bool{"writeStoreError": true}
	for changed := true; changed; {
		changed = false
		for method, callees := range callGraph {
			if reaches[method] {
				continue
			}
			for _, callee := range callees {
				if reaches[callee] {
					reaches[method] = true
					changed = true
					break
				}
			}
		}
	}
	return reaches
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
func generatedRoutes(t *testing.T, dir string, doc *openapi3.T) map[string]generatedRoute {
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
	// EXACT coverage against the document, not a floor. This mapping is the
	// completeness check's own denominator, so a partial parse would quietly
	// shrink what "every generated operation" means and the invariant would
	// report closure over the subset it managed to parse. Comparing against the
	// document makes that impossible and needs no threshold to maintain.
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.method+" "+r.path] = true
	}
	var unmapped []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			if !seen[method+" "+path] {
				unmapped = append(unmapped, method+" "+path)
			}
		}
	}
	if len(unmapped) > 0 {
		sort.Strings(unmapped)
		t.Fatalf("parsed %d \"// (METHOD /path)\" comments from ServerInterface but the document "+
			"declares %d operations these do not cover; the generator's comment format has "+
			"probably changed. Fix the parsing — a partial mapping shrinks the completeness "+
			"check's denominator, so the invariant would report closure over a subset. "+
			"Unmapped:\n  %s", len(routes), len(unmapped), strings.Join(unmapped, "\n  "))
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
