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
//
// Scope of the guarantee: the allowlist only validates catalog responses that
// actually reach validateServiceResponse via a smoke request. Catalog is
// deliberately absent from the uncovered2xxOps coverage gate (coverage_test.go),
// so a *new, unexercised* catalog operation can still be added without smoke
// contract-validating it — catalog's per-operation coverage lives in its unit
// suite by design. That exclusion is a recorded decision (ADR-030, TKT-109),
// pinned by TestCatalogCoverageGateIsDeliberatelyUnitScoped in coverage_test.go.
//
// This test relies on smoke's top-level tests running sequentially (none call
// t.Parallel()): it deletes and restores a shared smokeCoverage entry, which
// would race a concurrent catalog test if that invariant ever changed.
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
	// Restore the EXACT prior state, both ways. Setting it back only when prior was true
	// would leave this synthetic response's bit in the map when it was false. That is
	// inert today — catalog is deliberately absent from smokeCoverageGatedServices
	// (ADR-030), so uncovered2xxOps never reads a catalog key — but it would become gate
	// credit the moment catalog joins that list, and a stub-satisfied bit is
	// indistinguishable from a genuine real-stack one. Mirrors
	// TestConcurrentPostValidatesAndRecordsCoverage, where the same shape is live because
	// inventory IS gated (TKT-95).
	defer func() {
		smokeCoverageMu.Lock()
		if prior {
			smokeCoverage[op] = true
		} else {
			delete(smokeCoverage, op)
		}
		smokeCoverageMu.Unlock()
	}()
	// Cleanup runs after this function's defers, so it can check the restore actually
	// happened. This is the regression guard for the leak itself: an "if prior" restore
	// that skips the false case leaves this synthetic response's bit in the map, and it
	// would be credited as gate coverage the moment catalog joins
	// smokeCoverageGatedServices.
	// It asserts BOTH directions — leaked (present when it should be absent) and
	// over-deleted (absent when it should be present) — because "restores the exact prior
	// state" is a two-way claim and a guard that only checks one of them cannot see the
	// other regression.
	//
	// Which direction is *observable* depends on suite order, so be precise about it:
	//   - Focused run (`-run TestValidateServiceResponseCoversCatalog`): the coverage map
	//     starts empty, so prior is false and the leak arm is live — this is the arm that
	//     catches a revert to the one-way "if prior" restore.
	//   - Unfiltered run: backoffice_venue_list_test.go exercises
	//     GET /api/catalog/public/venues and sorts earlier (file order), so prior is
	//     already true and the over-deletion arm is the live one instead.
	// Every run therefore exercises exactly one arm, and between the focused and full runs
	// in `make check` both arms are covered.
	//
	// Note this differs from the reference guard in
	// TestConcurrentPostValidatesAndRecordsCoverage: its operation is inventory createHold,
	// whose real drivers live in group_reservations_test.go, happy_path_coverage_test.go and
	// inventory_contention_test.go — all of which sort AFTER contract_validation_test.go —
	// so its prior is false and its leak arm is live even in a full run. Do not assume the
	// two tests have the same observability; catalog's earlier driver is what shifts which
	// arm fires here (found by ai-review).
	t.Cleanup(func() {
		smokeCoverageMu.Lock()
		got := smokeCoverage[op]
		smokeCoverageMu.Unlock()
		switch {
		case got && !prior:
			t.Errorf("this test leaked %q into smokeCoverage: a synthetic response must never "+
				"satisfy the real-stack coverage gate if catalog is later added to "+
				"smokeCoverageGatedServices (ADR-030)", op)
		case !got && prior:
			t.Errorf("this test dropped %q from smokeCoverage: it was recorded by an earlier "+
				"real-stack driver and must survive this test's save/restore, or a later "+
				"coverage gate would report a genuinely exercised operation as uncovered", op)
		}
	})

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
