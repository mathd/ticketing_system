package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apispec "ticketing/services/commerce/api"
)

// The partner surface's guard (TKT-240 / ADR-056).
//
// WHAT THESE TESTS CAN AND CANNOT PROVE. They drive the real chi router through
// the real OpenAPI validator, so they prove the CONTRACT half: that the
// declaration exists, that the validator enforces it before routing, and that a
// refusal is rendered as the contract says. They deliberately do NOT prove that a
// credential is scoped correctly — that is a SQL predicate, it is asserted in
// services/commerce/internal/store/reseller_credentials_smoke_test.go where the
// predicate lives, and an assertion here (against a nil database) would prove only
// that a fake and a handler agree.

// partnerServer builds a commerce server with NO database. Every credential
// lookup therefore fails, which is exactly the fixture these tests want: they are
// about what happens when authentication does not succeed.
func partnerServer() http.Handler {
	return New(nil, http.DefaultClient, "", "", "", "secret").Router(nil, true)
}

func partnerRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "partner-test")
	return req
}

// An unauthenticated partner call is refused BY THE VALIDATOR, before the handler
// runs.
//
// "Before the handler runs" is the claim that matters and it is the one a status
// code alone cannot support: a handler that checked the header itself would also
// answer 401. So the fixture registers no database at all — a handler that ran
// would have to reach a nil *sql.DB to do anything, and the assertion is that the
// answer is the contract's declared 401 rather than a panic or a 500.
func TestPartnerRequestWithoutACredentialIsRefusedByTheValidator(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body string }{
		{"availability", http.MethodGet, "/partners/availability?slot_id=00000000-0000-0000-0000-000000000009", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			partnerServer().ServeHTTP(res, partnerRequest(tc.method, tc.path, tc.body))
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — an operation declaring security: must be refused "+
					"by the validator when no credential is presented (body %s)", res.Code, res.Body)
			}
			if !strings.Contains(res.Body.String(), "partner credential is not recognised") {
				t.Fatalf("body = %s, want the uniform partner refusal", res.Body)
			}
		})
	}
}

// A presented-but-unknown credential is refused identically to an absent one.
//
// The two must be indistinguishable: a partner integration that can tell "this
// token is wrong" from "you sent no token" learns nothing useful, but one that can
// tell "unknown" from "revoked" can probe other partners' credentials.
func TestAnUnknownCredentialIsIndistinguishableFromNoCredential(t *testing.T) {
	absent := httptest.NewRecorder()
	partnerServer().ServeHTTP(absent, partnerRequest(http.MethodGet,
		"/partners/availability?slot_id=00000000-0000-0000-0000-000000000009", ""))

	presented := partnerRequest(http.MethodGet, "/partners/availability?slot_id=00000000-0000-0000-0000-000000000009", "")
	presented.Header.Set(partnerCredentialHeader, "0123456789abcdef0123456789abcdef")
	unknown := httptest.NewRecorder()
	partnerServer().ServeHTTP(unknown, presented)

	if absent.Code != unknown.Code || absent.Body.String() != unknown.Body.String() {
		t.Fatalf("an unknown credential is distinguishable from an absent one:\n absent  = %d %s\n unknown = %d %s",
			absent.Code, absent.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// The scheme the code reads must be the scheme the contract declares.
//
// Modelled on catalog's TestCatalogStaffWriteSchemeIsADeclaredHeaderKey, and for
// the same reason: a header renamed in the document and not in the code would make
// the server read a header nobody sends, and every request would fail closed —
// loudly for a partner, invisibly in review, because both halves look right on
// their own.
func TestPartnerSchemeIsADeclaredHeaderKey(t *testing.T) {
	spec := string(apispec.Spec)
	if !strings.Contains(spec, partnerCredentialScheme+":") {
		t.Fatalf("the contract declares no securityScheme named %q", partnerCredentialScheme)
	}
	if !strings.Contains(spec, "name: "+partnerCredentialHeader) {
		t.Fatalf("the contract does not declare %q as the scheme's header name; the server would "+
			"read a header nobody sends", partnerCredentialHeader)
	}
	if !strings.Contains(spec, "type: apiKey") || !strings.Contains(spec, "in: header") {
		t.Fatal("the partner scheme is not declared as an apiKey in a header")
	}
}

// Every partner operation declares the security requirement.
//
// Derived from the document rather than from a hand-kept list: an operation added
// under /partners/ without a `security:` line is precisely the failure this test
// exists for, and a list would have to be updated by the same person who forgot
// the declaration.
func TestEveryPartnerOperationDeclaresTheCredential(t *testing.T) {
	spec := string(apispec.Spec)
	start := strings.Index(spec, "  /partners/availability:")
	end := strings.Index(spec, "  /internal/operational-holds/")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate the partner path block in the contract")
	}
	block := spec[start:end]
	ops := strings.Count(block, "operationId:")
	declarations := strings.Count(block, "security: [{PartnerCredential: []}]")
	if ops == 0 {
		t.Fatal("found no partner operations; this test has stopped looking at the right thing")
	}
	if declarations != ops {
		t.Fatalf("%d partner operations but %d security declarations: an operation on this surface "+
			"without one is reachable from the edge unauthenticated", ops, declarations)
	}
}

// A wrong METHOD on a pre-existing operation still answers 405.
//
// This test exists because its predecessor could not fail. That one sent a
// malformed POST and asserted the body shape, while its comment claimed to pin the
// 405-vs-404 behaviour -- so the regression it named shipped underneath it and was
// caught by ai-review instead. Confirmed by running both revisions: origin/main
// answered 405, the branch answered 404.
//
// The mechanism is that supplying any error handler switches the shared validator
// to ErrorHandlerWithOpts, which hard-codes 404 for every route-lookup failure.
// Deleting the restoration in server.go must turn this red.
func TestWrongMethodStillAnswers405(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"GET on a POST-only operation", http.MethodGet, "/reservations"},
		{"DELETE on a POST-only operation", http.MethodDelete, "/orders"},
		{"POST on a GET-only operation", http.MethodPost, "/orders/00000000-0000-0000-0000-000000000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			partnerServer().ServeHTTP(res, httptest.NewRequest(tc.method, tc.path, nil))
			if res.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 405. A wrong method reported as 404 is the platform-wide "+
					"regression that adding a validator error handler introduces (body %s)",
					tc.method, tc.path, res.Code, res.Body)
			}
		})
	}
}

// An UNKNOWN path still answers 404 -- the restoration above must not turn every
// miss into a 405.
func TestAnUnknownPathStillAnswers404(t *testing.T) {
	res := httptest.NewRecorder()
	partnerServer().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/no-such-operation", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("an unknown path = %d, want 404: the 405 restoration is too broad and is now "+
			"answering 405 for paths that do not exist (body %s)", res.Code, res.Body)
	}
}

// Validation errors OUTSIDE the partner surface keep the representation they had
// before this ticket.
//
// This ticket had to supply an error handler to give the partner 401 its own body,
// and supplying ANY handler switches the shared helper from its legacy hook to
// ErrorHandlerWithOpts. That is not cosmetic: the newer hook reports every
// route-lookup failure as 404 where the legacy one distinguishes a wrong method as
// 405. This test pins the customer surface's shape so that change cannot ride
// along unnoticed.
func TestNonPartnerValidationErrorsAreUnchanged(t *testing.T) {
	res := httptest.NewRecorder()
	partnerServer().ServeHTTP(res, partnerRequest(http.MethodPost, "/reservations", `{"organizer_id":"not-a-uuid"}`))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("a malformed customer reservation = %d, want 400 (body %s)", res.Code, res.Body)
	}
	body := res.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("the non-partner validation error lost its {\"error\": …} shape: %s", body)
	}
	if strings.Contains(body, "partner credential") {
		t.Fatalf("a customer-surface error was rendered as a partner refusal: %s", body)
	}
}
