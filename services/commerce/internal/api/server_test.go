package api

import "testing"

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
