package api

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ticketing/shared/fakepsp"
)

func TestPaymentFailureResponse(t *testing.T) {
	tests := []struct {
		name, body, fallback, wantStatus string
		wantReplay                       bool
	}{
		{name: "valid object", body: `{"status":"declined","replay":true,"reason":"card_declined","payment_id":"provider-secret"}`, fallback: "fallback", wantStatus: "declined", wantReplay: true},
		{name: "empty", body: ``, fallback: "timeout", wantStatus: "timeout"},
		{name: "malformed", body: `{`, fallback: "declined", wantStatus: "declined"},
		{name: "non object", body: `[]`, fallback: "timeout", wantStatus: "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paymentFailureResponse([]byte(tt.body), tt.fallback)
			if got["status"] != tt.wantStatus {
				t.Fatalf("status = %v, want %s", got["status"], tt.wantStatus)
			}
			if replay, ok := got["replay"]; ok != tt.wantReplay || (ok && replay != true) {
				t.Fatalf("replay = %v, present = %t, want replay = %t", replay, ok, tt.wantReplay)
			}
			if _, ok := got["reason"]; ok {
				t.Fatal("reason must not be exposed")
			}
			if _, ok := got["payment_id"]; ok {
				t.Fatal("payment_id must not be exposed")
			}
		})
	}
}

func TestReserveRejectsUnknownFields(t *testing.T) {
	s := New(nil, http.DefaultClient, "", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(`{"organizer_id":"00000000-0000-0000-0000-000000000001","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"amount":1}`))
	req.Header.Set("Idempotency-Key", "strict-json")
	res := httptest.NewRecorder()
	s.Router().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d", res.Code, http.StatusBadRequest)
	}
}

func TestCheckoutClaimProblemDoesNotLeakDetails(t *testing.T) {
	code, message := checkoutClaimProblem(errCheckoutConflict)
	if code != http.StatusConflict || message != "checkout conflicts with an existing request" {
		t.Fatalf("conflict mapping = %d %q", code, message)
	}
	code, message = checkoutClaimProblem(errors.New("duplicate key value violates unique constraint orders_pkey"))
	if code != http.StatusInternalServerError || message != "persist checkout" {
		t.Fatalf("unexpected mapping = %d %q", code, message)
	}
}

func TestPaymentOutcomeProblem(t *testing.T) {
	code, _, active := paymentOutcomeProblem(http.StatusConflict)
	if !active || code != http.StatusConflict {
		t.Fatalf("conflict must be retryable: code=%d active=%t", code, active)
	}
	if _, _, active = paymentOutcomeProblem(http.StatusBadRequest); active {
		t.Fatal("bad request must not be treated as an active operation")
	}
}

func TestTerminalCheckoutCode(t *testing.T) {
	if got := terminalCheckoutCode("declined"); got != http.StatusPaymentRequired {
		t.Fatalf("declined code = %d, want %d", got, http.StatusPaymentRequired)
	}
	if got := terminalCheckoutCode("timeout"); got != http.StatusRequestTimeout {
		t.Fatalf("timeout code = %d, want %d", got, http.StatusRequestTimeout)
	}
}

func TestPersistenceReadProblem(t *testing.T) {
	if code, message := persistenceReadProblem(sql.ErrNoRows); code != http.StatusNotFound || message != "not found" {
		t.Fatalf("not found mapping = %d %q", code, message)
	}
	if code, message := persistenceReadProblem(errors.New("database unavailable")); code != http.StatusServiceUnavailable || message != "temporarily unavailable" {
		t.Fatalf("database mapping = %d %q", code, message)
	}
}

func TestCheckoutRejectsUnknownPaymentToken(t *testing.T) {
	if fakepsp.ValidToken("not-a-token") {
		t.Fatal("unknown token accepted")
	}
}
