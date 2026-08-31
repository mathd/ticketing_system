package api

// TKT-278. TKT-256 established for PAYMENTS that every operation declares a 500-class
// response, and its own AC asked whether the other four services have the same shape. This
// is inventory's answer, and it is a different invariant from payments' on purpose.
//
// WHY NOT A PORT OF THE PAYMENTS TEST. That one asks "does this operation declare 500?" —
// a question about the document, answerable from the document. Run against these four
// services it finds three things and all three are correct: catalog's static
// `GET /openapi.yaml`, and commerce's `getOrder`/`getDeliveryEmail`, which declare 503
// because `persistenceReadProblem` returns exactly {404,503}. A check that measures the
// spec against itself passes forever. The ticket says so in as many words: the invariant to
// build is "every status a handler CAN WRITE is declared", not "every operation declares
// 500".
//
// WHAT THIS FILE ACTUALLY DELIVERS IS A SOUND SUBSET OF THAT, and the gap is stated in full
// under THE BLIND SPOTS below. The test is named for the FLOOR rather than for the ticket's
// wording precisely so a green run is not read as the stronger claim: a helper's conditional
// arms are out of scope, so deleting a declaration only that arm reaches — inventory's 404,
// say — stays green. That is a limitation, not a defect, and the remedy is NOT to widen the
// floors (measured below: it makes the audit useless) but to reach for the sharper
// per-status instrument where one exists, as catalog's lifecycle_notfound_test.go is for 404.
//
// So discovery runs over the Go SOURCE and the document is only ever CHECKED, never used to
// derive the expected set — the same discipline, and much of the same machinery, as
// catalog's 404 invariant (lifecycle_notfound_test.go, TKT-178), whose comment states the
// rule this file inherits: deriving the set from the document would make the test restate
// the spec back to itself.
//
// WHAT ADR-028 MAKES OF A MISS. The router wraps its handlers in response validation with
// `IncludeResponseStatus: true`, so an undeclared status is failed closed: the caller gets a
// generic 500 carrying "response violates OpenAPI contract" instead of the status the
// handler meant, and the real cause is obscured exactly while someone debugs an outage.
//
// ────────────────────────────────────────────────────────────────────────────────────────
// THE BLIND SPOTS, stated rather than implied — AC-1 asks for the method AND its limits.
//
// This is a SOUND SUBSET, not a complete derivation. It asserts two things:
//
//	1. Every status written as a LITERAL in the handler's own body, or in a wrapper the
//	   route composes (`internalOnly` writes 401), or in a helper the handler calls whose
//	   status is unconditional.
//	2. The UNCONDITIONAL FLOOR of each error helper — the statuses it emits for ANY input.
//	   `problem()`'s floor is {500}: its default arm takes every unmapped error.
//
// It does NOT assert a helper's CONDITIONAL arms. `problem()` can also answer 400, 404 and
// 409, but only for particular store sentinels, and which sentinels a given store function
// can return is not derivable from the call site. Two reasons that arm is out of scope, and
// both were measured rather than assumed:
//
//   - Over-approximating — treating every `problem()` caller as emitting {400,404,409,500} —
//     produces SEVEN false positives in this service alone (getCacheControl,
//     getCapacityAdjustments, getGroupReservationHistory, getHoldSeating,
//     getOperationalHoldHistory, getStaffAvailability, putCacheControl all lack 409 and all
//     are correct). A gate that fires on something it does not guard is worse than the gap.
//   - Modelling `problem()`'s branches precisely would put a second copy of it in this file,
//     including the ORDERING that TKT-307 made load-bearing (`belowConsumption` matches a
//     structural interface and claims every error carrying `Channel() string`). Then every
//     failure is a fact about whether the copy matches the original.
//
// The residual: a handler whose store function grows a sentinel that `problem()` maps to a
// status the operation does not declare is invisible here. `getHoldSeating` is the worked
// example and is hand-verified — `ClaimIsSeated` returns only `ErrNotFound` or the driver's
// error, so its reachable set is exactly {404,500} and both are declared (seating.go says so
// in the source, since TKT-305 fixed the collapse this ticket was filed to catch). That
// verification is a human reading the store function; this test cannot do it.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/inventory/api"
	"ticketing/shared/contract/statusaudit"
)

// auditConfig is inventory's half of the audit: how it writes responses, and what each of
// its helpers guarantees. The walker is shared; these judgements are local, because what a
// helper emits FOR ANY INPUT is a fact about that helper.
var auditConfig = statusaudit.Config{
	WriteFuncs: []string{"write"},
	StatusArg:  1,
	Floors: map[string][]int{
		// problem() ends in a default arm answering 500 for any error none of its cases
		// match, and on this service's public routes that is a wrapped pgx error. Every
		// caller can reach 500 whatever its store returns. Its 400/404/409 arms are
		// CONDITIONAL on particular sentinels and are out of scope — see the blind spots.
		"problem": {http.StatusInternalServerError},
		// The credential wrappers. Each composes a status onto every route it wraps, and it
		// is unconditional: a request without the credential is refused before the handler.
		"internalOnly":    {http.StatusUnauthorized},
		"staffOrInternal": {http.StatusUnauthorized},
		// Refuses a missing or oversized key before the handler proceeds.
		"idempotencyKey": {http.StatusBadRequest},
	},
}

func inventoryAudit(t *testing.T) ([]statusaudit.Route, *statusaudit.Package, *openapi3.T) {
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

func TestInventoryHandlerStatusFloorIsDeclared(t *testing.T) {
	routes, pkg, doc := inventoryAudit(t)
	var diffs []statusaudit.Diff
	for _, r := range routes {
		op := doc.Paths.Value(r.Path).GetOperation(r.Method)
		diffs = append(diffs, statusaudit.Audit(op.OperationID, r.Method+" "+r.Path, op,
			pkg.Emitted(r.Handlers, auditConfig)))
	}
	if report := statusaudit.Report(diffs); report != "" {
		t.Fatalf("these inventory handlers can write a status their operation does not declare. "+
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

// The discovery half, asserted separately (plan-review A2). Every mutation this file's
// evidence rests on deletes a DECLARATION; an adapter whose route discovery silently
// returned nothing would survive all of them and report closure over the empty set.
func TestInventoryStatusDiscoveryCoversTheWholeDocument(t *testing.T) {
	routes, pkg, doc := inventoryAudit(t)

	if len(pkg.Funcs) == 0 {
		t.Fatal("parsed no function declarations from the api package; the scan is empty and " +
			"every audit above it would report closure over nothing")
	}

	// EXACT coverage against the document, not a floor: a threshold would let a partial
	// parse quietly shrink the denominator, which is the failure mode the audit cannot see
	// from inside.
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
			"do not cover. The registration shape has probably changed — fix the parsing rather "+
			"than the threshold, because a partial mapping shrinks what \"every operation\" means "+
			"and the audit would report closure over a subset:\n  %s",
			len(routes), len(unmapped), strings.Join(unmapped, "\n  "))
	}

	// And every route must yield at least one status: an empty set satisfies the audit
	// vacuously, so it is a scan failure rather than a clean result.
	for _, r := range routes {
		if got := pkg.Emitted(r.Handlers, auditConfig); len(got) == 0 {
			t.Fatalf("%s %s: derived no statuses at all from handlers %v", r.Method, r.Path, r.Handlers)
		}
	}
}
