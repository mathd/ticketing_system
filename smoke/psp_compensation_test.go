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

	// After the refund completes, status reports the compensation, not the stale capture:
	// the money is no longer held (ai-review B6).
	code, body = internalJSON(t, http.MethodGet, statusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("post-refund status = %d: %s", code, body)
	}
	var postRefund struct {
		Outcome        string `json:"outcome"`
		Captured       bool   `json:"captured"`
		CapturedAmount int64  `json:"captured_amount"`
	}
	if err := json.Unmarshal(body, &postRefund); err != nil {
		t.Fatalf("post-refund status body: %v", err)
	}
	if postRefund.Outcome != "refunded" || postRefund.Captured || postRefund.CapturedAmount != 0 {
		t.Fatalf("post-refund status = %+v, want refunded with nothing captured", postRefund)
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

	// Void happy path: the auth-hold token simulates an interrupted provider flow — the
	// charge authorizes but never captures (500, operation stays unresolved), PSP status
	// resolves it to authorized-uncaptured and persists that evidence, and void then
	// releases the hold, appending payment.voided.
	holdKey := "psp-comp-hold-" + uuid.NewString()
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", holdKey,
		map[string]any{"order_id": uuid.NewString(), "organizer_id": organizer, "buyer_id": buyer,
			"amount": 800, "currency": "EUR", "payment_token": "fake-auth-hold"}); code != http.StatusInternalServerError {
		t.Fatalf("auth-hold charge = %d, want 500 (fails closed, operation unresolved): %s", code, body)
	}
	holdStatusURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizer + "&idempotency_key=" + holdKey
	code, body = internalJSON(t, http.MethodGet, holdStatusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("auth-hold status = %d: %s", code, body)
	}
	var holdStatus struct {
		Outcome          string `json:"outcome"`
		Authorized       bool   `json:"authorized"`
		AuthorizedAmount int64  `json:"authorized_amount"`
		CapturedAmount   int64  `json:"captured_amount"`
	}
	if err := json.Unmarshal(body, &holdStatus); err != nil {
		t.Fatalf("auth-hold status body: %v", err)
	}
	if holdStatus.Outcome != "authorized" || !holdStatus.Authorized || holdStatus.AuthorizedAmount != 800 || holdStatus.CapturedAmount != 0 {
		t.Fatalf("auth-hold status = %+v, want authorized 800/0", holdStatus)
	}
	holdComp := map[string]any{"organizer_id": organizer, "idempotency_key": holdKey}
	// Uncaptured money cannot be refunded.
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/refund", "", holdComp); code != http.StatusConflict {
		t.Fatalf("refund of authorized-uncaptured = %d, want 409: %s", code, body)
	}
	code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/void", "", holdComp)
	if code != http.StatusOK {
		t.Fatalf("void = %d: %s", code, body)
	}
	var void struct {
		Status string    `json:"status"`
		FactID uuid.UUID `json:"fact_id"`
		Replay bool      `json:"replay"`
	}
	if err := json.Unmarshal(body, &void); err != nil {
		t.Fatalf("void body: %v", err)
	}
	if void.Status != "voided" || void.Replay || void.FactID == uuid.Nil {
		t.Fatalf("void = %+v, want fresh voided with a fact id", void)
	}
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/void", "", holdComp); code != http.StatusOK {
		t.Fatalf("void replay = %d: %s", code, body)
	} else {
		var voidReplay struct {
			FactID uuid.UUID `json:"fact_id"`
			Replay bool      `json:"replay"`
		}
		if err := json.Unmarshal(body, &voidReplay); err != nil {
			t.Fatalf("void replay body: %v", err)
		}
		if !voidReplay.Replay || voidReplay.FactID != void.FactID {
			t.Fatalf("void replay = %+v, want replay of fact %s", voidReplay, void.FactID)
		}
	}

	// After the void completes, status reports voided (terminal-no-side-effect), and a
	// void retry still replays even though the evidence has moved on (ai-review B5/B6).
	code, body = internalJSON(t, http.MethodGet, holdStatusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("post-void status = %d: %s", code, body)
	}
	var postVoid struct {
		Outcome              string `json:"outcome"`
		TerminalNoSideEffect bool   `json:"terminal_no_side_effect"`
	}
	if err := json.Unmarshal(body, &postVoid); err != nil {
		t.Fatalf("post-void status body: %v", err)
	}
	if postVoid.Outcome != "voided" || !postVoid.TerminalNoSideEffect {
		t.Fatalf("post-void status = %+v, want voided+terminal", postVoid)
	}
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/void", "", holdComp); code != http.StatusOK {
		t.Fatalf("void replay after status = %d, want 200 replay: %s", code, body)
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
