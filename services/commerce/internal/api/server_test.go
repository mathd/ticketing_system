package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPaymentFailureResponse(t *testing.T) {
	tests := []struct {
		name, body, fallback, wantStatus string
	}{
		{name: "valid object", body: `{"status":"declined","reason":"card_declined"}`, fallback: "fallback", wantStatus: "declined"},
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
