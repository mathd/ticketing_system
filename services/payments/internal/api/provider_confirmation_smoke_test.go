//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// TKT-257 — the write-path proofs. Payments used to record what it ASKED the provider to
// move as evidence of what the provider DID move, and nothing failed closed on a
// disagreement.
//
// Why these tests need a divergent stub rather than the fake PSP: the fake echoes the
// request, so it can never disagree with itself. That is precisely why every pre-existing
// test in this repo stayed green against a live defect, and it is the reason this file
// exists as a separate seam.
//
// Two independent predicates guard each sink — AMOUNT and CURRENCY — and an earlier refusal
// short-circuits the later one. So each case below satisfies the OTHER predicate, and each
// therefore goes red only when its own comparison is removed. A case that failed both would
// be silent about which guard caught it.

// divergentPSP answers with money that does not match the request. It shadows the whole PSP
// surface rather than embedding a helpful default: the point is a provider that lies, and a
// stub that helpfully echoed would reproduce the defect under test.
type divergentPSP struct {
	psp.PSP
	amount   int64
	currency string
	// absent makes the provider report NO confirmation at all — silence, which must not be
	// read as assent.
	absent bool
}

func (d *divergentPSP) confirmation(reqAmount int64, reqCurrency string) *psp.ConfirmedMoney {
	if d.absent {
		return nil
	}
	c := psp.ConfirmedMoney{Amount: reqAmount, Currency: reqCurrency}
	if d.amount != 0 {
		c.Amount = d.amount
	}
	if d.currency != "" {
		c.Currency = d.currency
	}
	return &c
}

func (d *divergentPSP) Authorize(_ context.Context, req psp.AuthorizeRequest) (psp.Result, error) {
	return psp.Result{
		Outcome: psp.Captured, Captured: true, Authorized: true,
		ProviderRef: "pi_divergent", Confirmed: d.confirmation(req.Amount, req.Currency),
	}, nil
}

func (d *divergentPSP) Refund(_ context.Context, _, idempotencyKey string, amount int64, currency string) (psp.Result, error) {
	return psp.Result{
		Outcome: psp.Refunded, ProviderRef: "re_" + idempotencyKey,
		Confirmed: d.confirmation(amount, currency),
	}, nil
}

func divergentChargeServer(t *testing.T, p *divergentPSP) http.Handler {
	t.Helper()
	db, _ := refundDB(t)
	ring, err := store.NewKeyring("refund-key", []byte("payments-refund-leg-key-0123456"), "")
	if err != nil {
		t.Fatal(err)
	}
	p.PSP = psp.NewFake()
	return newTestServerWithPSP(store.New(db, ring), refundCredential, p).Router(nil, true)
}

// assertChargeWroteNothing re-reads the DATABASE rather than trusting the response code.
// A 502 alone would also be produced by a provider transport failure, and — the case that
// matters — by a handler that appended payment.authorized and only then refused. The
// journal's trigger forbids UPDATE and DELETE, so one premature append is unrecoverable;
// the only assertion that can see it is a count of what is actually in the table.
func assertChargeWroteNothing(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) {
	t.Helper()
	var facts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM journal_entries WHERE organizer_id=$1`, org).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("a refused charge left %d journal entries; the journal is append-only and cannot be repaired", facts)
	}
	var settlements int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM settlement_entries WHERE organizer_id=$1`, org).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if settlements != 0 {
		t.Fatalf("a refused charge wrote %d settlement entries; only a capture settles", settlements)
	}
	// The operation stays BOUND and unresolved — the payment_unknown shape, recoverable via
	// /internal/psp/status. A provider that captured the wrong amount DID move money, so
	// recording a terminal no-side-effect outcome would be a lie (ADR-016 §Dec3).
	var status sql.NullString
	var confirmed sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT status,confirmed_captured_amount FROM payment_operations WHERE organizer_id=$1`, org).Scan(&status, &confirmed); err != nil {
		t.Fatal(err)
	}
	if status.Valid {
		t.Fatalf("a refused charge resolved the operation to %q; it must stay recoverable", status.String)
	}
	if confirmed.Valid {
		t.Fatalf("a refused charge recorded confirmed money %d", confirmed.Int64)
	}
}

// A charge that is valid in every respect EXCEPT what the provider answers, so the only
// thing these tests can be refused for is the confirmation. It reuses feeFreePlan rather
// than hand-writing a settlement object: the plan is validated against the OpenAPI schema
// before the provider is called, so a hand-written one that drifts turns every assertion
// below into a 400 that never reaches the guard under test.
func chargeBody(org uuid.UUID) string {
	return `{"organizer_id":"` + org.String() + `","order_id":"` + uuid.New().String() +
		`","buyer_id":"` + uuid.New().String() +
		`","amount":5600,"currency":"EUR","payment_token":"fake-ok",` + feeFreePlan(5600) + `}`
}

// PREDICATE 1 of the charge guard: the provider captured a DIFFERENT AMOUNT. Currency
// matches, so this case reaches the amount comparison and nothing else.
func TestChargeFailsClosedWhenTheProviderCapturesADifferentAmount(t *testing.T) {
	org := uuid.New()
	h := divergentChargeServer(t, &divergentPSP{amount: 5599})
	db, ctx := refundDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})

	res := postCharge(t, h, "conf-amount-"+org.String(), chargeBody(org))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502: a provider that moved 5599 for a request of 5600 must not settle", res.Code, res.Body.String())
	}
	assertChargeWroteNothing(t, db, ctx, org)
}

// PREDICATE 2 of the charge guard: the AMOUNT MATCHES and the currency does not. Without
// this case the currency comparison could be deleted and every other test would stay green.
func TestChargeFailsClosedWhenTheProviderCapturesADifferentCurrency(t *testing.T) {
	org := uuid.New()
	h := divergentChargeServer(t, &divergentPSP{currency: "USD"})
	db, ctx := refundDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})

	res := postCharge(t, h, "conf-currency-"+org.String(), chargeBody(org))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502: 5600 USD is not 5600 EUR", res.Code, res.Body.String())
	}
	assertChargeWroteNothing(t, db, ctx, org)
}

// Silence is not assent. A provider that reports no figure has confirmed nothing, and
// settling on that is the fail-open this ticket closes — the pre-fix code did exactly this
// for every provider, because psp.Result carried no money at all.
func TestChargeFailsClosedWhenTheProviderConfirmsNothing(t *testing.T) {
	org := uuid.New()
	h := divergentChargeServer(t, &divergentPSP{absent: true})
	db, ctx := refundDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})

	res := postCharge(t, h, "conf-absent-"+org.String(), chargeBody(org))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502: an unconfirmed capture must not settle", res.Code, res.Body.String())
	}
	assertChargeWroteNothing(t, db, ctx, org)
}

// The agreement twin, over the SAME stub shape. Without it a disagreement test passes for
// the wrong reason — a handler that refused every charge, or a stub whose result never
// parsed, would satisfy all three tests above.
func TestChargeSettlesAndRecordsTheProvidersOwnFigure(t *testing.T) {
	org := uuid.New()
	h := divergentChargeServer(t, &divergentPSP{}) // no override: the provider agrees
	db, ctx := refundDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM settlement_entries WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})

	res := postCharge(t, h, "conf-ok-"+org.String(), chargeBody(org))
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", res.Code, res.Body.String())
	}
	// The COLUMN, re-read from PostgreSQL — not the response. store.ProviderResult gaining
	// a field does not add it to CompleteOperation's hand-written UPDATE column list, and
	// the compiler cannot see that gap: the response would still be correct while the
	// database recorded nothing.
	var confirmedAmount sql.NullInt64
	var confirmedCurrency sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT confirmed_captured_amount,confirmed_currency FROM payment_operations WHERE organizer_id=$1`, org).
		Scan(&confirmedAmount, &confirmedCurrency); err != nil {
		t.Fatal(err)
	}
	if !confirmedAmount.Valid || confirmedAmount.Int64 != 5600 || confirmedCurrency.String != "EUR" {
		t.Fatalf("confirmed money not persisted: amount=%v currency=%v", confirmedAmount, confirmedCurrency)
	}
}

// --- Refund legs: the same two predicates, one tier down ---

func divergentRefundServer(t *testing.T, org uuid.UUID, sourceKey string, captured int64, p *divergentPSP) http.Handler {
	t.Helper()
	db, ctx := refundDB(t)
	ring, err := store.NewKeyring("refund-key", []byte("payments-refund-leg-key-0123456"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,$5,'EUR','pi_test','captured',$5,$5,now())`,
		org, sourceKey, uuid.New(), uuid.New(), captured); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_refund_legs WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})
	p.PSP = psp.NewFake()
	return newTestServerWithPSP(store.New(db, ring), refundCredential, p).Router(nil, true)
}

// assertLegStillBound re-reads the LEG. A response-code assertion cannot distinguish a
// refusal that withheld the fact and the completion from one that appended the fact first
// and then failed — and ADR-037 §4 depends on the difference: a leg that stays bound keeps
// its allowance reserved against both ceilings, so nothing else can spend the money it may
// still take.
func assertLegStillBound(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) {
	t.Helper()
	var status string
	var factID uuid.NullUUID
	var confirmed sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT status,fact_id,confirmed_amount FROM payment_refund_legs WHERE organizer_id=$1`, org).
		Scan(&status, &factID, &confirmed); err != nil {
		t.Fatal(err)
	}
	if status != "bound" {
		t.Fatalf("leg status = %q, want bound: a disagreement must record no completion", status)
	}
	if factID.Valid {
		t.Fatalf("a refused leg carries fact %s; the fact must not be appended", factID.UUID)
	}
	if confirmed.Valid {
		t.Fatalf("a refused leg recorded confirmed money %d", confirmed.Int64)
	}
	var facts int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM journal_entries WHERE organizer_id=$1 AND fact_type='payment.refunded'`, org).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("a refused leg appended %d payment.refunded facts into an append-only journal", facts)
	}
}

// PREDICATE 1 of the leg guard: a different amount came back. Currency matches.
func TestRefundLegFailsClosedWhenTheProviderReturnsADifferentAmount(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-conf-amount"
	h := divergentRefundServer(t, org, sourceKey, 2500, &divergentPSP{amount: 1249})
	db, ctx := refundDB(t)

	res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-1","amount":1250,"currency":"EUR"}`)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", res.Code, res.Body.String())
	}
	assertLegStillBound(t, db, ctx, org)
}

// PREDICATE 2 of the leg guard: the amount matches, the currency does not.
func TestRefundLegFailsClosedWhenTheProviderReturnsADifferentCurrency(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-conf-currency"
	h := divergentRefundServer(t, org, sourceKey, 2500, &divergentPSP{currency: "USD"})
	db, ctx := refundDB(t)

	res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-1","amount":1250,"currency":"EUR"}`)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", res.Code, res.Body.String())
	}
	assertLegStillBound(t, db, ctx, org)
}

func TestRefundLegFailsClosedWhenTheProviderConfirmsNothing(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-conf-absent"
	h := divergentRefundServer(t, org, sourceKey, 2500, &divergentPSP{absent: true})
	db, ctx := refundDB(t)

	res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-1","amount":1250,"currency":"EUR"}`)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", res.Code, res.Body.String())
	}
	assertLegStillBound(t, db, ctx, org)
}

// The agreement twin for the leg path, asserting the persisted column.
func TestRefundLegRecordsTheProvidersOwnReturnedFigure(t *testing.T) {
	org, sourceKey := uuid.New(), "leg-conf-ok"
	h := divergentRefundServer(t, org, sourceKey, 2500, &divergentPSP{})
	db, ctx := refundDB(t)

	res := postRefundLeg(t, h, `{"organizer_id":"`+org.String()+`","idempotency_key":"`+sourceKey+`","refund_key":"refund-1","amount":1250,"currency":"EUR"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", res.Code, res.Body.String())
	}
	var amount sql.NullInt64
	var currency sql.NullString
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT confirmed_amount,confirmed_currency,status FROM payment_refund_legs WHERE organizer_id=$1`, org).
		Scan(&amount, &currency, &status); err != nil {
		t.Fatal(err)
	}
	if status != "refunded" {
		t.Fatalf("leg status = %q, want refunded", status)
	}
	if !amount.Valid || amount.Int64 != 1250 || currency.String != "EUR" {
		t.Fatalf("confirmed refund money not persisted: amount=%v currency=%v", amount, currency)
	}
}

// --- The whole-refund compensation path: the fourth sink ---
//
// This one is RECOVERY's, driven by commerce's runner with no human in the loop, and it is
// the sink the ticket text never named — found by the plan draft. It appends
// payment.refunded carrying the basis derived from the operation's durable evidence, so an
// unchecked disagreement journals a figure the provider never returned.

func postWholeRefund(t *testing.T, h http.Handler, org uuid.UUID, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/psp/refund",
		bytes.NewBufferString(`{"organizer_id":"`+org.String()+`","idempotency_key":"`+key+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", refundCredential)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

// assertCompensationStillBound re-reads the compensation row and the journal, for the same
// reason the leg assertions do: a 502 alone cannot distinguish a refusal that withheld the
// fact from one that appended it first.
func assertCompensationStillBound(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID) {
	t.Helper()
	var status sql.NullString
	var confirmed sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT status,confirmed_amount FROM payment_compensations WHERE organizer_id=$1 AND kind='refund'`, org).
		Scan(&status, &confirmed); err != nil {
		t.Fatal(err)
	}
	if status.Valid {
		t.Fatalf("a refused compensation completed as %q; it must stay bound and recoverable", status.String)
	}
	if confirmed.Valid {
		t.Fatalf("a refused compensation recorded confirmed money %d", confirmed.Int64)
	}
	var facts int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM journal_entries WHERE organizer_id=$1 AND fact_type='payment.refunded'`, org).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("a refused compensation appended %d payment.refunded facts into an append-only journal", facts)
	}
}

// PREDICATE 1: a different amount came back. Currency matches.
func TestWholeRefundFailsClosedWhenTheProviderReturnsADifferentAmount(t *testing.T) {
	org, key := uuid.New(), "comp-conf-amount"
	h := divergentRefundServer(t, org, key, 2500, &divergentPSP{amount: 2499})
	db, ctx := refundDB(t)

	res := postWholeRefund(t, h, org, key)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", res.Code, res.Body.String())
	}
	assertCompensationStillBound(t, db, ctx, org)
}

// PREDICATE 2: the amount matches, the currency does not.
func TestWholeRefundFailsClosedWhenTheProviderReturnsADifferentCurrency(t *testing.T) {
	org, key := uuid.New(), "comp-conf-currency"
	h := divergentRefundServer(t, org, key, 2500, &divergentPSP{currency: "USD"})
	db, ctx := refundDB(t)

	res := postWholeRefund(t, h, org, key)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", res.Code, res.Body.String())
	}
	assertCompensationStillBound(t, db, ctx, org)
}

// The agreement twin, asserting the PERSISTED column. Without it the whole-refund path could
// validate the confirmation and then discard it — which is exactly what it did until the
// adversarial review caught it: the guard was in place, the evidence was thrown away, and
// every disagreement test above still passed.
func TestWholeRefundRecordsTheProvidersOwnReturnedFigure(t *testing.T) {
	org, key := uuid.New(), "comp-conf-ok"
	h := divergentRefundServer(t, org, key, 2500, &divergentPSP{})
	db, ctx := refundDB(t)

	res := postWholeRefund(t, h, org, key)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", res.Code, res.Body.String())
	}
	var amount sql.NullInt64
	var currency sql.NullString
	var status sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT confirmed_amount,confirmed_currency,status FROM payment_compensations WHERE organizer_id=$1 AND kind='refund'`, org).
		Scan(&amount, &currency, &status); err != nil {
		t.Fatal(err)
	}
	if status.String != "refunded" {
		t.Fatalf("compensation status = %q, want refunded", status.String)
	}
	if !amount.Valid || amount.Int64 != 2500 || currency.String != "EUR" {
		t.Fatalf("the whole-refund path validated the provider's figure and then discarded it: amount=%v currency=%v", amount, currency)
	}
}
