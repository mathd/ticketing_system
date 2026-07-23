package psp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// All Stripe fixtures below are HAND-WRITTEN literal JSON — never marshalled from the
// adapter's own response structs. A fixture built from the type under test encodes the
// parser's own assumptions and cannot prove it parses a foreign Stripe payload (ADR-032,
// quality-practices §1). Each includes fields we do not parse (to prove the decoder
// tolerates unexpected keys) and omits fields that may be absent (latest_charge).

// A PaymentIntent confirmed with manual capture sits in requires_capture until captured.
const piRequiresCapture = `{
  "id": "pi_test_authonly",
  "object": "payment_intent",
  "amount": 1250,
  "amount_capturable": 1250,
  "amount_received": 0,
  "currency": "eur",
  "capture_method": "manual",
  "status": "requires_capture",
  "latest_charge": "ch_test_1",
  "some_future_field": {"nested": true},
  "metadata": {}
}`

// After capture, the PaymentIntent is succeeded.
const piSucceeded = `{
  "id": "pi_test_authonly",
  "object": "payment_intent",
  "amount": 1250,
  "amount_received": 1250,
  "currency": "eur",
  "status": "succeeded",
  "latest_charge": "ch_test_1"
}`

const cardDeclined = `{
  "error": {
    "code": "card_declined",
    "decline_code": "generic_decline",
    "message": "Your card was declined.",
    "type": "card_error"
  }
}`

// stripeStub is an httptest.Server recording the requests it received, so a test can assert
// the exact path/form/headers the adapter sent. handler maps "METHOD path" -> (status, body).
type stripeStub struct {
	srv      *httptest.Server
	requests []recordedReq
	routes   map[string]stubResp
}
type recordedReq struct {
	method, path, idempotencyKey, authUser string
	form                                   url.Values
}
type stubResp struct {
	status int
	body   string
}

func newStripeStub(t *testing.T, routes map[string]stubResp) *stripeStub {
	t.Helper()
	s := &stripeStub{routes: routes}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		user, _, _ := r.BasicAuth()
		s.requests = append(s.requests, recordedReq{
			method: r.Method, path: r.URL.Path,
			idempotencyKey: r.Header.Get("Idempotency-Key"), authUser: user, form: form,
		})
		key := r.Method + " " + r.URL.Path
		resp, ok := s.routes[key]
		if !ok {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"stub: no route for ` + key + `","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func newStripeForStub(stub *stripeStub) *Stripe {
	return NewStripe("sk_test_dummy", stub.srv.URL, stub.srv.Client())
}

// Authorize on the charge path does confirm-then-capture INTERNALLY (plan-final F2): the
// manual-capture PaymentIntent is created (requires_capture), then captured, so the charge
// handler observes Captured exactly as it did with the fake — no charge-handler change, no
// stranded auth. Two Stripe calls, one Result.
func TestStripeAuthorizeConfirmsThenCaptures(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents":                          {200, piRequiresCapture},
		"POST /v1/payment_intents/pi_test_authonly/capture": {200, piSucceeded},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa", IdempotencyKey: "idem-charge-1",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	want := Result{Outcome: Captured, Captured: true, Authorized: true, ProviderRef: "pi_test_authonly", ProviderChargeRef: "ch_test_1"}
	if got != want {
		t.Fatalf("Authorize\n got=%+v\nwant=%+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("result failed Validate: %v", err)
	}
	// Verify the wire: create carried lowercase currency, manual capture, confirm, the pm,
	// and the idempotency key; capture reused the same idempotency key derivation.
	if len(stub.requests) != 2 {
		t.Fatalf("want 2 Stripe calls (create+capture), got %d", len(stub.requests))
	}
	create := stub.requests[0]
	if create.form.Get("currency") != "eur" {
		t.Errorf("currency not lowercased outbound: %q", create.form.Get("currency"))
	}
	if create.form.Get("capture_method") != "manual" || create.form.Get("confirm") != "true" {
		t.Errorf("create missing manual/confirm: %v", create.form)
	}
	if create.form.Get("amount") != "1250" || create.form.Get("payment_method") != "pm_card_visa" {
		t.Errorf("create amount/pm wrong: %v", create.form)
	}
	if create.idempotencyKey == "" || create.authUser != "sk_test_dummy" {
		t.Errorf("create idem/auth wrong: idem=%q auth=%q", create.idempotencyKey, create.authUser)
	}
}

// A card decline on create is terminal-no-side-effect, mapped to Declined — unchanged
// charge-handler semantics (402).
func TestStripeAuthorizeDeclineIsTerminal(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents": {402, cardDeclined},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_chargeDeclined", IdempotencyKey: "idem-d",
	})
	if err != nil {
		t.Fatalf("decline should be a normal Result, not an error: %v", err)
	}
	if got.Outcome != Declined || !got.TerminalNoSideEffect {
		t.Fatalf("want Declined+terminal, got %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("decline result failed Validate: %v", err)
	}
	// A decline must NOT have attempted a capture.
	if len(stub.requests) != 1 {
		t.Fatalf("decline must not capture; got %d calls", len(stub.requests))
	}
}

// A transport failure (no HTTP response) is Unknown, never terminal — recovery must not
// release a claim on it (ADR-016 §Dec3). Simulated by pointing the adapter at a closed server.
func TestStripeAuthorizeTransportFailureIsUnknown(t *testing.T) {
	stub := newStripeStub(t, nil)
	baseURL := stub.srv.URL
	stub.srv.Close() // now connections fail
	s := NewStripe("sk_test_dummy", baseURL, http.DefaultClient)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm", IdempotencyKey: "idem-t",
	})
	if err == nil {
		t.Fatal("transport failure should return a non-nil error")
	}
	if got.Outcome != Unknown || got.TerminalNoSideEffect {
		t.Fatalf("want Unknown+non-terminal, got %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("unknown result failed Validate: %v", err)
	}
}

// Refund maps a succeeded Stripe refund to the Refunded outcome (money moved back, NOT
// terminal-no-side-effect). Uses the payment_intent ref.
func TestStripeRefundSucceeded(t *testing.T) {
	const refundOK = `{"id":"re_test_1","object":"refund","amount":1250,"currency":"eur","status":"succeeded","payment_intent":"pi_test_authonly","charge":"ch_test_1"}`
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/refunds": {200, refundOK}})
	s := newStripeForStub(stub)
	got, err := s.Refund(context.Background(), "pi_test_authonly", "psp-comp-v1:deadbeef", 1250, "EUR")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got.Outcome != Refunded || got.TerminalNoSideEffect || got.ProviderRef != "re_test_1" {
		t.Fatalf("want Refunded+non-terminal+re_, got %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("refund result failed Validate: %v", err)
	}
	req := stub.requests[0]
	if req.form.Get("payment_intent") != "pi_test_authonly" || req.form.Get("amount") != "1250" {
		t.Errorf("refund form wrong: %v", req.form)
	}
	if req.idempotencyKey != "psp-comp-v1:deadbeef" {
		t.Errorf("refund idempotency key not passed through: %q", req.idempotencyKey)
	}
}

// A refund still pending must NOT be reported as Refunded (the journal is append-only; a
// later 'failed' can't be retracted — plan-final A7). Pending surfaces as a non-terminal
// error so the caller keeps the compensation bound and retries.
func TestStripeRefundPendingIsNotDone(t *testing.T) {
	const refundPending = `{"id":"re_test_2","object":"refund","amount":1250,"currency":"eur","status":"pending","payment_intent":"pi_x"}`
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/refunds": {200, refundPending}})
	s := newStripeForStub(stub)
	got, err := s.Refund(context.Background(), "pi_x", "k", 1250, "EUR")
	if err == nil {
		t.Fatal("a pending refund must be a non-terminal error, not a Refunded Result")
	}
	if got.Outcome == Refunded {
		t.Fatalf("pending must not map to Refunded: %+v", got)
	}
}

// Void cancels an uncaptured authorization -> Voided (terminal-no-side-effect: nothing moved).
func TestStripeVoidSucceeded(t *testing.T) {
	const canceled = `{"id":"pi_test_authonly","object":"payment_intent","status":"canceled","currency":"eur"}`
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents/pi_test_authonly/cancel": {200, canceled}})
	s := newStripeForStub(stub)
	got, err := s.Void(context.Background(), "pi_test_authonly", "psp-comp-v1:void1")
	if err != nil {
		t.Fatalf("Void: %v", err)
	}
	if got.Outcome != Voided || !got.TerminalNoSideEffect {
		t.Fatalf("want Voided+terminal, got %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("void result failed Validate: %v", err)
	}
}

// Status maps requires_capture->Authorized and succeeded->Captured via GET retrieve.
func TestStripeStatusMapsState(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{"GET /v1/payment_intents/pi_test_authonly": {200, piRequiresCapture}})
	s := newStripeForStub(stub)
	got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "pi_test_authonly"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Outcome != Authorized {
		t.Fatalf("want Authorized from requires_capture, got %+v", got)
	}
}

// Status with no ProviderRef replays the original create under the SAME idempotency key
// (crash-safe replay, ADR-032 §Status/replay) — never a fresh key, never a duplicate charge.
func TestStripeStatusReplaysUnderSameKey(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents": {200, piRequiresCapture}})
	s := newStripeForStub(stub)
	_, err := s.Status(context.Background(), StatusRequest{
		ProviderRef: "", IdempotencyKey: "idem-original", Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa",
	})
	if err != nil {
		t.Fatalf("Status replay: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0].method != http.MethodPost {
		t.Fatalf("replay should POST create, got %+v", stub.requests)
	}
	if stub.requests[0].idempotencyKey != "idem-original" {
		t.Fatalf("replay must reuse the ORIGINAL idempotency key, got %q", stub.requests[0].idempotencyKey)
	}
	if strings.HasSuffix(stub.requests[0].path, "/capture") {
		t.Fatal("replay must not capture — it only re-creates to learn the outcome")
	}
}

// Sanity: the JSON shapes we hand-write actually parse (guards against a typo'd fixture).
func TestFixturesParse(t *testing.T) {
	for _, f := range []string{piRequiresCapture, piSucceeded, cardDeclined} {
		var m map[string]any
		if err := json.Unmarshal([]byte(f), &m); err != nil {
			t.Fatalf("fixture does not parse: %v", err)
		}
	}
}

// The charge reference (ch_) rides latest_charge on the captured PaymentIntent; the
// adapter surfaces it as ProviderChargeRef so the store can persist both pi_ and ch_
// (ADR-032 §provider reference identity). A PaymentIntent without latest_charge (or
// null) simply yields an empty ChargeRef — never an error.
func TestStripeCaptureExtractsChargeRef(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents":                          {200, piRequiresCapture},
		"POST /v1/payment_intents/pi_test_authonly/capture": {200, piSucceeded},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa", IdempotencyKey: "idem-ch-1",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.ProviderChargeRef != "ch_test_1" {
		t.Fatalf("want ProviderChargeRef ch_test_1 from latest_charge, got %q", got.ProviderChargeRef)
	}
	// null latest_charge: hand-written literal with explicit null — parses, empty ref.
	const piNullCharge = `{"id":"pi_nl","object":"payment_intent","status":"succeeded","currency":"eur","latest_charge":null}`
	stub2 := newStripeStub(t, map[string]stubResp{"GET /v1/payment_intents/pi_nl": {200, piNullCharge}})
	s2 := newStripeForStub(stub2)
	got2, err := s2.Status(context.Background(), StatusRequest{ProviderRef: "pi_nl"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got2.ProviderChargeRef != "" {
		t.Fatalf("null latest_charge must yield empty ChargeRef, got %q", got2.ProviderChargeRef)
	}
}

// 429 and 5xx mean the request may have reached Stripe: ambiguous, never terminal, never
// Declined (plan-final A1/A2, ADR-016 §Dec3).
func TestStripeRateLimitAndServerErrorsAreUnknown(t *testing.T) {
	for name, resp := range map[string]stubResp{
		"rate limited": {429, `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`},
		"server error": {500, `{"error":{"message":"boom","type":"api_error"}}`},
		"bad gateway no envelope": {502, `<html>bad gateway</html>`},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents": resp})
			s := newStripeForStub(stub)
			got, err := s.Authorize(context.Background(), AuthorizeRequest{
				Amount: 100, Currency: "EUR", PaymentToken: "pm", IdempotencyKey: "idem-u",
			})
			if err == nil {
				t.Fatal("ambiguous provider failure must surface an error")
			}
			if got.Outcome != Unknown || got.TerminalNoSideEffect {
				t.Fatalf("want Unknown+non-terminal, got %+v", got)
			}
		})
	}
}

// A capture-stage failure is NEVER terminal Declined: createIntent already succeeded, so a
// live authorization exists at Stripe holding the customer's funds. Mapping it Declined
// would record "no side effect" while money is held AND drop the pi_ identity needed to
// void it (ai-review A1). It must stay Unknown, with the ProviderRef preserved.
func TestStripeCaptureFailureIsUnknownAndKeepsProviderRef(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents":                          {200, piRequiresCapture},
		"POST /v1/payment_intents/pi_test_authonly/capture": {402, cardDeclined},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa", IdempotencyKey: "idem-capfail",
	})
	if err == nil {
		t.Fatal("a failed capture of a live authorization must surface an error")
	}
	if got.Outcome != Unknown || got.TerminalNoSideEffect {
		t.Fatalf("capture failure must be Unknown+non-terminal (a hold exists), got %+v", got)
	}
	if got.ProviderRef != "pi_test_authonly" {
		t.Fatalf("capture failure must preserve the pi_ identity for recovery, got %q", got.ProviderRef)
	}
}

// Status dispatches a re_ provider ref to the refund object (ai-review B3): a pending
// refund is resolved by RETRIEVING it — re-POSTing /v1/refunds under the same idempotency
// key would replay Stripe's original "pending" snapshot forever (and after key expiry,
// mint a SECOND refund). succeeded -> Refunded; pending -> ErrRefundPending.
func TestStripeStatusResolvesRefundRef(t *testing.T) {
	const refundDone = `{"id":"re_test_9","object":"refund","amount":1250,"currency":"eur","status":"succeeded","payment_intent":"pi_x","charge":"ch_x"}`
	stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds/re_test_9": {200, refundDone}})
	s := newStripeForStub(stub)
	got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "re_test_9"})
	if err != nil {
		t.Fatalf("Status(re_): %v", err)
	}
	if got.Outcome != Refunded || got.ProviderRef != "re_test_9" {
		t.Fatalf("want Refunded re_test_9, got %+v", got)
	}
	const refundStillPending = `{"id":"re_test_10","object":"refund","amount":1250,"currency":"eur","status":"pending"}`
	stub2 := newStripeStub(t, map[string]stubResp{"GET /v1/refunds/re_test_10": {200, refundStillPending}})
	s2 := newStripeForStub(stub2)
	got2, err := s2.Status(context.Background(), StatusRequest{ProviderRef: "re_test_10"})
	if !errors.Is(err, ErrRefundPending) {
		t.Fatalf("pending refund retrieve must be ErrRefundPending, got %v", err)
	}
	if got2.Outcome == Refunded {
		t.Fatalf("pending must not map to Refunded: %+v", got2)
	}
}
