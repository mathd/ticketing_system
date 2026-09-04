//go:build smoke

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// Provider-neutral amount evidence (TKT-168, ADR-032 §Provider-neutral evidence reads).
//
// The gap this closes: /internal/operations answered `resolved`/`status` and no amount, and
// nothing at all read a refund leg — so a caller could prove a charge RAN but not what it
// MOVED, and could not prove a refund leg existed. Changing a charge amount to 1 left every
// assertion in the exchange suite green.
//
// These run against a REAL store, because the thing under test is the projection from a
// durable row to the wire: which column is read, and which columns must never leave. A fake
// store would prove the fake and the handler agree.

const evidenceCredential = "evidence-credential"

// evidenceOperation is the seed's shape. request and captured amounts are DELIBERATELY
// different: an implementation that answers with request_amount is a real defect this
// suite must be able to see, and a fixture that seeds them equal cannot see it.
type evidenceOperation struct {
	org       uuid.UUID
	key       string
	request   int64
	captured  int64
	currency  string
	state     string // provider_state; "" leaves the operation unresolved
	providerP string
	providerC string
	methodRef string
	// confirmed is the PROVIDER's own figure (TKT-257). 0 leaves both confirmation columns
	// NULL, which is the shape of every row written before migration 0006 — so the default
	// seed is a legacy row, and a test wanting confirmed evidence has to say so.
	confirmed int64
}

// evidenceServer wires the real store and router, then seeds the given operations.
func evidenceServer(t *testing.T, ops ...evidenceOperation) (http.Handler, *sql.DB) {
	t.Helper()
	db, ctx := refundDB(t)
	ring, err := store.NewKeyring("evidence-key", []byte("payments-evidence-key-0123456789"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		var status any
		if op.state != "" {
			status = op.state
		}
		// The confirmation is resolved in Go and passed as two plain parameters, both NULL
		// when the seed asks for a legacy row. It used to be derived in SQL from the
		// amount parameter, which reused $7 (request_currency) in a second position and
		// made PostgreSQL refuse the statement outright: "inconsistent types deduced for
		// parameter $7". Deciding it here is also simply clearer about which shape is
		// being seeded.
		var confirmedAmount, confirmedCurrency any
		if op.confirmed != 0 {
			confirmedAmount, confirmedCurrency = op.confirmed, op.currency
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
			                               request_amount,request_currency,payment_method_ref,provider_payment_ref,
			                               provider_charge_ref,provider_state,authorized_amount,captured_amount,provider_state_at,
			                               confirmed_captured_amount,confirmed_currency)
			VALUES($1,$2,'fingerprint',$3,$4,$5,$6,$7,$8,$9,$10,$3,$6,$11,now(),$12,$13)`,
			op.org, op.key, status, uuid.New(), uuid.New(), op.request, op.currency,
			op.methodRef, op.providerP, op.providerC, op.captured,
			confirmedAmount, confirmedCurrency); err != nil {
			t.Fatal(err)
		}
		org := op.org
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM payment_refund_legs WHERE organizer_id=$1`, org)
			_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
		})
	}
	return newTestServerWithPSP(store.New(db, ring), evidenceCredential, psp.NewFake()).Router(nil, true), db
}

// getEvidence issues an authenticated GET. token == "" sends no credential at all.
func getEvidence(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func operationPath(org uuid.UUID, key string) string {
	return fmt.Sprintf("/internal/operations?organizer_id=%s&idempotency_key=%s", org, key)
}

func refundLegPath(org uuid.UUID, sourceKey, refundKey string) string {
	return fmt.Sprintf("/internal/refund-legs?organizer_id=%s&source_idempotency_key=%s&refund_idempotency_key=%s",
		org, sourceKey, refundKey)
}

// decodeEvidence returns the response as a generic map, so a field's ABSENCE is
// observable. A typed struct would silently zero a missing field, which is exactly the
// distinction these tests turn on.
func decodeEvidence(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", res.Body.String(), err)
	}
	return out
}

// A captured payment's evidence states how much money it moved, in its own currency.
//
// Written against the requirement, not against a run: the seed captures 1250 of a 2000
// request, so 1250 is what the rule says must be reported and 2000 is the value the most
// likely wrong implementation would report.
func TestCapturedOperationEvidenceReportsWhatMoved(t *testing.T) {
	org, key := uuid.New(), "evidence-captured"
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		currency: "EUR", state: "captured", providerP: "pi_leak", providerC: "ch_leak", methodRef: "pm_leak"})

	res := getEvidence(t, h, operationPath(org, key), evidenceCredential)
	if res.Code != http.StatusOK {
		t.Fatalf("captured operation evidence: status=%d body=%s", res.Code, res.Body.String())
	}
	body := decodeEvidence(t, res)
	if got, ok := body["captured_amount"]; !ok || got != float64(1250) {
		t.Fatalf("captured_amount = %v (present=%t), want the captured 1250 and not the requested 2000", got, ok)
	}
	if got := body["currency"]; got != "EUR" {
		t.Fatalf("currency = %v, want EUR", got)
	}
}

// Evidence names money and currency; it never names the processor, the instrument, or the
// provider object that moved it.
//
// The generator fills EVERY provider-identifying column with a marker and then asserts the
// whole serialized body contains none of them — a harness that seeded a harmless value in
// the position that leaks could not observe a leak (AGENTS.md, TKT-202).
func TestOperationEvidenceCarriesNoProviderIdentity(t *testing.T) {
	org, key := uuid.New(), "evidence-neutral"
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		currency: "EUR", state: "captured",
		providerP: "pi_must_not_leak", providerC: "ch_must_not_leak", methodRef: "pm_must_not_leak"})

	res := getEvidence(t, h, operationPath(org, key), evidenceCredential)
	if res.Code != http.StatusOK {
		t.Fatalf("operation evidence: status=%d body=%s", res.Code, res.Body.String())
	}
	for _, secret := range []string{"pi_must_not_leak", "ch_must_not_leak", "pm_must_not_leak"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("provider identity %q crossed the evidence boundary: %s", secret, res.Body.String())
		}
	}
}

// An operation that has not captured reports no captured amount — silence, not a zero.
//
// A zero would be indistinguishable from a genuine zero-value capture, so the assertion is
// on ABSENCE. The fixture reaches the failing state because the row exists with a NULL
// status: it is never repaired into a captured state during setup.
func TestUnresolvedOperationReportsNoAmountEvidence(t *testing.T) {
	org, key := uuid.New(), "evidence-unresolved"
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 0,
		currency: "EUR", state: ""})

	res := getEvidence(t, h, operationPath(org, key), evidenceCredential)
	if res.Code != http.StatusOK {
		t.Fatalf("unresolved operation: status=%d body=%s", res.Code, res.Body.String())
	}
	body := decodeEvidence(t, res)
	if body["resolved"] != false {
		t.Fatalf("resolved = %v, want false — the fixture must reach the unresolved branch", body["resolved"])
	}
	if got, ok := body["captured_amount"]; ok {
		t.Fatalf("captured_amount = %v on an unresolved operation, want it absent", got)
	}
	if got, ok := body["currency"]; ok {
		t.Fatalf("currency = %v on an unresolved operation, want it absent", got)
	}
}

// Evidence is answered only to a caller that presents the internal credential, and a
// refusal discloses nothing about whether the evidence exists.
//
// The assertion is PAIRED on purpose: the same seeded row read with and without the
// credential. Deleting the guard flips the second case from 401 to 200 with the amount in
// it — an unauthenticated request cannot otherwise be told apart from a missing row.
func TestEvidenceReadsRequireTheInternalCredential(t *testing.T) {
	org, key, refundKey := uuid.New(), "evidence-auth", "refund-auth"
	h, db := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		currency: "EUR", state: "captured"})
	seedRefundLeg(t, db, org, key, refundKey, 500, "EUR", true)

	for _, tc := range []struct{ name, path string }{
		{"operation", operationPath(org, key)},
		{"refund leg", refundLegPath(org, key, refundKey)},
	} {
		if res := getEvidence(t, h, tc.path, evidenceCredential); res.Code != http.StatusOK {
			t.Fatalf("%s with the credential: status=%d body=%s", tc.name, res.Code, res.Body.String())
		}
		res := getEvidence(t, h, tc.path, "")
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s without the credential: status=%d, want 401", tc.name, res.Code)
		}
		for _, disclosed := range []string{"captured_amount", "amount", "currency", "completed"} {
			if strings.Contains(res.Body.String(), disclosed) {
				t.Fatalf("a refused %s read disclosed %q: %s", tc.name, disclosed, res.Body.String())
			}
		}
	}
}

// An operation is answered only when the organizer AND the key name the same record.
//
// One case per predicate, each satisfying the other, so deleting either SQL predicate
// independently turns a 404 into a 200 (AGENTS.md: a guard with N predicates needs N
// tests). A single wrong-everything case would pass with either predicate deleted.
func TestOperationEvidenceIsScopedByOrganizerAndKey(t *testing.T) {
	org, key := uuid.New(), "evidence-scope"
	other := uuid.New()
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		currency: "EUR", state: "captured"})

	for _, tc := range []struct {
		name string
		path string
	}{
		{"another organizer's read of a key that exists", operationPath(other, key)},
		{"this organizer's read of a key that does not", operationPath(org, key+"-absent")},
	} {
		res := getEvidence(t, h, tc.path, evidenceCredential)
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s, want 404", tc.name, res.Code, res.Body.String())
		}
	}
}

// TKT-257. The two amount fields answer different questions, and the read must keep them
// distinguishable:
//
//	captured_amount            — what payments durably RECORDED for this operation.
//	confirmed_captured_amount  — what the PROVIDER said it moved, checked against the
//	                             request before it was recorded.
//
// Written against the requirement rather than against a run: the seed captures 1250 and has
// the provider confirm 1250, while the REQUEST was 2000. So an implementation that answered
// the confirmed field from request_amount reports 2000, and one that answered it from
// captured_amount happens to be right here — which is why the legacy test below, where the
// two must diverge, is the one that proves the source.
func TestCapturedOperationEvidenceReportsTheProvidersOwnFigure(t *testing.T) {
	org, key := uuid.New(), "evidence-confirmed"
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		confirmed: 1250, currency: "EUR", state: "captured", providerP: "pi_x", providerC: "ch_x"})

	body := decodeEvidence(t, getEvidence(t, h, operationPath(org, key), evidenceCredential))
	if got, ok := body["confirmed_captured_amount"]; !ok || got != float64(1250) {
		t.Fatalf("confirmed_captured_amount = %v (present=%t), want 1250", got, ok)
	}
	if got := body["confirmed_currency"]; got != "EUR" {
		t.Fatalf("confirmed_currency = %v, want EUR", got)
	}
	// The recorded figure keeps its own meaning and stays present. Commerce's recovery
	// runner decides whether to refund from captured_amount, so this field must not be
	// redefined or dropped by the addition of the confirmed one.
	if got, ok := body["captured_amount"]; !ok || got != float64(1250) {
		t.Fatalf("captured_amount = %v (present=%t), want it unchanged at 1250", got, ok)
	}
}

// A row written before migration 0006 has no provider confirmation and never can. It must
// answer ABSENT rather than having its requested or recorded figure promoted to confirmed
// evidence — the promotion would erase the distinction being added, silently and forever.
//
// The seed makes captured (1250) and request (2000) differ AND leaves the confirmation NULL,
// so the two plausible wrong implementations — falling back to captured_amount, or to
// request_amount — each produce a present key and fail here. Absence is the assertion.
func TestLegacyOperationEvidenceOmitsProviderConfirmation(t *testing.T) {
	org, key := uuid.New(), "evidence-legacy"
	h, _ := evidenceServer(t, evidenceOperation{org: org, key: key, request: 2000, captured: 1250,
		currency: "EUR", state: "captured", providerP: "pi_legacy"})

	body := decodeEvidence(t, getEvidence(t, h, operationPath(org, key), evidenceCredential))
	if got, present := body["confirmed_captured_amount"]; present {
		t.Fatalf("a pre-0006 row has no provider confirmation; the read must omit it, got %v", got)
	}
	if got, present := body["confirmed_currency"]; present {
		t.Fatalf("confirmed_currency must be omitted for a legacy row, got %v", got)
	}
	// The row is still perfectly readable for what it DOES record.
	if got, ok := body["captured_amount"]; !ok || got != float64(1250) {
		t.Fatalf("a legacy row must keep reporting what payments recorded: %v (present=%t)", got, ok)
	}
}
