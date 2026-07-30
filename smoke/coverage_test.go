//go:build smoke

package smoke_test

// TKT-47: every documented 2xx operation of the DB-backed services must be
// exercised through the running stack — real handler, real store, real
// response-validation middleware. Coverage is recorded at the two validation
// chokepoints (checkServiceResponse / checkDirectServiceResponse) and enforced
// after the run in TestMain; a new spec operation without a driving smoke test
// fails the suite. Catalog's coverage gate lives in its unit suite
// (services/catalog/internal/api), where a fake store exists — a deliberate
// exclusion, decided in ADR-030 (TKT-109) and pinned by
// TestCatalogCoverageGateIsDeliberatelyUnitScoped below.
//
// Scope, precisely (TKT-95 closed the raw-helper hole — every smoke HTTP helper
// that receives a service response now reaches a chokepoint, concurrent callers
// included, via the Async wrappers):
//   - Coverage is per-operation, not per-documented-2xx-status (checkout counts
//     covered via 200 even though 202 is also documented), and an operation
//     documented only via `default:` or a `2XX` wildcard has no explicit 2xx key
//     and is exempt.
//   - Catalog is outside uncovered2xxOps (ADR-030, above): catalog responses ARE
//     contract-validated wherever smoke drives them, but a new, undriven catalog
//     operation is caught by the unit gate, not here.
//   - Traffic with no service operation behind it is still raw, because there is
//     nothing to validate against: the gateway's refusals of routes absent from
//     every contract (TestGatewayDeniesGenericInternalRoutes — a contract lookup
//     there would fail by construction) and the storefront/scanner/health shell
//     checks.

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestCatalogCoverageGateIsDeliberatelyUnitScoped pins the ADR-030 / TKT-109
// decision: catalog's per-operation coverage gate lives in its unit suite
// (services/catalog/internal/api/coverage_test.go), NOT in uncovered2xxOps.
// The assertion is exact and ordered on purpose — smokeCoverageGatedServices
// is the single point of truth the gate iterates, so adding catalog (or any
// service) to the smoke gate must come through here, with ADR-030 amended
// first. If this test is failing, you either reordered the list (restore the
// order) or changed the gate's scope (update ADR-030 and this pin together).
// The second assertion guards one specific future refactor: uncovered2xxOps
// growing a catalog scan that bypasses smokeCoverageGatedServices. Today it
// is implied by the first assertion (the gate only iterates the pinned list);
// it exists so that class of change fails here, against ADR-030, instead of
// shipping silently.
func TestCatalogCoverageGateIsDeliberatelyUnitScoped(t *testing.T) {
	want := []string{"inventory", "commerce", "payments", "access"}
	if !slices.Equal(smokeCoverageGatedServices, want) {
		t.Fatalf("smokeCoverageGatedServices = %v, want %v (exact order): the smoke coverage gate's scope is pinned by ADR-030 — amend the ADR before changing it", smokeCoverageGatedServices, want)
	}
	for _, missing := range uncovered2xxOps() {
		if strings.HasPrefix(missing, "catalog ") {
			t.Fatalf("uncovered2xxOps reported %q: the smoke gate must not scan catalog (ADR-030 — amend the ADR before changing its scope)", missing)
		}
	}
}

var (
	smokeCoverageMu sync.Mutex
	smokeCoverage   = map[string]bool{} // "service operationId"
)

func recordSmokeCoverage(service, operationID string, status int) {
	if operationID == "" || status < 200 || status > 299 {
		return
	}
	smokeCoverageMu.Lock()
	smokeCoverage[service+" "+operationID] = true
	smokeCoverageMu.Unlock()
}

// coverageAllowlist names documented 2xx operations deliberately not driven by
// the smoke suite. Every entry carries its justification; additions should be
// rare and reviewed.
var coverageAllowlist = map[string]string{}

// smokeCoverageGatedServices is the single point of truth for which services'
// documented 2xx operations the smoke gate enforces. Catalog is deliberately
// absent (ADR-030): its per-operation gate lives in its unit suite, and smoke
// contract-validates catalog only on exercised routes.
var smokeCoverageGatedServices = []string{"inventory", "commerce", "payments", "access"}

func uncovered2xxOps() []string {
	smokeCoverageMu.Lock()
	defer smokeCoverageMu.Unlock()
	var missing []string
	for _, service := range smokeCoverageGatedServices {
		contract := loadContract(service)
		if contract.err != nil {
			missing = append(missing, fmt.Sprintf("%s: load contract: %v", service, contract.err))
			continue
		}
		for path, item := range contract.doc.Paths.Map() {
			for method, op := range item.Operations() {
				has2xx := false
				for status := range op.Responses.Map() {
					if strings.HasPrefix(status, "2") {
						has2xx = true
						break
					}
				}
				key := service + " " + op.OperationID
				if !has2xx || smokeCoverage[key] {
					continue
				}
				if _, allowed := coverageAllowlist[key]; allowed {
					continue
				}
				missing = append(missing, fmt.Sprintf("%s %s %s (%s)", service, method, path, op.OperationID))
			}
		}
	}
	sort.Strings(missing)
	return missing
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Enforce only on unfiltered runs: a focused `go test -run X` exercises
	// a few ops by design and must not fail the coverage gate.
	filtered := flag.Lookup("test.run") != nil && flag.Lookup("test.run").Value.String() != ""
	if code == 0 && !filtered {
		if missing := uncovered2xxOps(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "documented 2xx operations with no smoke happy-path coverage:\n  %s\n",
				strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}
