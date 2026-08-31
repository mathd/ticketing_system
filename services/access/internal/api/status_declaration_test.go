package api

// TKT-278. Access's half of the sweep TKT-256 asked for. The reasoning, the method and its
// blind spots are written out once in
// services/inventory/internal/api/status_declaration_test.go — read that file first; this
// one differs only in what access's helpers guarantee.
//
// The short version: the invariant is "every status a handler CAN WRITE is declared", not
// "every operation declares 500", because the latter is answerable from the document and
// therefore measures the spec against itself. Discovery runs over Go source; the document is
// only ever checked. Under ADR-028 an undeclared status is failed closed into a generic 500
// carrying "response violates OpenAPI contract", which hides the real cause during an outage.
//
// ACCESS-SPECIFIC NOTES.
//
// It writes every response through `write(w, code, …)` and has no error-mapping helper of
// inventory's `problem()` kind — so its Floors table is EMPTY, and every status this audit
// derives is a literal in a handler's own body. That makes access the least
// under-approximated of the four, and it is worth saying because an empty Floors map reads
// like an oversight: it is not, it is the absence of the helpers that need one.
//
// `staffOrInternal` is NOT a floor entry, despite the name matching inventory's wrapper. In
// access it returns a bool (redeliveries.go) rather than wrapping a handler, so the 401 is
// written by the handler that consults it and is picked up as an ordinary literal.

import (
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/access/api"
	"ticketing/shared/contract/statusaudit"
)

var auditConfig = statusaudit.Config{
	WriteFuncs: []string{"write"},
	StatusArg:  1,
	// Empty on purpose — see the note above. Access composes no response helper whose
	// status is decided somewhere other than the call site.
	Floors: map[string][]int{},
}

func accessAudit(t *testing.T) ([]statusaudit.Route, *statusaudit.Package, *openapi3.T) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	pkg, err := statusaudit.ParsePackage(".")
	if err != nil {
		t.Fatalf("parse api package: %v", err)
	}
	if pkg.Register == nil {
		t.Fatal("no registerRoutes found in the api package; route discovery has nothing to walk")
	}
	routes := pkg.Routes(func(method, path string) bool {
		item := doc.Paths.Value(path)
		return item != nil && item.GetOperation(method) != nil
	})
	return routes, pkg, doc
}

func TestAccessHandlersOnlyWriteDeclaredStatuses(t *testing.T) {
	routes, pkg, doc := accessAudit(t)
	var diffs []statusaudit.Diff
	for _, r := range routes {
		op := doc.Paths.Value(r.Path).GetOperation(r.Method)
		diffs = append(diffs, statusaudit.Audit(op.OperationID, r.Method+" "+r.Path, op,
			pkg.Emitted(r.Handlers, auditConfig)))
	}
	if report := statusaudit.Report(diffs); report != "" {
		t.Fatalf("these access handlers can write a status their operation does not declare. "+
			"ADR-028's response validator fails closed on one, rewriting it into a generic 500 "+
			"carrying \"response violates OpenAPI contract\" — so the caller cannot see what the "+
			"handler meant. Declare the status (never relax the validator):\n%s", report)
	}
}

// The discovery half, asserted separately: every mutation this file's evidence rests on
// deletes a DECLARATION, and an adapter whose route discovery silently returned nothing
// would survive all of them while reporting closure over the empty set.
func TestAccessStatusDiscoveryCoversTheWholeDocument(t *testing.T) {
	routes, pkg, doc := accessAudit(t)

	if len(pkg.Funcs) == 0 {
		t.Fatal("parsed no function declarations from the api package; the scan is empty and " +
			"every audit above it would report closure over nothing")
	}

	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.Method+" "+r.Path] = true
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
		t.Fatalf("registerRoutes covers %d routes but the document declares %d operations these "+
			"do not cover. Fix the parsing rather than the threshold — a partial mapping shrinks "+
			"what \"every operation\" means and the audit would report closure over a subset:\n  %s",
			len(routes), len(unmapped), strings.Join(unmapped, "\n  "))
	}

	for _, r := range routes {
		if got := pkg.Emitted(r.Handlers, auditConfig); len(got) == 0 {
			t.Fatalf("%s %s: derived no statuses at all from handlers %v. An empty set satisfies "+
				"the audit vacuously, so this is a scan failure, not a clean result",
				r.Method, r.Path, r.Handlers)
		}
	}
}
