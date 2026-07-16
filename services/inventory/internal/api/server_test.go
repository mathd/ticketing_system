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

func TestOperationalEndpointsRequireInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	placeBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"purpose":"house","label":"foh","actor":"staff:amy","reason":"ops"}`
	mutBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"ops"}`
	requests := map[string]func() *http.Request{
		"place": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/internal/operational-holds", bytes.NewBufferString(placeBody))
		},
		"release": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/internal/operational-holds/"+id+"/release", bytes.NewBufferString(mutBody))
		},
		"convert": func() *http.Request {
			convBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","quantity":1,"ticket_type_id":"00000000-0000-0000-0000-000000000004","unit_amount":1000,"currency":"EUR","actor":"staff:amy","reason":"ops"}`
			return httptest.NewRequest(http.MethodPost, "/internal/operational-holds/"+id+"/convert", bytes.NewBufferString(convBody))
		},
		"history": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/internal/operational-holds/"+id+"/history?organizer_id="+id, nil)
		},
		"availability": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/internal/slots/"+id+"/availability?organizer_id="+id, nil)
		},
	}
	for name, make := range requests {
		for _, token := range []string{"", "wrong"} {
			r := make()
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "k")
			if token != "" {
				r.Header.Set("X-Internal-Token", token)
			}
			res := httptest.NewRecorder()
			s.Router().ServeHTTP(res, r)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("%s token %q: status=%d want=%d", name, token, res.Code, http.StatusUnauthorized)
			}
		}
	}
	// An empty configured credential must fail closed, not open.
	open := New(nil, "")
	req := httptest.NewRequest(http.MethodGet, "/internal/slots/"+id+"/availability?organizer_id="+id, nil)
	res := httptest.NewRecorder()
	open.Router().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("empty credential: status=%d want=%d", res.Code, http.StatusUnauthorized)
	}
}

func TestOperationalPlaceRejectsBadShapes(t *testing.T) {
	s := New(nil, "secret")
	for name, body := range map[string]string{
		"bad purpose":   `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"purpose":"vip","label":"foh","actor":"staff:amy","reason":"ops"}`,
		"blank label":   `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"purpose":"house","label":"","actor":"staff:amy","reason":"ops"}`,
		"zero quantity": `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":0,"purpose":"house","label":"foh","actor":"staff:amy","reason":"ops"}`,
		"no reason":     `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"purpose":"house","label":"foh","actor":"staff:amy"}`,
		"unknown field": `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"purpose":"house","label":"foh","actor":"staff:amy","reason":"ops","extra":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/operational-holds", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router().ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
}
