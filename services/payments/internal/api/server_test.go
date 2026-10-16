package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The charge handler binds a payment operation on the journal before doing anything else,
// so it cannot run against a nil journal — it needs a real database. This file has never
// had charge-handler unit tests (none were removed): the port refactor's behaviour (each
// token → normalized outcome, invalid-token rejection, Result validation) is unit-tested in
// internal/psp, and the end-to-end charge path (authorize→journal→complete, HTTP codes) is
// exercised by the compose-backed smoke suite. This file keeps the fact-endpoint
// strict-JSON tests, which need no journal.

func TestFactRejectsNonStrictJSON(t *testing.T) {
	server := newTestServer(nil, "secret")
	valid := `{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"00000000-0000-0000-0000-000000000002","type":"order.created"}`
	for name, body := range map[string]string{
		"unknown field":  valid[:len(valid)-1] + `,"unexpected":true}`,
		"trailing value": valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/internal/facts", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Internal-Token", "secret")
			recorder := httptest.NewRecorder()
			server.Router(nil, true).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
		})
	}
}

// TKT-285 (D9). Two independent guards refuse a zero-amount charge, and each needs its own
// test at its own tier. The contract validator runs BEFORE the handler on the routed
// endpoint, so a routed zero charge is answered by the validator's own message and the
// handler's `invalid charge` body is unreachable there. Calling the handler directly, with no
// contract middleware, is the only way to see the handler's guard.
//
// The journal is nil on purpose: the refusal must come before the operation lookup, the bind
// and the provider, so a handler that gets past it dereferences the nil journal. The recover
// turns that into a plain failure that names the cause instead of a stack trace.
func TestChargeHandlerRefusesAZeroAmountBeforeAnySideEffect(t *testing.T) {
	server := newTestServer(nil, "secret")
	body := `{"order_id":"00000000-0000-0000-0000-000000000001","organizer_id":"00000000-0000-0000-0000-000000000002",` +
		`"buyer_id":"00000000-0000-0000-0000-000000000003","amount":0,"currency":"EUR","payment_token":"fake-ok"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/charges", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Internal-Token", "secret")
	request.Header.Set("Idempotency-Key", "zero-amount")
	recorder := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("the handler went past the amount check and touched the journal: %v", recovered)
			}
		}()
		server.charge(recorder, request)
	}()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"error":"invalid charge"}` {
		t.Fatalf("body = %s, want the handler's own invalid charge refusal", got)
	}
}
