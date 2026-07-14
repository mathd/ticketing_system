package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateHoldRejectsNonStrictJSON(t *testing.T) {
	s := New(nil, "")
	valid := `{"organizer_id":"00000000-0000-0000-0000-000000000001","slot_id":"00000000-0000-0000-0000-000000000002","quantity":1,"unit_amount":100}`
	for name, body := range map[string]string{
		"unknown field":  valid[:len(valid)-1] + `,"unexpected":true}`,
		"trailing value": valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/holds", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "strict-json")
			res := httptest.NewRecorder()
			s.Router().ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d", res.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestTransitionsRequireInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	for _, token := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/holds/00000000-0000-0000-0000-000000000001/finalize?organizer_id=00000000-0000-0000-0000-000000000002", nil)
		req.Header.Set("X-Internal-Token", token)
		res := httptest.NewRecorder()
		s.Router().ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status=%d want=%d", token, res.Code, http.StatusUnauthorized)
		}
	}
}
