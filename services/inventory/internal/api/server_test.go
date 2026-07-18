package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ticketing/services/inventory/internal/store"
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
			s.Router(nil).ServeHTTP(res, req)
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
		s.Router(nil).ServeHTTP(res, req)
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
			convBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000005","quantity":1,"ticket_type_id":"00000000-0000-0000-0000-000000000004","unit_amount":1000,"currency":"EUR","actor":"staff:amy","reason":"ops"}`
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
			s.Router(nil).ServeHTTP(res, r)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("%s token %q: status=%d want=%d", name, token, res.Code, http.StatusUnauthorized)
			}
		}
	}
	// An empty configured credential must fail closed, not open.
	open := New(nil, "")
	req := httptest.NewRequest(http.MethodGet, "/internal/slots/"+id+"/availability?organizer_id="+id, nil)
	res := httptest.NewRecorder()
	open.Router(nil).ServeHTTP(res, req)
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
			s.Router(nil).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
}

func TestGroupReservationEndpointsRequireInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	placeBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003","quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops"}`
	drawBody := `{"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000005","quantity":1,"ticket_type_id":"00000000-0000-0000-0000-000000000004","unit_amount":1000,"currency":"EUR","actor":"staff:amy","reason":"ops"}`
	requests := map[string]func() *http.Request{
		"place": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/internal/group-reservations", bytes.NewBufferString(placeBody))
		},
		"draw-down": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/internal/group-reservations/"+id+"/draw-down", bytes.NewBufferString(drawBody))
		},
		"history": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/internal/group-reservations/"+id+"/history?organizer_id="+id, nil)
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
			s.Router(nil).ServeHTTP(res, r)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("%s token %q: status=%d want=%d", name, token, res.Code, http.StatusUnauthorized)
			}
		}
	}
}

func TestGroupReservationPlaceRejectsBadShapes(t *testing.T) {
	s := New(nil, "secret")
	base := `"organizer_id":"00000000-0000-0000-0000-000000000002","slot_id":"00000000-0000-0000-0000-000000000003"`
	for name, body := range map[string]string{
		"blank counterparty": `{` + base + `,"quantity":5,"counterparty":"","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops"}`,
		"no counterparty":    `{` + base + `,"quantity":5,"expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops"}`,
		"no expiry":          `{` + base + `,"quantity":5,"counterparty":"Acme","actor":"staff:amy","reason":"ops"}`,
		"bad expiry":         `{` + base + `,"quantity":5,"counterparty":"Acme","expires_at":"soon","actor":"staff:amy","reason":"ops"}`,
		"zero quantity":      `{` + base + `,"quantity":0,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops"}`,
		"no reason":          `{` + base + `,"quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy"}`,
		"empty channel":      `{` + base + `,"quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","channel":"","actor":"staff:amy","reason":"ops"}`,
		"overlong channel":   `{` + base + `,"quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","channel":"` + strings.Repeat("x", 101) + `","actor":"staff:amy","reason":"ops"}`,
		"unknown field":      `{` + base + `,"quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops","extra":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/group-reservations", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
	// A missing Idempotency-Key rejects before any store work.
	req := httptest.NewRequest(http.MethodPost, "/internal/group-reservations", bytes.NewBufferString(`{`+base+`,"quantity":5,"counterparty":"Acme","expires_at":"2027-01-01T00:00:00Z","actor":"staff:amy","reason":"ops"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "secret")
	res := httptest.NewRecorder()
	s.Router(nil).ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("missing key: status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
	}
}

func TestChannelAllocationEndpointRequiresInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	body := `{"organizer_id":"00000000-0000-0000-0000-000000000002","allocations":[{"channel":"presale","cap":10}]}`
	for _, token := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPut, "/internal/slots/"+id+"/channel-allocations", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Internal-Token", token)
		}
		res := httptest.NewRecorder()
		s.Router(nil).ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status=%d want=%d", token, res.Code, http.StatusUnauthorized)
		}
	}
}

func TestChannelAllocationRejectsBadShapes(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	org := `"organizer_id":"00000000-0000-0000-0000-000000000002"`
	for name, body := range map[string]string{
		"missing allocations": `{` + org + `}`,
		"empty channel":       `{` + org + `,"allocations":[{"channel":"","cap":10}]}`,
		"zero cap":            `{` + org + `,"allocations":[{"channel":"presale","cap":0}]}`,
		"duplicate channel":   `{` + org + `,"allocations":[{"channel":"presale","cap":1},{"channel":"presale","cap":2}]}`,
		"unknown field":       `{` + org + `,"allocations":[{"channel":"presale","cap":1,"bogus":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/internal/slots/"+id+"/channel-allocations", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
}

func TestCreateHoldRejectsBadChannel(t *testing.T) {
	s := New(nil, "")
	base := `"organizer_id":"00000000-0000-0000-0000-000000000001","slot_id":"00000000-0000-0000-0000-000000000002","quantity":1`
	for name, body := range map[string]string{
		"empty channel":    `{` + base + `,"channel":""}`,
		"overlong channel": `{` + base + `,"channel":"` + strings.Repeat("x", 101) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/holds", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "bad-channel")
			res := httptest.NewRecorder()
			s.Router(nil).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
}

// A dead slot must be distinguishable from contention: same 409, different code
// (TKT-75 AC2). Other conflicts keep the code-less shape they always had.
func TestOfferingStateProblemsAreDistinguishable(t *testing.T) {
	for _, tt := range []struct {
		err      error
		wantCode int
		wantBody string
	}{
		{store.ErrSlotArchived, 409, `{"code":"slot_archived","error":"slot archived"}`},
		{store.ErrSlotClosed, 409, `{"code":"slot_closed","error":"slot closed"}`},
		{store.ErrUnavailable, 409, `{"error":"insufficient capacity"}`},
		{store.ErrIdempotency, 409, `{"error":"idempotency key reused with different request"}`},
		{store.ErrNotFound, 404, `{"error":"not found"}`},
	} {
		res := httptest.NewRecorder()
		problem(res, tt.err)
		if res.Code != tt.wantCode {
			t.Fatalf("%v: status=%d want=%d", tt.err, res.Code, tt.wantCode)
		}
		if got := strings.TrimSpace(res.Body.String()); got != tt.wantBody {
			t.Fatalf("%v: body=%s want=%s", tt.err, got, tt.wantBody)
		}
	}
}

func TestCapacityAdjustmentEndpointsRequireInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	body := `{"organizer_id":"00000000-0000-0000-0000-000000000002","capacity":50,"actor":"staff:amy","reason":"reconfig"}`
	requests := map[string]func() *http.Request{
		"adjust": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/internal/slots/"+id+"/capacity-adjustments", bytes.NewBufferString(body))
		},
		"history": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/internal/slots/"+id+"/capacity-adjustments?organizer_id="+id, nil)
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
			s.Router(nil).ServeHTTP(res, r)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("%s token %q: status=%d want=%d", name, token, res.Code, http.StatusUnauthorized)
			}
		}
	}
}

func TestCapacityAdjustmentRejectsBadShapes(t *testing.T) {
	s := New(nil, "secret")
	id := "00000000-0000-0000-0000-000000000001"
	org := `"organizer_id":"00000000-0000-0000-0000-000000000002"`
	for name, body := range map[string]string{
		"zero capacity":     `{` + org + `,"capacity":0,"actor":"staff:amy","reason":"r"}`,
		"negative capacity": `{` + org + `,"capacity":-5,"actor":"staff:amy","reason":"r"}`,
		"missing actor":     `{` + org + `,"capacity":50,"reason":"r"}`,
		"missing reason":    `{` + org + `,"capacity":50,"actor":"staff:amy"}`,
		"unknown field":     `{` + org + `,"capacity":50,"actor":"staff:amy","reason":"r","extra":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/slots/"+id+"/capacity-adjustments", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
	// A missing Idempotency-Key rejects before any store work.
	req := httptest.NewRequest(http.MethodPost, "/internal/slots/"+id+"/capacity-adjustments", bytes.NewBufferString(`{`+org+`,"capacity":50,"actor":"staff:amy","reason":"r"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "secret")
	res := httptest.NewRecorder()
	s.Router(nil).ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("missing key: status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
	}
}
