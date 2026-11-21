package psp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TKT-525 (ADR-087): the read-only terminal-failure proof. Every case asserts the verdict AND
// that the adapter made no POST, because the property that makes this safe to call from an
// operator command is that it cannot move money.

const proofPI, proofKey = "pi_proof", "psp-refund-leg-v1:proof"

func proofRefund(id, status, pi, key string, amount int64, currency string) string {
	return fmt.Sprintf(`{"id":%q,"object":"refund","amount":%d,"currency":%q,"status":%q,"payment_intent":%q,"metadata":{"compensation_key":%q}}`,
		id, amount, currency, status, pi, key)
}

func proofList(hasMore bool, refunds ...string) string {
	data := ""
	for i, r := range refunds {
		if i > 0 {
			data += ","
		}
		data += r
	}
	return fmt.Sprintf(`{"object":"list","has_more":%t,"data":[%s]}`, hasMore, data)
}

func assertProofMadeNoPost(t *testing.T, stub *stripeStub) {
	t.Helper()
	for _, r := range stub.requests {
		if r.method != http.MethodGet || r.path != "/v1/refunds" {
			t.Fatalf("the proof made a %s %s; it may only list refunds", r.method, r.path)
		}
		if r.query.Get("payment_intent") != proofPI {
			t.Fatalf("the listing was not scoped to the PaymentIntent: %v", r.query)
		}
	}
}

func TestStripeProveRefundFailureAcceptsAMatchingTerminalRefund(t *testing.T) {
	for _, status := range []string{"failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			list := proofList(false,
				// A foreign dashboard refund beside ours does not disturb the proof.
				`{"id":"re_dashboard","object":"refund","amount":1000,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`,
				proofRefund("re_ours_"+status, status, proofPI, proofKey, 1000, "eur"))
			stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds": {200, list}})
			got, err := newStripeForStub(stub).ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
			if err != nil {
				t.Fatalf("a matching %s refund was not accepted as proof: %v", status, err)
			}
			if got.ProviderRef != "re_ours_"+status {
				t.Fatalf("proof reference = %q, want the matching refund's own id", got.ProviderRef)
			}
			assertProofMadeNoPost(t, stub)
		})
	}
}

func TestStripeProveRefundFailureFindsTheMatchOnALaterPage(t *testing.T) {
	var queries []url.Values
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/refunds" {
			t.Errorf("the proof made a %s %s; it may only list refunds", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.Query())
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			_, _ = w.Write([]byte(proofList(true, `{"id":"re_other","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`)))
			return
		}
		_, _ = w.Write([]byte(proofList(false, proofRefund("re_late", "failed", proofPI, proofKey, 1000, "eur"))))
	}))
	t.Cleanup(srv.Close)
	got, err := NewStripe("sk_test_dummy", srv.URL, srv.Client()).ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
	if err != nil || got.ProviderRef != "re_late" {
		t.Fatalf("proof = %+v err=%v, want re_late from page two", got, err)
	}
	if len(queries) != 2 || queries[1].Get("starting_after") != "re_other" {
		t.Fatalf("listing queries = %v, want two pages with the cursor advanced past re_other", queries)
	}
}

// Every case that is not a proof, with the verdict it must get. Each fixture differs from the
// accepted one in ONE field, so each predicate in the match rule and the status mapping is
// reached on its own.
func TestStripeProveRefundFailureRefusesEverythingElse(t *testing.T) {
	ok := proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "eur")
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"a matching refund that succeeded", 200, proofList(false, proofRefund("re_ours", "succeeded", proofPI, proofKey, 1000, "eur")), ErrRefundProofContradicted},
		{"two refunds carry the key", 200, proofList(false, ok, proofRefund("re_second", "failed", proofPI, proofKey, 1000, "eur")), ErrRefundProofContradicted},
		{"the matching refund is pending", 200, proofList(false, proofRefund("re_ours", "pending", proofPI, proofKey, 1000, "eur")), ErrRefundProofInconclusive},
		{"the matching refund needs action", 200, proofList(false, proofRefund("re_ours", "requires_action", proofPI, proofKey, 1000, "eur")), ErrRefundProofInconclusive},
		{"no refund carries the key", 200, proofList(false, proofRefund("re_ours", "failed", proofPI, "psp-refund-leg-v1:other", 1000, "eur")), ErrRefundProofInconclusive},
		{"the refund is unstamped", 200, proofList(false, `{"id":"re_ours","object":"refund","amount":1000,"currency":"eur","status":"failed","payment_intent":"pi_proof","metadata":{}}`), ErrRefundProofInconclusive},
		{"a different PaymentIntent", 200, proofList(false, proofRefund("re_ours", "failed", "pi_other", proofKey, 1000, "eur")), ErrRefundProofInconclusive},
		{"a different amount", 200, proofList(false, proofRefund("re_ours", "failed", proofPI, proofKey, 999, "eur")), ErrRefundProofInconclusive},
		{"a different currency", 200, proofList(false, proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "usd")), ErrRefundProofInconclusive},
		{"a stamped refund with no id", 200, proofList(false, proofRefund("", "failed", proofPI, proofKey, 1000, "eur")), ErrRefundProofInconclusive},
		// Uncorroborated stamped evidence takes precedence over a corroborated match: 502, not
		// a proof and not a 409. The id-less object must count even though it has no id (R1-2),
		// in either position.
		{"a corroborated match beside an uncorroborated one", 200, proofList(false, ok, proofRefund("re_odd", "failed", proofPI, proofKey, 5, "eur")), ErrRefundProofInconclusive},
		{"a corroborated match beside a stamped refund with no id", 200, proofList(false, ok, proofRefund("", "failed", proofPI, proofKey, 1000, "eur")), ErrRefundProofInconclusive},
		{"a stamped refund with no id before a corroborated match", 200, proofList(false, proofRefund("", "failed", proofPI, proofKey, 1000, "eur"), ok), ErrRefundProofInconclusive},
		{"an incomplete page", 200, `{"object":"list","data":[` + ok + `]}`, ErrRefundProofInconclusive},
		{"a malformed page", 200, `{"object":"list",`, ErrRefundProofInconclusive},
		{"more pages but none returned", 200, proofList(true), ErrRefundProofInconclusive},
		{"the provider is unavailable", 503, `{"error":{"message":"down","type":"api_error"}}`, ErrRefundProofInconclusive},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newStripeStub(t, map[string]stubResp{"GET /v1/refunds": {c.status, c.body}})
			got, err := newStripeForStub(stub).ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v (proof %+v), want %v", err, got, c.want)
			}
			if got.ProviderRef != "" {
				t.Fatalf("a refused proof returned a reference %q", got.ProviderRef)
			}
			if len(stub.requests) == 0 {
				t.Fatal("the proof never asked the provider")
			}
			assertProofMadeNoPost(t, stub)
		})
	}
}

// A proof cannot be looked up without the charge reference and the stamp; it does not call the
// provider with an empty filter, which would list refunds across every charge.
func TestStripeProveRefundFailureNeedsAChargeAndAKey(t *testing.T) {
	for _, c := range []struct{ pi, key string }{{"", proofKey}, {proofPI, ""}} {
		stub := newStripeStub(t, nil)
		_, err := newStripeForStub(stub).ProveRefundFailure(context.Background(), c.pi, c.key, 1000, "EUR")
		if !errors.Is(err, ErrRefundProofInconclusive) {
			t.Fatalf("pi=%q key=%q: err = %v, want inconclusive", c.pi, c.key, err)
		}
		if len(stub.requests) != 0 {
			t.Fatalf("pi=%q key=%q: the proof called the provider %d times", c.pi, c.key, len(stub.requests))
		}
	}
}

// Both adapters are provers. The fake proves only refunds it itself failed (TKT-525); its own
// tests are in fake_refund_proof_test.go, including that an ordinary fake charge can never be
// proven.
func TestBothPSPsAreRefundFailureProvers(t *testing.T) {
	var p PSP = NewFake()
	if _, ok := p.(RefundFailureProver); !ok {
		t.Fatal("the fake PSP does not implement RefundFailureProver")
	}
	var s PSP = &Stripe{}
	if _, ok := s.(RefundFailureProver); !ok {
		t.Fatal("the Stripe adapter does not implement RefundFailureProver")
	}
}
