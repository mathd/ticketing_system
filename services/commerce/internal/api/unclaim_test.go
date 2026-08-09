package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The un-claim's HTTP surface (TKT-225). The store's behaviour is proven against
// real PostgreSQL in unclaim_smoke_test.go; what is tested here is the guard, the
// refusals, and that neither reveals more than it should.
//
// Every request below is otherwise VALID — a real uuid and the required body —
// because the contract validator runs before the handler. A fixture the validator
// rejects is refused with 400 without the credential check ever running, which
// would pass just as happily against a handler with no guard at all. TKT-191
// needed three attempts to get exactly this right.

func unclaimRequest(t *testing.T, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	s := New(nil, http.DefaultClient, "", "", "", internalTok)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	return res
}

const validUnclaimBody = `{"actor":"staff:amy","reason":"claimed by the wrong account"}`

func TestUnclaimRefusesAWrongOrMissingInternalToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"no credential at all", nil},
		{"the wrong internal token", map[string]string{"X-Internal-Token": "not-the-token"}},
		{"an empty internal token", map[string]string{"X-Internal-Token": ""}},
		// The staff credential must NOT open this: it is a support action on
		// someone else's purchase and this slice ships no back-office surface.
		// staff_credential_test.go enumerates the same claim from the other side.
		{"the back office's staff credential", map[string]string{"X-Commerce-Staff-Write-Token": staffTok}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := unclaimRequest(t, "/internal/orders/"+someUUID+"/unclaim", validUnclaimBody, tc.headers)
			// 404, not 401: the service does not confirm the route exists.
			if res.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404. Body: %.200s", res.Code, res.Body.String())
			}
			// And it must not leak the reason it refused — "order is not
			// detachable" would confirm both the route and the credential.
			if strings.Contains(res.Body.String(), "detachable") {
				t.Fatalf("the credential refusal discloses the operation: %.200s", res.Body.String())
			}
		})
	}
}

func TestUnclaimRefusesABodyThatDescribesNothing(t *testing.T) {
	// The contract declares actor and reason required with minLength 1, so the
	// validator refuses these before the handler sees them. Asserted anyway: the
	// requirement is that a detach cannot be recorded without saying who and why,
	// and which layer enforces it is an implementation detail that may move.
	for _, tc := range []struct{ name, body string }{
		{"no actor", `{"reason":"wrong account"}`},
		{"no reason", `{"actor":"staff:amy"}`},
		{"empty actor", `{"actor":"","reason":"wrong account"}`},
		{"empty reason", `{"actor":"staff:amy","reason":""}`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := unclaimRequest(t, "/internal/orders/"+someUUID+"/unclaim", tc.body,
				map[string]string{"X-Internal-Token": internalTok})
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. Body: %.200s", res.Code, res.Body.String())
			}
		})
	}
}

func TestUnclaimRefusesAnIdThatIsNotAUUID(t *testing.T) {
	res := unclaimRequest(t, "/internal/orders/not-a-uuid/unclaim", validUnclaimBody,
		map[string]string{"X-Internal-Token": internalTok})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %.200s", res.Code, res.Body.String())
	}
}

// The complement to the refusal tests: with the right credential and a valid
// body, the request is NOT refused for want of a credential. It gets past the
// guard and fails on the nil database instead, which is what this fixture has.
//
// Without this, every test above would still pass against a handler that refuses
// everything unconditionally.
func TestUnclaimWithTheInternalTokenReachesTheHandler(t *testing.T) {
	defer func() {
		// A nil *sql.DB panics inside the store call. That panic IS the evidence:
		// the request passed the credential check, passed validation, parsed the
		// id, and reached the store.
		if recover() == nil {
			t.Fatal("expected the handler to reach the store with a nil database")
		}
	}()
	_ = unclaimRequest(t, "/internal/orders/"+someUUID+"/unclaim", validUnclaimBody,
		map[string]string{"X-Internal-Token": internalTok})
}
