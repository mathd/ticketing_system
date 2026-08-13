package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apispec "ticketing/services/inventory/api"
)

// WHERE a reseller identity may enter inventory (TKT-246, ai-review pass 3 [critical]).
//
// The defect these pin: `reseller_id` was added to the PUBLIC `POST /holds` body. That
// route is not under /internal/, so the gateway proxies it and any caller on the
// internet reaches it — making an authorization input caller-supplied, which is exactly
// the defect TKT-240 was reverted for, one service further down.
//
// The store-tier forgery test could not catch it: it drives the store directly with no
// reseller, so it proves a caller cannot forge the COLUMN through a key string while the
// HTTP layer handed the column over on request. These tests live at the tier where the
// mechanism now is (AGENTS.md).

// The public hold contract has no reseller_id, so the field cannot be asked for.
//
// Asserted against the DOCUMENT rather than a handler, because that is where the guard
// is: `additionalProperties: false` on HoldCreate makes the field a 400 at the validator,
// before any handler runs. A handler that ignores the field would be one refactor from
// honouring it; a field that does not exist is not.
func TestThePublicHoldContractCannotNameAReseller(t *testing.T) {
	spec := string(apispec.Spec)

	start := strings.Index(spec, "    HoldCreate:")
	end := strings.Index(spec, "    InternalHoldCreate:")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate HoldCreate; this test has stopped looking at the right thing")
	}
	if public := spec[start:end]; strings.Contains(public, "reseller_id") {
		t.Fatal("HoldCreate declares reseller_id. POST /holds is NOT under /internal/, so the " +
			"gateway proxies it to the edge: any caller who knows a reseller's uuid could then " +
			"present it and consume that reseller's bound allocation. That is the TKT-240 " +
			"bypass, one service down.")
	}
	if !strings.Contains(spec[start:end], "additionalProperties: false") {
		t.Fatal("HoldCreate is not additionalProperties:false — an unknown reseller_id would be " +
			"accepted and silently ignored rather than refused, which is one refactor away from " +
			"being honoured")
	}

	// And the internal operation, which is where it belongs, does declare it.
	internal := spec[end:]
	if !strings.Contains(internal, "reseller_id") {
		t.Fatal("InternalHoldCreate does not declare reseller_id — the field has to live " +
			"somewhere, and nowhere is not an option")
	}
}

// The internal hold route is under /internal/, which is what makes the edge refuse it.
//
// The gateway 404s every /api/<svc>/internal/ prefix (ADR-002). This asserts inventory's
// side of that contract: if the operation were ever moved out from under the prefix, the
// gateway would start proxying a route that accepts an authorization input.
func TestTheResellerBearingHoldIsAnInternalOperation(t *testing.T) {
	spec := string(apispec.Spec)
	i := strings.Index(spec, "operationId: createInternalHold")
	if i < 0 {
		t.Fatal("createInternalHold is not in the contract")
	}
	// Walk back to the path key that owns this operation.
	path := strings.LastIndex(spec[:i], "\n  /")
	if path < 0 {
		t.Fatal("could not find the path for createInternalHold")
	}
	owner := spec[path+1 : path+1+strings.Index(spec[path+1:], ":")]
	if !strings.HasPrefix(strings.TrimSpace(owner), "/internal/") {
		t.Fatalf("createInternalHold is mounted at %q, which the gateway PROXIES. The reseller "+
			"identity it accepts would then be reachable from the internet, which is the whole "+
			"defect this route exists to avoid.", strings.TrimSpace(owner))
	}
}

// An internal hold without the service credential is refused, THROUGH THE ROUTER.
//
// The second of the two independent guards. The route table is one edit from exposing a
// prefix, so the handler must not rely on the gateway alone.
//
// Driven through `Router(...)` rather than by calling `internalOnly(createInternalHold)`
// directly (ai-review pass 4): the direct call INSTALLS the guard it claims to verify,
// so deleting internalOnly from the real registration left it green — while exposing a
// reseller-bearing handler to anyone who can reach inventory on the network. The
// registration is the thing under test, not the wrapper.
func TestTheInternalHoldRouteRequiresTheServiceCredential(t *testing.T) {
	h := New(nil, "the-real-token", nil).Router(nil, true)
	body := `{"organizer_id":"11111111-1111-1111-1111-111111111111",` +
		`"slot_id":"22222222-2222-2222-2222-222222222222","quantity":1,` +
		`"reseller_id":"33333333-3333-3333-3333-333333333333"}`

	for _, tc := range []struct{ name, token string }{
		{"no credential", ""},
		{"wrong credential", "not-the-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/holds", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "internal-hold-"+tc.name)
			if tc.token != "" {
				req.Header.Set("X-Internal-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("answered %d, want 401 — a route that accepts a reseller identity must "+
					"not run for an unauthenticated caller even if the gateway let it through. "+
					"body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// And the route EXISTS, so the 401s above are the credential guard refusing rather
	// than the router having no such path — a 404 would satisfy neither assertion but a
	// deleted route plus a catch-all could. Probed with a wrong METHOD, which the
	// validator answers 405 for a registered path without ever reaching the handler:
	// this server has a nil store, so a successful POST would panic in the handler
	// rather than tell us anything about routing.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/holds", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("GET /internal/holds answered 404 — the route is not registered at all, so the " +
			"401s above prove nothing about the credential guard")
	}
}
