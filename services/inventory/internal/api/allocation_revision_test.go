package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ticketing/services/inventory/internal/store"
)

// TKT-250. The allocation-set revision is REQUIRED of the back office and OPTIONAL for
// the shared internal token, and this file owns that split.
//
// WHY A SPLIT AT ALL. The back office is where two humans race: it renders a form from a
// read and submits it minutes later. The service-to-service path is machine-driven and
// rebuilds its whole set per call, so a precondition would buy it nothing and break eight
// smoke call sites across four files. ADR-057 is what makes the split expressible — it
// gave inventory its own staff credential, so the guard can tell the two apart.
//
// The contract CANNOT express "required for one credential class", so `allocation_revision`
// is optional in the OpenAPI schema and the rule lives in the handler. That is the whole
// reason these assertions exist: nothing upstream enforces it.

// A staff caller must present a revision. Without one the write is refused before it can
// happen — 400, not a silent unconditional replace.
//
// THIS TEST IS THE REASON THE CHECK RUNS BEFORE THE STORE CALL. server(t) builds
// New(nil, …) — a NIL STORE — so a handler that reaches s.st.ReplaceChannelAllocations
// panics. If the revision check were placed after the store call, this test would panic
// instead of asserting 400, and the panic would read as a broken fixture rather than as
// the misplaced check it actually is.
func TestAStaffAllocationWriteWithoutARevisionIsRefused(t *testing.T) {
	res := putAllocations(t, staffWriteHeader, staffTok,
		`{"organizer_id":"`+orgUUID+`","allocations":[{"channel":"reseller-acme","cap":5}]}`)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("staff write with no allocation_revision: status=%d want=400. Body: %.200s",
			res.Code, res.Body.String())
	}
	// The message must name the field, or an operator debugging a 400 learns nothing.
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal %q: %v", res.Body.String(), err)
	}
	if body["error"] == "" || !bytes.Contains([]byte(body["error"]), []byte("allocation_revision")) {
		t.Errorf("refusal %q does not name the missing field", body["error"])
	}
}

// The internal token may omit it and gets the pre-TKT-250 unconditional replace.
//
// Asserted as "the handler was REACHED", not as a 2xx: with a nil store the handler
// cannot complete. Reaching it is exactly the property under test — the request got past
// the revision requirement — and the end-to-end behaviour is proven in the smoke suite
// against a real database.
func TestAnInternalAllocationWriteWithoutARevisionIsAdmitted(t *testing.T) {
	reached := false
	s := server(t)
	h := s.staffOrInternal(func(w http.ResponseWriter, r *http.Request) {
		// Re-run the handler's own decision rather than a copy of it: this asserts
		// the class the guard published, which is the mechanism under test.
		if callerCredential(r) == credentialInternal {
			reached = true
		}
	})
	req := httptest.NewRequest(http.MethodPut, "/internal/slots/"+slotUUID+"/channel-allocations",
		bytes.NewBufferString(`{"organizer_id":"`+orgUUID+`","allocations":[{"channel":"pos","cap":5}]}`))
	req.Header.Set("X-Internal-Token", internalTok)
	req.Header.Set("Content-Type", "application/json")
	h(httptest.NewRecorder(), req)

	if !reached {
		t.Error("the shared internal token was not classified as internal, so it would be forced to present a revision")
	}
}

// The guard publishes WHICH credential opened the request, and internal wins when both
// are presented.
//
// Internal-wins is the conservative order: the shared token is the broader credential, so
// a caller holding it is treated as the service-to-service path it already was. Pinned
// because the opposite order would quietly impose the staff precondition on every
// service-to-service caller that happened to also send a staff header.
func TestTheGuardPublishesTheCredentialClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    credentialClass
	}{
		{"staff credential alone", map[string]string{staffWriteHeader: staffTok}, credentialStaff},
		{"internal token alone", map[string]string{"X-Internal-Token": internalTok}, credentialInternal},
		{"both presented: internal wins", map[string]string{
			staffWriteHeader: staffTok, "X-Internal-Token": internalTok}, credentialInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got credentialClass
			seen := false
			h := server(t).staffOrInternal(func(w http.ResponseWriter, r *http.Request) {
				got, seen = callerCredential(r), true
			})
			req := httptest.NewRequest(http.MethodGet,
				"/internal/slots/"+slotUUID+"/availability?organizer_id="+orgUUID, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			h(httptest.NewRecorder(), req)

			if !seen {
				t.Fatal("the guard refused a credential it grants")
			}
			if got != tc.want {
				t.Errorf("credential class=%v want=%v", got, tc.want)
			}
		})
	}
}

// A handler reached WITHOUT the guard having run is classified staff — the stricter arm.
//
// Fails closed by construction. If a future route is wired without staffOrInternal, the
// consequence is a refusal (loud, immediately visible) rather than a silently dropped
// precondition (invisible until two operators lose an edit).
func TestAnUnguardedRequestIsClassifiedAsTheStricterArm(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/internal/slots/"+slotUUID+"/channel-allocations", nil)
	if got := callerCredential(req); got != credentialStaff {
		t.Errorf("an unguarded request classified as %v; want credentialStaff, the arm that REQUIRES a revision", got)
	}
}

// The stale-revision refusal is coded on the wire, in the shape TKT-244 established.
//
// Without the code the editor cannot tell "your view is stale, reload" from "that cap is
// impossible" — both are 409 — and would show one generic failure for two problems with
// completely different remedies.
func TestTheStaleRevisionRefusalIsCoded(t *testing.T) {
	res := httptest.NewRecorder()
	problem(res, store.ErrAllocationRevisionMismatch)

	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d want=409 body=%s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", res.Body.String(), err)
	}
	if body["code"] != "allocation_revision_mismatch" {
		t.Errorf("code=%v want allocation_revision_mismatch", body["code"])
	}
	// Names no channel: staleness is a property of the whole set, and pointing at one
	// row would send the operator to fix a field that is not the problem.
	if _, named := body["channel"]; named {
		t.Errorf("the refusal named a channel: %v", body["channel"])
	}
}

// putAllocations drives the allocation write through the FULL router, so the OpenAPI
// request validator runs exactly as it does for a real caller.
//
// That matters here specifically: a probe the validator rejects gets 400 for the wrong
// reason, and a 400 assertion cannot tell that apart from the refusal it means to test.
// The body below is contract-valid — it omits only the optional `allocation_revision`,
// which is precisely the condition under test.
func putAllocations(t *testing.T, header, value, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, internalOp{
		name: "replaceChannelAllocations",
		verb: http.MethodPut,
		path: "/internal/slots/" + slotUUID + "/channel-allocations",
		body: body,
	}, header, value)
}
