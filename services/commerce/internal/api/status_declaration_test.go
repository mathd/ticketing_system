package api

// TKT-278. Commerce's half of the sweep TKT-256 asked for. The reasoning, the method and its
// blind spots are written out once in
// services/inventory/internal/api/status_declaration_test.go — read that file first; this
// one differs only in what commerce's helpers guarantee.
//
// The short version: the invariant is "every status this audit can DERIVE a handler writes is
// declared" — a sound SUBSET of "every status it can write" (the difference is the blind spots
// below, and it is real: a helper's conditional arms are out of scope). Not. Commerce is where that distinction was first shown to
// matter: a naive port of the payments 500-class test flags `getOrder` and
// `getDeliveryEmail` here, and BOTH ARE CORRECT — they answer 503, not 500, because
// `persistenceReadProblem` returns exactly {404,503}, and both declare it. A check that asks
// the document what the document says would have "found" two defects that are not there and
// missed everything else.
//
// COMMERCE-SPECIFIC NOTES.
//
// Two helpers RETURN a status instead of writing one — `persistenceReadProblem` and
// `checkoutClaimProblem`, both `func(error) (int, string)` whose result is passed to
// `write`. They are declared in ReturningFuncs so the walker contributes their set when the
// value reaches a write, rather than needing the handler to spell the status out.
//
// Their floors are their TOTAL return sets, not a subset, and that is a property of these
// two functions rather than a general rule: each arm is reachable for some input, and there
// is no default arm answering one status for everything the way inventory's `problem()`
// does. So unlike `problem()`, listing every status here is sound.
//
// The `write*` wrappers (`writeRefund`, `writeExchange`, `writeAwaitingReconciliation`,
// `writeTooManyRequests`) need no entry: each calls `write` with a literal, so the ordinary
// transitive walk picks them up. Do not add them as floors — a floor SHORT-CIRCUITS the walk
// into a body, so a floor here would replace what those functions actually write with
// whatever the table said, which is the drift a hand-maintained list is supposed to avoid.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/commerce/api"
	"ticketing/shared/contract/statusaudit"
)

var auditConfig = statusaudit.Config{
	WriteFuncs: []string{"write"},
	StatusArg:  1,
	Floors: map[string][]int{
		// Exactly two arms and they partition every error: sql.ErrNoRows → 404, anything
		// else → 503. This is the function whose 503 makes the naive 500-class check wrong
		// about getOrder and getDeliveryEmail.
		"persistenceReadProblem": {http.StatusNotFound, http.StatusServiceUnavailable},
		// Three arms, two statuses: the two conflict sentinels → 409, everything else → 500.
		"checkoutClaimProblem": {http.StatusConflict, http.StatusInternalServerError},
	},
	ReturningFuncs: map[string]bool{
		"persistenceReadProblem": true,
		"checkoutClaimProblem":   true,
	},
}

// unreachableOnRoute records a status the walker derives for a route that the route cannot
// actually reach, with the reason. Each entry is a claim about production code, and the walk
// over-collects in exactly one documented way — a callee SHARED by two routes contributes
// its whole set to both — so this exists to record where that bites, not to silence output.
//
// KEEP IT SMALL AND KEEP THE REASON. The alternative is teaching the walker to evaluate
// arguments, which is the symbolic analyzer this audit deliberately is not: it would need to
// know that `reserveWithScope(w, r, nil)` cannot enter `if scope != nil`, and once it models
// that it must model every other guard too, and every failure becomes a fact about whether
// the model matches the code. One reasoned line is cheaper and more honest.
//
// The cost of an entry is real: it is a status this audit will never report for that route
// again, so a genuine future emission of it hides here. Add one only after reading the path.
var unreachableOnRoute = map[string][]int{
	// POST /reservations → s.reserve → reserveWithScope(w, r, **nil**). The 403 lives
	// inside `if scope != nil` (server.go), which is the PARTNER path: only
	// partnerReserve passes a non-nil scope, and it is mounted at /partners/reservations,
	// which the document does not declare — so it is out of this audit's scope entirely and
	// gains nothing from the 403 being counted here. Verified by reading both call sites.
	"POST /reservations": {http.StatusForbidden},
}

func commerceAudit(t *testing.T) ([]statusaudit.Route, *statusaudit.Package, *openapi3.T) {
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

// emittedOn is the derived set minus this route's recorded-unreachable statuses.
func emittedOn(r statusaudit.Route, pkg *statusaudit.Package) []int {
	var out []int
	excluded := map[int]bool{}
	for _, s := range unreachableOnRoute[r.Method+" "+r.Path] {
		excluded[s] = true
	}
	for _, s := range pkg.Emitted(r.Handlers, auditConfig) {
		if !excluded[s] {
			out = append(out, s)
		}
	}
	return out
}

func TestCommerceHandlerStatusFloorIsDeclared(t *testing.T) {
	routes, pkg, doc := commerceAudit(t)
	var diffs []statusaudit.Diff
	for _, r := range routes {
		op := doc.Paths.Value(r.Path).GetOperation(r.Method)
		diffs = append(diffs, statusaudit.Audit(op.OperationID, r.Method+" "+r.Path, op,
			emittedOn(r, pkg)))
	}
	if report := statusaudit.Report(diffs); report != "" {
		t.Fatalf("these commerce handlers can write a status their operation does not declare. "+
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
// deletes a DECLARATION, and an adapter whose route discovery silently returned nothing
// would survive all of them while reporting closure over the empty set.
func TestCommerceStatusDiscoveryCoversTheWholeDocument(t *testing.T) {
	routes, pkg, doc := commerceAudit(t)

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

// Every recorded-unreachable entry must still be DOING something. An exemption whose status
// the walker no longer derives — because the shared callee moved, or the guard changed, or
// the route was rewritten — is a silent hole: it would suppress that status for that route
// forever, including a future genuine emission, and nothing else would notice.
//
// This is the AGENTS.md inert-mechanism check applied to the exemption list rather than to
// production code: if deleting an entry changes nothing, the entry is not a documented
// limitation, it is decoration that reads like one.
func TestEveryRecordedUnreachableStatusIsStillDerived(t *testing.T) {
	routes, pkg, _ := commerceAudit(t)
	byRoute := map[string]statusaudit.Route{}
	for _, r := range routes {
		byRoute[r.Method+" "+r.Path] = r
	}
	for key, statuses := range unreachableOnRoute {
		r, ok := byRoute[key]
		if !ok {
			t.Fatalf("unreachableOnRoute names %q, which registerRoutes no longer mounts as a "+
				"documented route. Delete the entry — it can only hide a status now", key)
		}
		derived := map[int]bool{}
		for _, s := range pkg.Emitted(r.Handlers, auditConfig) {
			derived[s] = true
		}
		for _, s := range statuses {
			if !derived[s] {
				t.Fatalf("unreachableOnRoute[%q] excludes %d, but the walker no longer derives it "+
					"for that route. The entry is inert and must go: leaving it there suppresses "+
					"%d for this route forever, including a real one", key, s, s)
			}
		}
	}
}
