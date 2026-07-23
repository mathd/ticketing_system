//go:build smoke

package smoke_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The PSP recovery surface (TKT-114/S2, ADR-032) against the composed stack: real
// PostgreSQL (migration 0002), real journal, fake PSP. Drives the durable compensation
// sequence the unit tests cannot (bind → provider → fact → complete → replay), and the
// contract middleware validates every response against the payments OpenAPI document.
func TestPSPCompensationFlow(t *testing.T) {
	organizer := uuid.NewString()
	buyer := uuid.NewString()
	order := uuid.NewString()
	chargeKey := "psp-comp-smoke-" + uuid.NewString()

	// A fake-ok charge: captured, with provider evidence persisted on the operation row.
	code, body := internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", chargeKey,
		map[string]any{"order_id": order, "organizer_id": organizer, "buyer_id": buyer,
			"amount": 1250, "currency": "EUR", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("charge = %d: %s", code, body)
	}

	// Status answers from the recorded terminal evidence, provider-neutrally.
	statusURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizer + "&idempotency_key=" + chargeKey
	code, body = internalJSON(t, http.MethodGet, statusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("psp status = %d: %s", code, body)
	}
	var st struct {
		Outcome        string `json:"outcome"`
		Captured       bool   `json:"captured"`
		CapturedAmount int64  `json:"captured_amount"`
		Currency       string `json:"currency"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if st.Outcome != "captured" || !st.Captured || st.CapturedAmount != 1250 || st.Currency != "EUR" {
		t.Fatalf("status = %+v, want captured 1250 EUR", st)
	}

	// Captured money cannot be voided — the stored evidence, not the caller, decides.
	compensation := map[string]any{"organizer_id": organizer, "idempotency_key": chargeKey}
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/void", "", compensation); code != http.StatusConflict {
		t.Fatalf("void of captured operation = %d, want 409: %s", code, body)
	}

	// Refund: appends payment.refunded and completes the compensation.
	code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/refund", "", compensation)
	if code != http.StatusOK {
		t.Fatalf("refund = %d: %s", code, body)
	}
	var refund struct {
		Status string    `json:"status"`
		FactID uuid.UUID `json:"fact_id"`
		Replay bool      `json:"replay"`
	}
	if err := json.Unmarshal(body, &refund); err != nil {
		t.Fatalf("refund body: %v", err)
	}
	if refund.Status != "refunded" || refund.Replay || refund.FactID == uuid.Nil {
		t.Fatalf("refund = %+v, want fresh refunded with a fact id", refund)
	}

	// A duplicate refund replays the stored completion: same fact, no second provider
	// operation (the deterministic compensation key + PK make this structural).
	code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/refund", "", compensation)
	if code != http.StatusOK {
		t.Fatalf("refund replay = %d: %s", code, body)
	}
	var replayed struct {
		Status string    `json:"status"`
		FactID uuid.UUID `json:"fact_id"`
		Replay bool      `json:"replay"`
	}
	if err := json.Unmarshal(body, &replayed); err != nil {
		t.Fatalf("replay body: %v", err)
	}
	if !replayed.Replay || replayed.FactID != refund.FactID {
		t.Fatalf("replay = %+v, want replay of fact %s", replayed, refund.FactID)
	}

	// An unknown operation supports nothing: 404 on all three endpoints.
	missing := map[string]any{"organizer_id": organizer, "idempotency_key": "psp-comp-missing-" + uuid.NewString()}
	missingURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizer + "&idempotency_key=missing-" + uuid.NewString()
	if code, body = internalJSON(t, http.MethodGet, missingURL, "", nil); code != http.StatusNotFound {
		t.Fatalf("status of missing operation = %d: %s", code, body)
	}
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/refund", "", missing); code != http.StatusNotFound {
		t.Fatalf("refund of missing operation = %d: %s", code, body)
	}

	// A declined charge left no captured money: refund refused.
	declinedKey := "psp-comp-declined-" + uuid.NewString()
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", declinedKey,
		map[string]any{"order_id": uuid.NewString(), "organizer_id": organizer, "buyer_id": buyer,
			"amount": 900, "currency": "EUR", "payment_token": "fake-decline"}); code != http.StatusPaymentRequired {
		t.Fatalf("declined charge = %d: %s", code, body)
	}
	declined := map[string]any{"organizer_id": organizer, "idempotency_key": declinedKey}
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/refund", "", declined); code != http.StatusConflict {
		t.Fatalf("refund of declined operation = %d, want 409: %s", code, body)
	}
}
