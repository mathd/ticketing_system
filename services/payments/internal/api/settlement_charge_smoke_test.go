//go:build smoke

package api

import (
	"bytes"
	"context"
	"fmt"
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
	return newTestServerWithPSP(store.New(db, ring), refundCredential, provider).Router(nil, true), provider
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

// Second review pass, and a defect the FIRST pass's fix created: moving plan
// validation ahead of the provider left it behind BindOperation. A charge that
// cannot be settled then persisted a pending operation before failing — and a
// pending operation is not inert. Commerce's recovery path treats one as
// payment-unknown and resolves it against the provider, so an operation that
// never had a usable ledger could be confirmed as captured.
//
// Asserting "the provider was not called" was too weak to see this. The state
// that matters is the durable one.
func TestAnUnsettleableChargeLeavesNoOperationBehind(t *testing.T) {
	h, provider := chargeServer(t)
	db, ctx := refundDB(t)
	org := uuid.New()
	key := "no-operation-" + org.String()
	// The UNUSABLE plan, not the absent one. A plan-less charge is now refused by
	// the contract validator before the handler runs, so it proves nothing about
	// the handler's own ordering. A well-formed plan that cannot be allocated is
	// the case that actually reaches BindOperation.
	body := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + uuid.New().String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok","settlement":{` +
		`"face_value":5000,"passed_on":600,"absorbed":0,"total_amount":5600,"currency":"EUR",` +
		`"fees":[{"fee_code":"booking","incidence":"passed_on","amount":600,"currency":"EUR",` +
		`"parts":[{"payee_id":"` + uuid.New().String() +
		`","kind":"venue","display_name":"The venue","share_bps":4000}]}]}}`

	if res := postCharge(t, h, key, body); res.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", res.Code, res.Body.String())
	}
	if n := provider.authorizeCount(); n != 0 {
		t.Errorf("provider called %d time(s)", n)
	}
	var operations int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	// Positive control. Without it this query could be observing nothing at all —
	// a wrong table, a wrong column, a scope that matches no row — and would read
	// as "no operation survived" no matter what the handler did.
	var settleable int
	good := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + uuid.New().String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok",` + feeFreePlan(5600) + `}`
	goodKey := "control-" + org.String()
	if res := postCharge(t, h, goodKey, good); res.Code != http.StatusOK {
		t.Fatalf("control charge: status=%d body=%s", res.Code, res.Body.String())
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, goodKey).Scan(&settleable); err != nil {
		t.Fatal(err)
	}
	if settleable != 1 {
		t.Fatalf("the control charge left %d operation(s), want 1 — this query cannot see "+
			"what it claims to be checking", settleable)
	}
	if operations != 0 {
		t.Fatalf("%d payment operation(s) survive a charge that can never be settled — "+
			"recovery can resolve a pending operation against the provider, so this one is "+
			"a capture waiting to happen with no ledger to record it", operations)
	}
}

// A settlement plan that attributes the whole capture to the organizer and owes
// nobody a fee. Enough to make a charge settleable without involving payees.
func feeFreePlan(amount int64) string {
	return fmt.Sprintf(`"settlement":{"face_value":%d,"passed_on":0,"absorbed":0,`+
		`"total_amount":%d,"currency":"EUR","fees":[]}`, amount, amount)
}

// Idempotency must not depend on the settlement plan travelling with every retry.
// The plan is not part of the request fingerprint, and a replay of a captured
// charge has to return that capture — not a plan error from validation that now
// runs earlier than the operation lookup.
func TestAReplayedCaptureIsAnsweredFromTheRecord(t *testing.T) {
	h, provider := chargeServer(t)
	org := uuid.New()
	key := "replay-" + org.String()
	order, buyer := uuid.New().String(), uuid.New().String()
	base := `{"organizer_id":"` + org.String() + `","order_id":"` + order +
		`","buyer_id":"` + buyer + `","amount":5600,"currency":"EUR","payment_token":"fake-ok",`

	first := postCharge(t, h, key, base+feeFreePlan(5600)+`}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first charge: status=%d body=%s", first.Code, first.Body.String())
	}
	// The same charge again, this time carrying a plan that cannot be allocated.
	unusable := base + `"settlement":{"face_value":5000,"passed_on":600,"absorbed":0,` +
		`"total_amount":5600,"currency":"EUR","fees":[{"fee_code":"booking",` +
		`"incidence":"passed_on","amount":600,"currency":"EUR","parts":[{"payee_id":"` +
		uuid.New().String() + `","kind":"venue","display_name":"The venue","share_bps":4000}]}]}}`
	second := postCharge(t, h, key, unusable)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%s, want the stored 200 — a decided operation is "+
			"answered from the record, not re-validated", second.Code, second.Body.String())
	}
	if n := provider.authorizeCount(); n != 1 {
		t.Errorf("provider called %d time(s) across a charge and its replay, want 1", n)
	}
}

// And the reused-key 409 must survive the same reordering: a different request
// under an existing key is a conflict whatever its plan looks like.
func TestAReusedKeyWithADifferentRequestStillConflicts(t *testing.T) {
	h, _ := chargeServer(t)
	org := uuid.New()
	key := "conflict-" + org.String()
	buyer := uuid.New().String()
	first := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + buyer + `","amount":5600,"currency":"EUR","payment_token":"fake-ok",` +
		feeFreePlan(5600) + `}`
	if res := postCharge(t, h, key, first); res.Code != http.StatusOK {
		t.Fatalf("first charge: status=%d body=%s", res.Code, res.Body.String())
	}
	// Different order and amount, same key, and a plan that cannot be allocated.
	other := `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + buyer + `","amount":9900,"currency":"EUR","payment_token":"fake-ok",` +
		`"settlement":{"face_value":9000,"passed_on":900,"absorbed":0,"total_amount":9900,` +
		`"currency":"EUR","fees":[{"fee_code":"booking","incidence":"passed_on","amount":900,` +
		`"currency":"EUR","parts":[{"payee_id":"` + uuid.New().String() +
		`","kind":"venue","display_name":"The venue","share_bps":4000}]}]}}`
	res := postCharge(t, h, key, other)
	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 — a reused key with a different request is a "+
			"conflict, and an unusable plan must not answer in its place", res.Code, res.Body.String())
	}
}

// TKT-219, found by the fourth adversarial review pass on TKT-217.
//
// The request fingerprint covers order, buyer, amount, currency and token — not
// the settlement plan. An operation that bound, captured at the provider and
// died before journalling leaves an unresolved row; once its lease expires, a
// retry can arrive with a DIFFERENT valid plan and the already-captured money
// would be recorded under that new attribution.
//
// Seeded rather than raced: the state that matters is an unresolved operation
// with an expired lease and a known plan digest, and provoking it through a real
// crash would test the crash, not the rule.
func TestALeaseRetryCannotSwapTheSettlementPlan(t *testing.T) {
	h, provider := chargeServer(t)
	db, ctx := refundDB(t)
	org := uuid.New()
	order, buyer := uuid.New(), uuid.New()

	// The plan the operation bound with: the whole capture to the organizer.
	bound, err := store.BuildSettlementEntries(store.SettlementPlan{
		FaceValue: 5600, TotalAmount: 5600, Currency: "EUR",
	}, 5600)
	if err != nil {
		t.Fatal(err)
	}
	// A real charge first, only to obtain the fingerprint the handler computes,
	// so the fixture cannot drift from the real formula.
	seedKey := "lease-seed-" + org.String()
	body := `{"organizer_id":"` + org.String() + `","order_id":"` + order.String() +
		`","buyer_id":"` + buyer.String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok",` + feeFreePlan(5600) + `}`
	if res := postCharge(t, h, seedKey, body); res.Code != http.StatusOK {
		t.Fatalf("seed charge: status=%d body=%s", res.Code, res.Body.String())
	}
	var fingerprint string
	if err := db.QueryRowContext(ctx,
		`SELECT request_fingerprint FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, seedKey).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}

	// The stranded operation: same request, no outcome, lease long expired.
	strandedKey := "lease-stranded-" + org.String()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations
		  (organizer_id,idempotency_key,request_fingerprint,order_id,buyer_id,
		   request_amount,request_currency,payment_method_ref,lease_until,settlement_digest)
		VALUES ($1,$2,$3,$4,$5,5600,'EUR','fake-ok',now()-interval '1 hour',$6)`,
		org, strandedKey, fingerprint, order, buyer, store.PlanDigest(bound)); err != nil {
		t.Fatal(err)
	}
	before := provider.authorizeCount()

	// The retry: same request in every field the fingerprint covers, and a
	// different attribution.
	swapped := `{"organizer_id":"` + org.String() + `","order_id":"` + order.String() +
		`","buyer_id":"` + buyer.String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok","settlement":{` +
		`"face_value":5000,"passed_on":600,"absorbed":0,"total_amount":5600,"currency":"EUR",` +
		`"fees":[{"fee_code":"booking","incidence":"passed_on","amount":600,"currency":"EUR",` +
		`"parts":[{"payee_id":"` + uuid.New().String() +
		`","kind":"venue","display_name":"The venue","share_bps":10000}]}]}}`
	res := postCharge(t, h, strandedKey, swapped)
	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 — a retry may not re-attribute money the "+
			"operation already bound a plan for", res.Code, res.Body.String())
	}
	if n := provider.authorizeCount(); n != before {
		t.Errorf("provider called %d extra time(s) for a refused retry", n-before)
	}

	// The control: the SAME plan still gets through after a lease expiry, or the
	// rule would have turned a recoverable operation into a stuck one.
	same := `{"organizer_id":"` + org.String() + `","order_id":"` + order.String() +
		`","buyer_id":"` + buyer.String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok",` + feeFreePlan(5600) + `}`
	if res := postCharge(t, h, strandedKey, same); res.Code != http.StatusOK {
		t.Fatalf("retry with the bound plan: status=%d body=%s, want 200 — an expired lease "+
			"must still be recoverable", res.Code, res.Body.String())
	}
}
