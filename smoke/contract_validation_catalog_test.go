//go:build smoke

package smoke_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidateServiceResponseCoversCatalog pins the fix for TKT-106: the
// response contract-validator (validateServiceResponse) allowlisted only
// " inventory commerce payments access " and returned early for every
// /api/catalog/* response, so catalog reads got no end-to-end contract
// validation in smoke despite the assertions that appeared to exercise it.
//
// This is a stack-free unit-level pin — it calls validateServiceResponse
// directly with a spec-valid catalog response and asserts the coverage
// chokepoint actually recorded the catalog operation. It fails RED while
// catalog is absent from the allowlist (early return -> no recordSmokeCoverage)
// and GREEN once catalog is in it. It also permanently guards the allowlist
// against a future regression that silently drops catalog again.
func TestValidateServiceResponseCoversCatalog(t *testing.T) {
	const op = "catalog listPublicVenues"

	// A response that conforms to the committed catalog contract for
	// GET /public/venues: 200 with a PublicVenueList body (an empty venues
	// array satisfies the schema — only the wrapper object is required).
	request := httptest.NewRequest(http.MethodGet,
		"http://gateway/api/catalog/public/venues?organizer_id=00000000-0000-0000-0000-000000000001", nil)
	header := http.Header{"Content-Type": []string{"application/json"}}
	body := []byte(`{"venues":[]}`)

	// Isolate the observation so this pin asserts regardless of suite order:
	// another catalog test may have already recorded this operation, which
	// would make a plain "is it present afterward" check pass vacuously if
	// catalog were ever dropped from the allowlist again. Snapshot the entry,
	// clear it, run the validator, then assert *this call* re-recorded it, and
	// restore the prior state so we don't perturb the coverage gate.
	smokeCoverageMu.Lock()
	prior := smokeCoverage[op]
	delete(smokeCoverage, op)
	smokeCoverageMu.Unlock()
	defer func() {
		if prior {
			smokeCoverageMu.Lock()
			smokeCoverage[op] = true
			smokeCoverageMu.Unlock()
		}
	}()

	// A validateServiceResponse that reaches the contract also Fatals if the
	// body drifts, so a spec-valid fixture keeps a green run green.
	validateServiceResponse(t, request, http.StatusOK, header, body)

	smokeCoverageMu.Lock()
	recorded := smokeCoverage[op]
	smokeCoverageMu.Unlock()
	if !recorded {
		t.Fatalf("validateServiceResponse did not contract-validate catalog: %q not recorded by this call. "+
			"catalog is likely missing from the allowlist in contract_validation_test.go (TKT-106).", op)
	}
}
