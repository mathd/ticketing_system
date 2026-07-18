//go:build smoke

package smoke_test

// TKT-47: every documented 2xx operation of the DB-backed services must be
// exercised through the running stack — real handler, real store, real
// response-validation middleware. Coverage is recorded at the two validation
// chokepoints (validateServiceResponse / validateDirectServiceResponse) and
// enforced after the run in TestMain; a new spec operation without a driving
// smoke test fails the suite. Catalog's coverage gate lives in its unit suite
// (services/catalog/internal/api), where a fake store exists.
//
// Scope, precisely: only traffic through the validating helpers (postJSON,
// getWithHeaders, get, internalJSON) reaches the chokepoints — raw helpers
// like holdRequest/postScan are invisible to the gate, which is why
// happy_path_coverage_test.go drives some heavily-exercised endpoints again.
// Coverage is per-operation, not per-documented-2xx-status (checkout counts
// covered via 200 even though 202 is also documented), and an operation
// documented only via `default:` has no explicit 2xx and is exempt.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

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

func uncovered2xxOps() []string {
	smokeCoverageMu.Lock()
	defer smokeCoverageMu.Unlock()
	var missing []string
	for _, service := range []string{"inventory", "commerce", "payments", "access"} {
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
