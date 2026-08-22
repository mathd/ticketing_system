package psp

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TKT-257: the Stripe adapter must carry the provider's OWN monetary figure into the
// Result, instead of leaving payments to record the amount it asked for.
//
// Every fixture below is HAND-WRITTEN literal JSON, per this package's standing rule
// (stripe_test.go header): a fixture marshalled from stripePI/stripeRefund would encode the
// parser's own field set and could not prove the adapter reads a foreign Stripe payload.
// That rule is load-bearing here specifically — the defect this ticket closes is a struct
// that never declared `amount_received`, which is exactly what a self-marshalled fixture
// would have hidden.

// A capture whose amount_received DISAGREES with what was requested. This shape cannot be
// produced by the fake PSP at all (it has no amounts), which is why every existing test in
// this repo is green against a live defect.
const piSucceededShort = `{
  "id": "pi_test_authonly",
  "object": "payment_intent",
  "amount": 1250,
  "amount_received": 1200,
  "currency": "eur",
  "status": "succeeded",
  "latest_charge": "ch_test_1"
}`

// A succeeded capture in a currency other than the one requested.
const piSucceededOtherCurrency = `{
  "id": "pi_test_authonly",
  "object": "payment_intent",
  "amount": 1250,
  "amount_received": 1250,
  "currency": "usd",
  "status": "succeeded",
  "latest_charge": "ch_test_1"
}`

// A succeeded PaymentIntent that reports NO amount_received at all. Absence is a real state
// and must stay distinguishable from a confirmed zero.
const piSucceededNoAmount = `{
  "id": "pi_test_authonly",
  "object": "payment_intent",
  "amount": 1250,
  "currency": "eur",
  "status": "succeeded",
  "latest_charge": "ch_test_1"
}`

// The adapter must read Stripe's own figure into the Result. The mutation this catches is
// the shipped defect itself: `stripePI` never declared `amount_received`, so the value
// arrived on the wire and was dropped.
//
// Note this cannot be caught by TestFixturesParse — the fixtures ALREADY carried
// `amount_received` while the struct ignored it, so that test was green throughout.
func TestStripeCaptureCarriesAmountReceived(t *testing.T) {
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
	if got.Confirmed == nil {
		t.Fatal("a succeeded capture must carry the provider's amount_received, got none")
	}
	if got.Confirmed.Amount != 1250 || got.Confirmed.Currency != "EUR" {
		t.Fatalf("confirmed money = %+v, want 1250 EUR from the PaymentIntent", got.Confirmed)
	}
	// Uppercased on the way in, matching the schema's ^[A-Z]{3}$ and every other money
	// value payments stores. Stripe speaks lowercase.
	if err := got.Validate(); err != nil {
		t.Fatalf("result failed Validate: %v", err)
	}
}

// The disagreement itself must survive into the Result rather than being normalized away in
// the adapter. The adapter REPORTS; the write sinks REFUSE. Splitting it the other way would
// put the money decision in the provider adapter, where each new provider re-implements it.
func TestStripeCaptureReportsADisagreeingAmount(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents":                          {200, piRequiresCapture},
		"POST /v1/payment_intents/pi_test_authonly/capture": {200, piSucceededShort},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa", IdempotencyKey: "idem-charge-2",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Confirmed == nil || got.Confirmed.Amount != 1200 {
		t.Fatalf("the adapter must report Stripe's 1200, not the requested 1250: %+v", got.Confirmed)
	}
	if got.Confirmed.Agrees(1250, "EUR") {
		t.Fatal("1200 EUR must not agree with a request for 1250 EUR")
	}
}

func TestStripeCaptureReportsADisagreeingCurrency(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents":                          {200, piRequiresCapture},
		"POST /v1/payment_intents/pi_test_authonly/capture": {200, piSucceededOtherCurrency},
	})
	s := newStripeForStub(stub)
	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm_card_visa", IdempotencyKey: "idem-charge-3",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	// The amount MATCHES here. This case passes the amount predicate so it can reach the
	// currency one — a case that failed both would be silent about which guard caught it.
	if got.Confirmed == nil || got.Confirmed.Amount != 1250 || got.Confirmed.Currency != "USD" {
		t.Fatalf("want the provider's 1250 USD reported verbatim, got %+v", got.Confirmed)
	}
	if got.Confirmed.Agrees(1250, "EUR") {
		t.Fatal("1250 USD must not agree with a request for 1250 EUR")
	}
}

// Stripe reports `amount_received: 0` on a requires_capture PaymentIntent — a REAL zero,
// not a missing value. An authorization has moved no money, so it must carry NO
// confirmation; manufacturing a confirmed zero from it would settle an authorization as a
// capture of nothing, and would also make `Validate` reject a structurally fine result.
func TestStripeAuthorizationDoesNotManufactureAConfirmedZero(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"GET /v1/payment_intents/pi_test_authonly": {200, piRequiresCapture},
	})
	s := newStripeForStub(stub)
	got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "pi_test_authonly"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Outcome != Authorized {
		t.Fatalf("requires_capture must map to Authorized, got %s", got.Outcome)
	}
	if got.Confirmed != nil {
		t.Fatalf("an authorization moved no money and must carry no confirmation, got %+v", got.Confirmed)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("result failed Validate: %v", err)
	}
}

// A provider answer that omits the figure entirely is ABSENT, never zero. The write sinks
// then refuse to settle on it, because `(*ConfirmedMoney)(nil).Agrees` is false.
func TestStripeCaptureWithoutAnAmountReportsAbsentNotZero(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{
		"GET /v1/payment_intents/pi_test_authonly": {200, piSucceededNoAmount},
	})
	s := newStripeForStub(stub)
	got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "pi_test_authonly"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Outcome != Captured {
		t.Fatalf("succeeded must map to Captured, got %s", got.Outcome)
	}
	if got.Confirmed != nil {
		t.Fatalf("an omitted amount_received must be absent, not zero: %+v", got.Confirmed)
	}
	if got.Confirmed.Agrees(1250, "EUR") {
		t.Fatal("absent must never agree; silence is not assent")
	}
}

// The refund side already PARSED amount/currency — `classify` compares `rf.Amount != amount`
// for replay matching — and `mapRefundStatus` dropped both on the floor. This catches that
// exact drop.
func TestStripeRefundCarriesReturnedMoney(t *testing.T) {
	const refundOK = `{"id":"re_test_1","object":"refund","amount":1250,"currency":"eur","status":"succeeded","payment_intent":"pi_test_authonly","charge":"ch_test_1"}`
	stub := newStripeStub(t, map[string]stubResp{
		"GET /v1/refunds":  {200, refundListEmpty},
		"POST /v1/refunds": {200, refundOK},
	})
	s := newStripeForStub(stub)
	got, err := s.Refund(context.Background(), "pi_test_authonly", "psp-comp-v1:deadbeef", 1250, "EUR")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got.Confirmed == nil || got.Confirmed.Amount != 1250 || got.Confirmed.Currency != "EUR" {
		t.Fatalf("a settled refund must carry the provider's returned money, got %+v", got.Confirmed)
	}
	if !got.Confirmed.Agrees(1250, "EUR") {
		t.Fatal("a refund of exactly what was asked must agree")
	}
}

// `mapRefundStatus` has THREE callers: the Refund POST, resolveRefund's adoption path, and
// Status's re_ retrieve. Attaching the confirmation at the callers rather than inside
// mapRefundStatus would leave the two crash-recovery paths — the ones that matter most —
// carrying no confirmation. This test reaches the other two.
func TestStripeRefundConfirmationReachesEveryMapRefundStatusCaller(t *testing.T) {
	t.Run("adopted by resolution", func(t *testing.T) {
		stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds": {200, refundListSettled}})
		s := newStripeForStub(stub)
		got, err := s.Refund(context.Background(), "pi_test_authonly", "psp-comp-v1:deadbeef", 1250, "EUR")
		if err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if got.Outcome != Refunded {
			t.Fatalf("an adopted settled refund must be Refunded, got %s", got.Outcome)
		}
		if got.Confirmed == nil || got.Confirmed.Amount != 1250 || got.Confirmed.Currency != "EUR" {
			t.Fatalf("adopted refund carries no confirmation: %+v", got.Confirmed)
		}
		for _, r := range stub.requests {
			if r.method == http.MethodPost {
				t.Fatal("resolution adopted its own refund and must not submit a second")
			}
		}
	})

	t.Run("retrieved by Status", func(t *testing.T) {
		const refundRetrieved = `{"id":"re_lost_1","object":"refund","amount":1250,"currency":"eur","status":"succeeded","payment_intent":"pi_test_authonly"}`
		stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds/re_lost_1": {200, refundRetrieved}})
		s := newStripeForStub(stub)
		got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "re_lost_1"})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if got.Confirmed == nil || got.Confirmed.Amount != 1250 || got.Confirmed.Currency != "EUR" {
			t.Fatalf("a retrieved settled refund carries no confirmation: %+v", got.Confirmed)
		}
	})
}

// A PENDING refund must carry NO confirmation. Stripe's refund object has an `amount` while
// pending, but the money has not come back — attaching it would let an unsettled refund be
// recorded as settled evidence. `Validate` also refuses it, since pending maps to Unknown.
func TestStripePendingRefundCarriesNoConfirmedMoney(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds": {200, refundListPending}})
	s := newStripeForStub(stub)
	got, err := s.Refund(context.Background(), "pi_test_authonly", "psp-comp-v1:deadbeef", 1250, "EUR")
	if !errors.Is(err, ErrRefundPending) {
		t.Fatalf("want ErrRefundPending, got %v", err)
	}
	if got.Confirmed != nil {
		t.Fatalf("pending money has not moved and must carry no confirmation: %+v", got.Confirmed)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("a pending result must still be structurally valid: %v", err)
	}
}

// A FAILED refund likewise moved no money.
func TestStripeFailedRefundCarriesNoConfirmedMoney(t *testing.T) {
	stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds": {200, refundListFailed}})
	s := newStripeForStub(stub)
	got, _ := s.Refund(context.Background(), "pi_test_authonly", "psp-comp-v1:deadbeef", 1250, "EUR")
	if got.Confirmed != nil {
		t.Fatalf("a failed refund moved no money: %+v", got.Confirmed)
	}
}

// A void released a hold; nothing moved on the ledger, so no confirmation.
func TestStripeVoidCarriesNoConfirmedMoney(t *testing.T) {
	const piCanceled = `{"id":"pi_test_authonly","object":"payment_intent","status":"canceled","amount":1250,"amount_received":0,"currency":"eur"}`
	stub := newStripeStub(t, map[string]stubResp{
		"POST /v1/payment_intents/pi_test_authonly/cancel": {200, piCanceled},
	})
	s := newStripeForStub(stub)
	got, err := s.Void(context.Background(), "pi_test_authonly", "psp-comp-v1:void")
	if err != nil {
		t.Fatalf("Void: %v", err)
	}
	if got.Confirmed != nil {
		t.Fatalf("a void moved no money: %+v", got.Confirmed)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("result failed Validate: %v", err)
	}
}
