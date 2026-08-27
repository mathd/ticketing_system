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
	// Cache-Control is part of what makes this fixture spec-valid, not decoration:
	// TKT-209 made listPublicVenues' declaration single-valued and `required: true`
	// (HoursCacheControl), so a response without it — or with any other tier — is a
	// contract violation and validateServiceResponse would Fatal here before the
	// coverage assertion below could run. The value must stay in step with
	// CacheControlPublicVenueReads; the enforcement that they agree lives in
	// catalog's own suite (TestPublicReadCacheTiersAreContractEnforced), which is
	// where the handler constant is reachable.
	header := http.Header{
		"Content-Type":  []string{"application/json"},
		"Cache-Control": []string{"public, max-age=3600, s-maxage=3600"},
	}
	body := []byte(`{"venues":[]}`)

	// Isolate the observation so this pin asserts regardless of suite order:
	// another catalog test may have already recorded this operation, which
	// would make a plain "is it present afterward" check pass vacuously if
	// catalog were ever dropped from the allowlist again. Snapshot the entry,
	// clear it, run the validator, then assert *this call* re-recorded it, and
	// restore the prior state so we don't perturb the coverage gate.
	// Restore the EXACT prior state, both ways — see saveRestoreSmokeCoverage. Setting it
	// back only when prior was true would leave this synthetic response's bit in the map
	// when it was false. That is inert today, because catalog is deliberately absent from
	// smokeCoverageGatedServices (ADR-030), so uncovered2xxOps never reads a catalog key —
	// but it would become gate credit the moment catalog joins that list, and a
	// stub-satisfied bit is indistinguishable from a genuine real-stack one. The same shape
	// is already live in TestConcurrentPostValidatesAndRecordsCoverage, where the operation
	// is inventory createHold and inventory IS gated (TKT-95).
	prior, restore := saveRestoreSmokeCoverage(op)
	defer restore()
	// Cleanup runs after this function's defers, so it can check the restore actually
	// happened. This is a backstop for THIS test's own bookkeeping, and it is deliberately
	// only a backstop: which of its two arms can fire depends on suite order (in an
	// unfiltered run backoffice_venue_list_test.go records the operation first, so prior is
	// true), so it cannot by itself prove the restore is correct in both directions.
	// TestSmokeCoverageSaveRestoreIsExact below is what proves that, against both prior
	// states explicitly, independent of order.
	t.Cleanup(func() {
		assertSmokeCoverageRestored(t, op, prior)
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

// assertSmokeCoverageRestored fails t unless smokeCoverage[op] is exactly `want`.
// Both directions matter: a bit left behind (present when it should be absent) would
// credit a synthetic response to the real-stack coverage gate, and a bit removed
// (absent when it should be present) would report a genuinely exercised operation as
// uncovered. A one-directional check cannot see the other regression.
func assertSmokeCoverageRestored(t *testing.T, op string, want bool) {
	t.Helper()
	smokeCoverageMu.Lock()
	got := smokeCoverage[op]
	smokeCoverageMu.Unlock()
	switch {
	case got && !want:
		t.Errorf("smokeCoverage[%q] leaked: a synthetic response must never satisfy the "+
			"real-stack coverage gate if catalog is later added to smokeCoverageGatedServices "+
			"(ADR-030)", op)
	case !got && want:
		t.Errorf("smokeCoverage[%q] was dropped: an operation recorded by a real-stack driver "+
			"must survive a save/restore, or the coverage gate would report a genuinely "+
			"exercised operation as uncovered", op)
	}
}

// saveRestoreSmokeCoverage is the save/restore that every test perturbing a shared
// smokeCoverage entry must use: it snapshots the entry, clears it, and returns the prior
// value plus a restore func that puts back the EXACT prior state — setting it when it was
// set, deleting it when it was not.
//
// It exists as a named function so TestSmokeCoverageSaveRestoreIsExact can drive it
// against both prior states directly. The bug this ticket fixes (TKT-140, and TKT-95
// before it in a gated service) was a hand-inlined restore that handled only the
// prior==true case, and the reason it survived review twice is that whether it is
// observable depends on which other tests ran first. A helper the gate can test against
// explicit inputs does not have that problem.
func saveRestoreSmokeCoverage(op string) (prior bool, restore func()) {
	smokeCoverageMu.Lock()
	prior = smokeCoverage[op]
	delete(smokeCoverage, op)
	smokeCoverageMu.Unlock()
	return prior, func() {
		smokeCoverageMu.Lock()
		if prior {
			smokeCoverage[op] = true
		} else {
			delete(smokeCoverage, op)
		}
		smokeCoverageMu.Unlock()
	}
}

// TestSmokeCoverageSaveRestoreIsExact is the order-independent proof that the save/restore
// used by the coverage-perturbing tests restores the exact prior state in BOTH directions.
//
// Why this exists as its own test rather than relying on the t.Cleanup guard inside
// TestValidateServiceResponseCoversCatalog: that guard's observability depends on suite
// order. In `make check` there is exactly one unfiltered smoke process
// (Makefile:17,122-123 -> scripts/smoke.sh's single `go test -tags smoke ./...`; the smoke
// module is excluded from test-go, Makefile:4-6), and in it an earlier driver has already
// recorded the catalog operation, so prior is true and the leak direction — the very
// regression TKT-140 fixes — cannot fire. Seeding both states here makes both directions
// gate-enforced regardless of what ran before (found by ai-review's second pass).
//
// It uses a synthetic operation key so it never perturbs a real one.
func TestSmokeCoverageSaveRestoreIsExact(t *testing.T) {
	const op = "catalog tkt140SaveRestoreProbe"

	for _, test := range []struct {
		name string
		seed bool
	}{
		{"absent before, must be absent after", false},
		{"present before, must be present after", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Establish the prior state explicitly instead of inheriting it.
			smokeCoverageMu.Lock()
			if test.seed {
				smokeCoverage[op] = true
			} else {
				delete(smokeCoverage, op)
			}
			smokeCoverageMu.Unlock()
			t.Cleanup(func() {
				smokeCoverageMu.Lock()
				delete(smokeCoverage, op)
				smokeCoverageMu.Unlock()
			})

			prior, restore := saveRestoreSmokeCoverage(op)
			if prior != test.seed {
				t.Fatalf("save read prior=%v, want %v", prior, test.seed)
			}
			// Between save and restore the entry must be cleared, so the caller can
			// attribute a later recording to its own call.
			smokeCoverageMu.Lock()
			cleared := !smokeCoverage[op]
			smokeCoverageMu.Unlock()
			if !cleared {
				t.Fatal("save did not clear the entry: the caller could not tell its own recording apart")
			}
			// Simulate the perturbation every caller performs: a synthetic response
			// records the operation.
			smokeCoverageMu.Lock()
			smokeCoverage[op] = true
			smokeCoverageMu.Unlock()

			restore()
			assertSmokeCoverageRestored(t, op, test.seed)
		})
	}
}
