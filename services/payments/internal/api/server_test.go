package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The charge handler binds a payment operation on the journal before doing anything else,
// so it cannot run against a nil journal — it needs a real database. The port refactor's
// behaviour (each token → normalized outcome, unknown-token rejection) is unit-tested in
// internal/psp; the end-to-end charge path (authorize→journal→complete, HTTP codes) is
// exercised by the compose-backed smoke suite, as it was before this refactor. This file
// keeps the fact-endpoint strict-JSON tests, which need no journal.

func TestFactRejectsNonStrictJSON(t *testing.T) {
	server := New(nil, "secret")
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
			server.Router(nil).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
		})
	}
}
