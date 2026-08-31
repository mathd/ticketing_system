//go:build smoke

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// TKT-298. Two halves of the status endpoint's confirmation contract, at the tier that
// decides them.
//
// COS2 — the status resolver's fail-closed guard still refuses a genuinely unconfirmed
// Captured answer. This ticket made the FAKE confirm its success token, so the guard must
// be shown still standing against a provider that does not: the risk in a fix like this is
// buying COS1 by making the guard unreachable rather than by making the simulator honest.
//
// COS4 — a refund-superseded status answer publishes the compensation's own stored
// confirmation, which it previously dropped on the floor.
//
// TIER (AGENTS.md). Both are asserted here, at the HTTP handler, because both COS name a
// STATUS CODE and a RESPONSE BODY, and `pspStatus` is where those are decided. `statusBody`
// is the response boundary; a store-level test would prove the row holds the figure and say
// nothing about whether the endpoint publishes it, which is the entire finding.

// statusDivergentPSP answers a status REPLAY with money that does not match the operation's
// durable request. It exists because divergentPSP shadows Authorize and Refund only, and
// the status resolver is a different sink with its own guard (api/psp.go).
type statusDivergentPSP struct {
	psp.PSP
	amount   int64
	currency string
	// absent reports NO confirmation — silence, which the guard must not read as assent.
	// This is the case the fake produced for `fake-ok` before this ticket, and the reason
	// the offline stack could never resolve its own success token.
	absent bool
}

func (d *statusDivergentPSP) Status(_ context.Context, req psp.StatusRequest) (psp.Result, error) {
	res := psp.Result{Outcome: psp.Captured, Captured: true, Authorized: true, ProviderRef: "pi_statusdivergent"}
	if d.absent {
		return res, nil
	}
	c := psp.ConfirmedMoney{Amount: req.Amount, Currency: req.Currency}
	if d.amount != 0 {
		c.Amount = d.amount
	}
	if d.currency != "" {
		c.Currency = d.currency
	}
	res.Confirmed = &c
	return res, nil
}

// seedUnresolvedCharge writes the row a crash between bind and complete leaves behind:
// BOUND, `status IS NULL`, no fact, carrying the durable request the status replay is
// resolved against. This is the precondition of the whole ticket and no fixture in the
// suite constructed it before — which is why the gap was invisible to the gate.
func seedUnresolvedCharge(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID, key, token string, amount int64, currency string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,order_id,buyer_id,
		                               request_amount,request_currency,payment_method_ref)
		VALUES($1,$2,'fingerprint',$3,$4,$5,$6,$7)`,
		org, key, uuid.New(), uuid.New(), amount, currency, token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_compensations WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})
}

func statusRequest(t *testing.T, h http.Handler, org uuid.UUID, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/internal/psp/status?organizer_id="+org.String()+"&idempotency_key="+key, nil)
	req.Header.Set("X-Internal-Token", refundCredential)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// COS1's unit-tier half: the OFFLINE stack resolves its own success token. The composed
// end-to-end proof (order does not park) is smoke/psp_recovery_test.go; this one pins the
// payments-side answer that proof depends on, at the handler that produces it.
//
// Before this ticket the fake answered Captured with no confirmation, the guard below
// refused it, and this returned 502 forever.
func TestStatusResolvesAnUnresolvedFakeOKCharge(t *testing.T) {
	db, ctx := refundDB(t)
	org, key := uuid.New(), "status-fakeok-"+uuid.NewString()
	const amount, currency = 5600, "EUR"
	seedUnresolvedCharge(t, db, ctx, org, key, "fake-ok", amount, currency)

	rec := statusRequest(t, refundServerWithPSP(t, db, psp.NewFake()), org, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("an unresolved fake-ok charge must resolve, got %d %s: the offline stack "+
			"cannot status-resolve its own success token", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if body["outcome"] != "captured" {
		t.Fatalf("outcome = %v, want captured: %s", body["outcome"], rec.Body.String())
	}
	// The evidence must be PUBLISHED, not merely accepted. Commerce reads only this
	// endpoint, so a 200 that dropped the figure would resolve the operation and still
	// leave the caller unable to see what the provider confirmed.
	if got := body["confirmed_captured_amount"]; got != float64(amount) {
		t.Fatalf("confirmed_captured_amount = %v, want %d: %s", got, amount, rec.Body.String())
	}
	if got := body["confirmed_currency"]; got != currency {
		t.Fatalf("confirmed_currency = %v, want %q: %s", got, currency, rec.Body.String())
	}
	// And it landed durably, so a later read answers from stored truth without replaying.
	var confAmt sql.NullInt64
	var confCur sql.NullString
	var state sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT confirmed_captured_amount,confirmed_currency,provider_state FROM payment_operations
		 WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).Scan(&confAmt, &confCur, &state); err != nil {
		t.Fatal(err)
	}
	if !confAmt.Valid || confAmt.Int64 != amount || confCur.String != currency {
		t.Fatalf("stored confirmation = (%v,%d,%q), want (true,%d,%q)",
			confAmt.Valid, confAmt.Int64, confCur.String, amount, currency)
	}
	if state.String != "captured" {
		t.Fatalf("provider_state = %q, want captured", state.String)
	}
}

// COS2. The guard is unchanged and must still refuse every way a provider can fail to
// confirm a capture. One case per predicate, each satisfying the others so it goes red only
// when its OWN comparison is removed — the same discipline the charge/refund divergence
// tests in provider_confirmation_smoke_test.go already follow.
func TestStatusStillFailsClosedOnAnUnconfirmedCapture(t *testing.T) {
	for _, tc := range []struct {
		name string
		psp  *statusDivergentPSP
	}{
		// ABSENT: silence is not assent. This is the case the fake used to produce, and it
		// is why the fix had to be in the fake rather than in the guard: had this ticket
		// exempted the offline provider instead, this case would now pass and the whole
		// fail-closed property would be gone for one implementation.
		{"no confirmation at all", &statusDivergentPSP{absent: true}},
		// AMOUNT: currency matches, so only the amount comparison can refuse this.
		{"a different amount", &statusDivergentPSP{amount: 5599}},
		// CURRENCY: amount matches, so only the currency comparison can refuse this.
		{"a different currency", &statusDivergentPSP{currency: "USD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := refundDB(t)
			org, key := uuid.New(), "status-diverge-"+uuid.NewString()
			const amount, currency = 5600, "EUR"
			// A token the FAKE cannot resolve, so the answer under test can only come from
			// the stub. With `fake-ok` the embedded fake would answer first for any method
			// the stub failed to shadow, and the test would pass without the guard.
			seedUnresolvedCharge(t, db, ctx, org, key, "pm_divergent", amount, currency)

			rec := statusRequest(t, refundServerWithPSP(t, db, tc.psp), org, key)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("a Captured resolution the provider did not confirm must be refused 502, got %d %s",
					rec.Code, rec.Body.String())
			}
			// The refusal must leave the operation exactly as recoverable as it was
			// (ADR-016 §Dec3). A 502 that had already recorded the disputed figure would be
			// the defect with an error message on top — and the response code alone cannot
			// tell the two apart, which is why this re-reads the row.
			var status, confCur sql.NullString
			var confAmt sql.NullInt64
			if err := db.QueryRowContext(ctx,
				`SELECT status,confirmed_captured_amount,confirmed_currency FROM payment_operations
				 WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).Scan(&status, &confAmt, &confCur); err != nil {
				t.Fatal(err)
			}
			if status.Valid {
				t.Fatalf("a refused resolution resolved the operation to %q; it must stay recoverable",
					status.String)
			}
			if confAmt.Valid || confCur.Valid {
				t.Fatalf("a refused resolution recorded confirmed money (%d,%q)", confAmt.Int64, confCur.String)
			}
		})
	}
}

// COS4. A completed whole-refund compensation supersedes the operation's terminal status,
// and that answer must carry the REFUND's stored confirmation — what the provider said it
// returned — rather than nil.
//
// The seeded figures deliberately differ from the operation's captured amount. Seeding them
// equal would make the assertion pass against a handler that published the OPERATION's
// confirmation instead, which is a different value that happens to coincide.
func TestRefundSupersededStatusPublishesTheStoredRefundConfirmation(t *testing.T) {
	db, ctx := refundDB(t)
	org, key := uuid.New(), "status-refunded-"+uuid.NewString()
	const captured, refunded, currency = 5600, 4200, "EUR"

	seedUnresolvedCharge(t, db, ctx, org, key, "fake-ok", captured, currency)
	if _, err := db.ExecContext(ctx, `
		UPDATE payment_operations SET status='captured',fact_id=$3,provider_state='captured',
		    authorized_amount=$4,captured_amount=$4,confirmed_captured_amount=$4,confirmed_currency=$5,
		    provider_state_at=now()
		WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key, uuid.New(), int64(captured), currency); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,
		                                  status,provider_ref,fact_id,amount,currency,completed_at,
		                                  confirmed_amount,confirmed_currency)
		VALUES($1,$2,'refund',$3,'refunded','re_test',$4,$5,$6,now(),$7,$6)`,
		org, key, "provkey-"+key, uuid.New(), int64(captured), currency, int64(refunded)); err != nil {
		t.Fatal(err)
	}

	rec := statusRequest(t, refundServerWithPSP(t, db, psp.NewFake()), org, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if body["outcome"] != "refunded" {
		t.Fatalf("outcome = %v, want refunded: %s", body["outcome"], rec.Body.String())
	}
	// Under confirmed_REFUNDED_amount, not confirmed_captured_amount (ai-review [medium]).
	// The two confirm opposite movements, and the schema defines the capture field as what
	// the provider reported CAPTURING — publishing a refund's figure there would hand a
	// reconciliation consumer refund evidence under a capture-evidence name with nothing in
	// the payload to tell them apart.
	if got := body["confirmed_refunded_amount"]; got != float64(refunded) {
		t.Fatalf("confirmed_refunded_amount = %v, want the REFUND's stored %d: %s",
			got, refunded, rec.Body.String())
	}
	if _, present := body["confirmed_captured_amount"]; present {
		t.Fatalf("a refunded answer must NOT publish a capture confirmation (the capture was "+
			"reversed, and %d is not what the provider captured): %s", captured, rec.Body.String())
	}
	if got := body["confirmed_currency"]; got != currency {
		t.Fatalf("confirmed_currency = %v, want %q: %s", got, currency, rec.Body.String())
	}
}

// The legacy half of COS4 (ai-review [medium]): a whole-refund compensation completed BEFORE
// payments migration 0006 carries no confirmation and never can. Its superseding answer must
// omit the key entirely rather than publish a zero — `confirmed_refunded_amount: 0` reads as
// "the provider confirmed returning nothing", which is a claim about money, not an absence.
//
// This is the case the nil-check in confirmedCompensation exists for, and without a test the
// check is an untested guarantee.
func TestRefundSupersededStatusOmitsAMissingLegacyConfirmation(t *testing.T) {
	db, ctx := refundDB(t)
	org, key := uuid.New(), "status-refunded-legacy-"+uuid.NewString()
	const captured, currency = 5600, "EUR"

	seedUnresolvedCharge(t, db, ctx, org, key, "fake-ok", captured, currency)
	if _, err := db.ExecContext(ctx, `
		UPDATE payment_operations SET status='captured',fact_id=$3,provider_state='captured',
		    authorized_amount=$4,captured_amount=$4,provider_state_at=now()
		WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key, uuid.New(), int64(captured)); err != nil {
		t.Fatal(err)
	}
	// Completed, with confirmed_amount/confirmed_currency left NULL: the pre-0006 shape.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,
		                                  status,provider_ref,fact_id,amount,currency,completed_at)
		VALUES($1,$2,'refund',$3,'refunded','re_legacy',$4,$5,$6,now())`,
		org, key, "provkey-"+key, uuid.New(), int64(captured), currency); err != nil {
		t.Fatal(err)
	}

	rec := statusRequest(t, refundServerWithPSP(t, db, psp.NewFake()), org, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if body["outcome"] != "refunded" {
		t.Fatalf("outcome = %v, want refunded: %s", body["outcome"], rec.Body.String())
	}
	if _, present := body["confirmed_refunded_amount"]; present {
		t.Fatalf("a legacy refund has no confirmation and must publish none, got %s", rec.Body.String())
	}
	if _, present := body["confirmed_currency"]; present {
		t.Fatalf("a legacy refund must publish no confirmed currency, got %s", rec.Body.String())
	}
}

// The other half of COS4, and it is not decoration: a void moves no money, so its
// superseding answer must publish NO confirmation. CompleteCompensation refuses a
// confirmation on a void, so the column is guaranteed NULL — publishing one would report a
// value the store forbids writing, and `confirmed_captured_amount: 0` would read as
// "the provider confirmed zero" rather than "nothing was confirmed".
func TestVoidSupersededStatusPublishesNoConfirmation(t *testing.T) {
	db, ctx := refundDB(t)
	org, key := uuid.New(), "status-voided-"+uuid.NewString()
	const amount, currency = 5600, "EUR"

	seedUnresolvedCharge(t, db, ctx, org, key, "fake-auth-hold", amount, currency)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,
		                                  status,provider_ref,fact_id,amount,currency,completed_at)
		VALUES($1,$2,'void',$3,'voided','',$4,$5,$6,now())`,
		org, key, "provkey-"+key, uuid.New(), int64(amount), currency); err != nil {
		t.Fatal(err)
	}

	rec := statusRequest(t, refundServerWithPSP(t, db, psp.NewFake()), org, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if body["outcome"] != "voided" {
		t.Fatalf("outcome = %v, want voided: %s", body["outcome"], rec.Body.String())
	}
	if _, present := body["confirmed_captured_amount"]; present {
		t.Fatalf("a void must publish no confirmation, got %s", rec.Body.String())
	}
	if _, present := body["confirmed_currency"]; present {
		t.Fatalf("a void must publish no confirmed currency, got %s", rec.Body.String())
	}
}

// refundServerWithPSP wires the real store and router against an arbitrary PSP, mirroring
// divergentChargeServer's shape. The provided PSP is used as-is: a stub that shadows only
// some methods must embed psp.PSP itself and be given a delegate by its own test.
func refundServerWithPSP(t *testing.T, db *sql.DB, p psp.PSP) http.Handler {
	t.Helper()
	ring, err := store.NewKeyring("refund-key", []byte("payments-refund-leg-key-0123456"), "")
	if err != nil {
		t.Fatal(err)
	}
	return NewWithPSP(store.New(db, ring), refundCredential, p).Router(nil, true)
}
