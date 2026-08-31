package api

// TKT-278. Catalog's half of the sweep TKT-256 asked for. The reasoning, the method and its
// blind spots are written out once in
// services/inventory/internal/api/status_declaration_test.go — read that file first; this
// one differs in what catalog's helpers guarantee and in how its routes are discovered.
//
// The short version: the invariant is "every status this audit can DERIVE a handler writes is
// declared" — a sound SUBSET of "every status it can write" (the difference is the blind spots
// below, and it is real: a helper's conditional arms are out of scope). Not, because the latter is answerable from the document and so
// measures the spec against itself. Under ADR-028 an undeclared status is failed closed into
// a generic 500 carrying "response violates OpenAPI contract".
//
// CATALOG-SPECIFIC NOTES.
//
// ROUTE DISCOVERY IS DIFFERENT. Catalog is the one codegen service: it has no
// `registerRoutes`, and its handlers reach routes through the generated `ServerInterface`.
// So discovery reuses `generatedRoutes` from lifecycle_notfound_test.go (TKT-178) — the same
// mapping, with the same exact-coverage check against the document, rather than a second
// copy that could drift from it.
//
// `writeStoreError`'s floor is 500 for the same reason inventory's `problem()` is: its
// default arm answers 500 for any error none of its cases match. Its 400/404/409 arms are
// conditional on particular store sentinels and are out of scope — and catalog already has a
// dedicated invariant for the 404 half (lifecycle_notfound_test.go), which walks the call
// graph to `writeStoreError` and asserts the declaration. This audit is the general case;
// that one is the sharper instrument for the status it covers.
//
// `GET /openapi.yaml` drops out STRUCTURALLY: it serves an embedded []byte and the document
// does not list it under `paths`, so `generatedRoutes` never maps it and the audit never
// sees it. Do not turn that into a name-based exclusion — the exclusion should stay a
// consequence of the route being outside the contract, which is also why it must not acquire
// an artificial 500 obligation.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/catalog/api"
	"ticketing/shared/contract/statusaudit"
)

var statusAuditConfig = statusaudit.Config{
	WriteFuncs: []string{"writeJSON"},
	StatusArg:  1,
	Floors: map[string][]int{
		// The default arm answers 500 for any error none of its cases match. Its other
		// arms are conditional on store sentinels — see the note above.
		"writeStoreError": {http.StatusInternalServerError},
	},
}

// catalogStatusRoutes maps each generated handler name to the route it serves, reusing
// TKT-178's generatedRoutes so the two invariants cannot disagree about the mapping.
func catalogStatusRoutes(t *testing.T, doc *openapi3.T) ([]statusaudit.Route, *statusaudit.Package) {
	t.Helper()
	pkg, err := statusaudit.ParsePackage(".")
	if err != nil {
		t.Fatalf("parse api package: %v", err)
	}
	var routes []statusaudit.Route
	for handler, route := range generatedRoutes(t, ".", doc) {
		item := doc.Paths.Value(route.path)
		if item == nil || item.GetOperation(route.method) == nil {
			continue
		}
		routes = append(routes, statusaudit.Route{
			Method: route.method, Path: route.path, Handlers: []string{handler},
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Method+routes[i].Path < routes[j].Method+routes[j].Path
	})
	return routes, pkg
}

func TestCatalogHandlerStatusFloorIsDeclared(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	routes, pkg := catalogStatusRoutes(t, doc)

	var diffs []statusaudit.Diff
	for _, r := range routes {
		op := doc.Paths.Value(r.Path).GetOperation(r.Method)
		diffs = append(diffs, statusaudit.Audit(op.OperationID, r.Method+" "+r.Path, op,
			pkg.Emitted(r.Handlers, statusAuditConfig)))
	}
	if report := statusaudit.Report(diffs); report != "" {
		t.Fatalf("these catalog handlers can write a status their operation does not declare. "+
			"ADR-028's response validator fails closed on one, rewriting it into a generic 500 "+
			"carrying \"response violates OpenAPI contract\" — so the caller cannot see what the "+
			"handler meant. Declare the status (never relax the validator).\n\n"+
			"NOTE ON WHAT A PASS MEANS: this checks the derived FLOOR, not every status a handler "+
			"can write — a helper's conditional arms are out of scope by design (see the blind "+
			"spots at the top of "+
			"services/inventory/internal/api/status_declaration_test.go). A green run is not a "+
			"proof that no undeclared status exists.\n%s", report)
	}
}

// The discovery half, asserted separately: every mutation this file's evidence rests on
// deletes a DECLARATION, and an adapter whose handler discovery silently returned nothing
// would survive all of them while reporting closure over the empty set.
//
// generatedRoutes already fails on a document operation it cannot map, so the coverage
// direction is covered there; what is added here is that each mapped handler was actually
// FOUND in the source and yields a status.
func TestCatalogStatusDiscoveryReachesEveryHandler(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	routes, pkg := catalogStatusRoutes(t, doc)

	if len(pkg.Funcs) == 0 {
		t.Fatal("parsed no function declarations from the api package; the scan is empty and " +
			"every audit above it would report closure over nothing")
	}
	if len(routes) == 0 {
		t.Fatal("mapped no routes at all; the audit would report closure over the empty set")
	}

	var missing []string
	for _, r := range routes {
		if _, ok := pkg.Funcs[r.Handlers[0]]; !ok {
			missing = append(missing, r.Method+" "+r.Path+" ("+r.Handlers[0]+")")
			continue
		}
		if got := pkg.Emitted(r.Handlers, statusAuditConfig); len(got) == 0 {
			missing = append(missing, r.Method+" "+r.Path+" ("+r.Handlers[0]+"): no statuses derived")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("these generated operations map to a handler the source scan did not find, or "+
			"one it derived no status from. Either is a scan failure rather than a clean result — "+
			"an empty status set satisfies the audit vacuously:\n  %s", strings.Join(missing, "\n  "))
	}
}
