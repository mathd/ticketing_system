package psp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ticketing/shared/httpx"
)

// stripeCallTimeout bounds how LONG a provider call may take. It says nothing about
// how MANY bytes come back, so before maxStripeResponseBytes a provider streaming
// steadily inside its deadline allocated without a ceiling on the money path.
//
// The boundary is tested as a boundary. A padded PaymentIntent of exactly the limit
// must still be parsed and must still produce the normal terminal outcome; one byte
// more must be refused. Testing only a hugely oversized body would pass against an
// off-by-one ceiling, and testing only the refusal would not notice a limit that had
// begun rejecting legitimate payloads.
//
// The padding rides in a field the adapter does not parse, so size is the ONLY
// variable between the two cases: if the accepted case went red, it would be about
// the ceiling and not about a fixture the decoder stopped understanding.
func paddedPI(t *testing.T, total int) string {
	t.Helper()
	const head = `{"id":"pi_test_authonly","object":"payment_intent","status":"succeeded","latest_charge":"ch_test_1","padding":"`
	const tail = `"}`
	if n := total - len(head) - len(tail); n >= 0 {
		return head + strings.Repeat("x", n) + tail
	}
	t.Fatalf("padded fixture cannot be shrunk to %d bytes", total)
	return ""
}

func TestStripeAcceptsAResponseOfExactlyTheLimit(t *testing.T) {
	body := paddedPI(t, int(maxStripeResponseBytes))
	if int64(len(body)) != maxStripeResponseBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(body), maxStripeResponseBytes)
	}
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents": {200, body}})
	s := newStripeForStub(stub)

	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm", IdempotencyKey: "idem-at-limit",
	})
	if err != nil {
		t.Fatalf("a response of exactly the limit must be read: %v", err)
	}
	if got.Outcome != Captured {
		t.Fatalf("want Captured from the padded succeeded intent, got %+v", got)
	}
}

// One byte over. The required direction is ambiguity, NOT a terminal outcome: a body
// the adapter would not read is a body it cannot classify, and inventing Declined or
// Voided from one is how a captured payment is recorded as a failure (ADR-016 §Dec3).
// So this asserts the outcome and TerminalNoSideEffect, not merely that an error came
// back — a refusal mapped to Declined would also return an error.
func TestStripeRefusesAResponseOneByteOverTheLimitAsUnknown(t *testing.T) {
	body := paddedPI(t, int(maxStripeResponseBytes)+1)
	if int64(len(body)) != maxStripeResponseBytes+1 {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(body), maxStripeResponseBytes+1)
	}
	stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents": {200, body}})
	s := newStripeForStub(stub)

	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm", IdempotencyKey: "idem-over-limit",
	})
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
	if got.Outcome != Unknown {
		t.Fatalf("an unreadable provider response must be Unknown, got %+v", got)
	}
	if got.TerminalNoSideEffect {
		t.Fatal("an unreadable provider response must never be terminal: the charge may have succeeded")
	}
	// The adapter must not have proceeded to capture on an unclassifiable create.
	if len(stub.requests) != 1 {
		t.Fatalf("want 1 call, got %d", len(stub.requests))
	}
}

// The same ceiling on the READ paths, which is where recovery decides whether a claim
// may be released. Status is the call ADR-032 relies on when the idempotency key has
// aged out, so an oversize body here must leave the operation recoverable rather than
// resolve it.
func TestStripeStatusRefusesAnOversizeResponseAsUnknown(t *testing.T) {
	body := paddedPI(t, int(maxStripeResponseBytes)+1)
	stub := newStripeStub(t, map[string]stubResp{
		"GET /v1/payment_intents/pi_test_authonly": {200, body},
	})
	s := newStripeForStub(stub)

	got, err := s.Status(context.Background(), StatusRequest{ProviderRef: "pi_test_authonly"})
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
	if got.Outcome != Unknown || got.TerminalNoSideEffect {
		t.Fatalf("an unreadable status response must be Unknown and non-terminal, got %+v", got)
	}
}

// A NON-2xx oversize body must be refused too, and must NOT be classified from the
// error envelope the adapter could not read. classifyError maps a card_error to the
// only terminal outcome the adapter has, so a truncated read here is the one that
// would fabricate a decline.
func TestStripeRefusesAnOversizeErrorEnvelopeAsUnknown(t *testing.T) {
	head := `{"error":{"code":"card_declined","type":"card_error","message":"`
	tail := `"}}`
	pad := int(maxStripeResponseBytes) + 1 - len(head) - len(tail)
	body := head + strings.Repeat("x", pad) + tail

	stub := newStripeStub(t, map[string]stubResp{"POST /v1/payment_intents": {402, body}})
	s := newStripeForStub(stub)

	got, err := s.Authorize(context.Background(), AuthorizeRequest{
		OrganizerID: "org", OrderID: "ord", BuyerID: "buy",
		Amount: 1250, Currency: "EUR", PaymentToken: "pm", IdempotencyKey: "idem-over-decline",
	})
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
	if got.Outcome != Unknown || got.TerminalNoSideEffect {
		t.Fatalf("an unreadable error envelope must not become a terminal decline, got %+v", got)
	}
}
