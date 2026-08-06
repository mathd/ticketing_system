//go:build smoke

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// TKT-217, found by adversarial review: the settlement plan was validated AFTER
// the provider had already taken the money. An internal caller sending a charge
// with a missing or unusable plan got an error back while the PSP had captured,
// leaving no payment.captured fact and no ledger — the deferred triggers cannot
// repair that, because they only govern journal commits, not the outside world.
//
// The assertion that matters is not the status code. It is that the provider was
// never called.

func (c *countingPSP) Authorize(ctx context.Context, req psp.AuthorizeRequest) (psp.Result, error) {
	c.mu.Lock()
	c.authorizes++
	c.mu.Unlock()
	return c.PSP.Authorize(ctx, req)
}

func (c *countingPSP) authorizeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authorizes
}

func chargeServer(t *testing.T) (http.Handler, *countingPSP) {
	t.Helper()
	db, _ := refundDB(t)
	ring, err := store.NewKeyring("refund-key", []byte("payments-refund-leg-key-0123456"), "")
	if err != nil {
		t.Fatal(err)
	}
	provider := &countingPSP{PSP: psp.NewFake()}
	return NewWithPSP(store.New(db, ring), refundCredential, provider).Router(nil, true), provider
}

func postCharge(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/charges", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", refundCredential)
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestAPlanlessChargeNeverReachesTheProvider(t *testing.T) {
	h, provider := chargeServer(t)
	org := uuid.New()
	body := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + uuid.New().String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok"}`

	res := postCharge(t, h, "planless-"+org.String(), body)
	if res.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s, want 400", res.Code, res.Body.String())
	}
	if n := provider.authorizeCount(); n != 0 {
		t.Fatalf("provider called %d time(s) for a charge that cannot be settled — money "+
			"moved that the ledger can never account for", n)
	}
}

// The same guarantee for a plan that is present but cannot be built into a
// balanced ledger. A payout misconfiguration must not cost the buyer a capture
// that the system then refuses to record.
func TestAnUnusableSettlementPlanNeverReachesTheProvider(t *testing.T) {
	h, provider := chargeServer(t)
	org := uuid.New()
	// Shares that do not total 10000 bps: the allocator refuses, so no balanced
	// ledger exists for this charge.
	body := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + uuid.New().String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok","settlement":{` +
		`"face_value":5000,"passed_on":600,"absorbed":0,"total_amount":5600,"currency":"EUR","fees":[{"fee_code":"booking","incidence":"passed_on",` +
		`"amount":600,"currency":"EUR","parts":[{"payee_id":"` + uuid.New().String() +
		`","kind":"venue","display_name":"The venue","share_bps":4000}]}]}}`

	res := postCharge(t, h, "unusable-"+org.String(), body)
	if res.Code != http.StatusInternalServerError {
		t.Errorf("status=%d body=%s, want 500", res.Code, res.Body.String())
	}
	if n := provider.authorizeCount(); n != 0 {
		t.Fatalf("provider called %d time(s) for an unusable plan", n)
	}
}
