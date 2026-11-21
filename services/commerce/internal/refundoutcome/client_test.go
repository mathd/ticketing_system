package refundoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The read-only payments proof client (TKT-525). Only a clean, declared 200 is a proof; a
// conclusive 404 or 409 is a refusal; everything else is unavailable. Collapsing the last two
// would tell an operator "payments says no" when payments could not be asked.

func TestHTTPPaymentsSendsOneScopedReadOnlyGet(t *testing.T) {
	org, refund := uuid.New(), uuid.New()
	var seen *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_, _ = w.Write([]byte(`{"provider_ref":"re_failed","amount":2000,"currency":"EUR"}`))
	}))
	t.Cleanup(srv.Close)
	got, err := NewHTTPPayments(srv.URL, "payments-token", time.Second).ProveRefundFailure(context.Background(), org, "charge key/with?specials", refund.String())
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if got != (FailureProof{ProviderRef: "re_failed", Amount: 2000, Currency: "EUR"}) {
		t.Fatalf("proof = %+v", got)
	}
	if seen.Method != http.MethodGet || seen.URL.Path != "/internal/psp/partial-refund/failure-proof" {
		t.Fatalf("request = %s %s, want GET /internal/psp/partial-refund/failure-proof", seen.Method, seen.URL.Path)
	}
	q := seen.URL.Query()
	if q.Get("organizer_id") != org.String() || q.Get("idempotency_key") != "charge key/with?specials" || q.Get("refund_key") != refund.String() {
		t.Fatalf("query = %v", q)
	}
	if seen.Header.Get("X-Internal-Token") != "payments-token" {
		t.Fatalf("credential header = %q", seen.Header.Get("X-Internal-Token"))
	}
}

func TestHTTPPaymentsClassifiesEveryOtherAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"no such leg", 404, `{"error":"refund leg not found"}`, ErrProofRefused},
		{"conclusive refusal", 409, `{"error":"refund leg completed"}`, ErrProofRefused},
		{"inconclusive provider", 502, `{"error":"refund failure proof unavailable"}`, ErrProofUnavailable},
		{"payments fault", 500, `{"error":"lookup refund leg"}`, ErrProofUnavailable},
		{"bad request", 400, `{"error":"x"}`, ErrProofUnavailable},
		{"bad credential", 401, `{"error":"unauthorized"}`, ErrProofUnavailable},
		{"trailing content", 200, `{"provider_ref":"re_a","amount":1,"currency":"EUR"}{}`, ErrProofUnavailable},
		{"not JSON", 200, `proof`, ErrProofUnavailable},
		{"no reference", 200, `{"amount":1,"currency":"EUR"}`, ErrProofUnavailable},
		{"not a refund reference", 200, `{"provider_ref":"pi_a","amount":1,"currency":"EUR"}`, ErrProofUnavailable},
		{"bare prefix", 200, `{"provider_ref":"re_","amount":1,"currency":"EUR"}`, ErrProofUnavailable},
		{"no amount", 200, `{"provider_ref":"re_a","currency":"EUR"}`, ErrProofUnavailable},
		{"bad currency", 200, `{"provider_ref":"re_a","amount":1,"currency":"eur"}`, ErrProofUnavailable},
		{"unknown field", 200, `{"provider_ref":"re_a","amount":1,"currency":"EUR","completed":true}`, ErrProofUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)
			got, err := NewHTTPPayments(srv.URL, "t", time.Second).ProveRefundFailure(context.Background(), uuid.New(), "k", uuid.NewString())
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v (proof %+v), want %v", err, got, c.want)
			}
			if got != (FailureProof{}) {
				t.Fatalf("a refused proof returned %+v", got)
			}
		})
	}
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		if _, err := NewHTTPPayments(srv.URL, "t", time.Second).ProveRefundFailure(context.Background(), uuid.New(), "k", uuid.NewString()); !errors.Is(err, ErrProofUnavailable) {
			t.Fatalf("err = %v, want ErrProofUnavailable", err)
		}
	})
}

// The abandon client (TKT-525 S2): one POST with the stored identities and money. A 200 in the
// declared shape is evidence; 404 and 409 are conclusive refusals; everything else is
// uncertain, because payments may have committed its marker before the answer was lost.
func TestHTTPAbandonerPostsOneScopedAbandon(t *testing.T) {
	org, refund := uuid.New(), uuid.New()
	var seen *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"provider_ref":"re_failed","amount":2000,"currency":"EUR"}`))
	}))
	t.Cleanup(srv.Close)
	got, err := NewHTTPAbandoner(srv.URL, "payments-token", time.Second).AbandonRefundLeg(context.Background(), org, "charge-key", refund.String(), 2000, "EUR")
	if err != nil || got != (FailureProof{ProviderRef: "re_failed", Amount: 2000, Currency: "EUR"}) {
		t.Fatalf("abandon = %+v err=%v", got, err)
	}
	if seen.Method != http.MethodPost || seen.URL.Path != "/internal/psp/partial-refund/abandon" || seen.Header.Get("X-Internal-Token") != "payments-token" {
		t.Fatalf("request = %s %s token=%q", seen.Method, seen.URL.Path, seen.Header.Get("X-Internal-Token"))
	}
	want := map[string]any{"organizer_id": org.String(), "idempotency_key": "charge-key", "refund_key": refund.String(), "amount": float64(2000), "currency": "EUR"}
	if len(body) != len(want) {
		t.Fatalf("body = %v", body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
}

func TestHTTPAbandonerClassifiesEveryAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"no such leg", 404, `{"error":"refund leg not found"}`, ErrAbandonRefused},
		{"leg completed", 409, `{"error":"refund leg completed"}`, ErrAbandonRefused},
		{"provider inconclusive", 502, `{"error":"refund failure proof unavailable"}`, ErrAbandonUncertain},
		{"server error", 500, `{"error":"persist refund leg abandonment"}`, ErrAbandonUncertain},
		{"bad request", 400, `{"error":"invalid body"}`, ErrAbandonUncertain},
		{"unauthorized", 401, `{"error":"unauthorized"}`, ErrAbandonUncertain},
		{"200 not the declared shape", 200, `{"provider_ref":"SEPA","amount":2000,"currency":"EUR"}`, ErrAbandonUncertain},
		{"200 with trailing content", 200, `{"provider_ref":"re_x","amount":2000,"currency":"EUR"}{}`, ErrAbandonUncertain},
		{"200 with an unknown field", 200, `{"provider_ref":"re_x","amount":2000,"currency":"EUR","x":1}`, ErrAbandonUncertain},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)
			_, err := NewHTTPAbandoner(srv.URL, "t", time.Second).AbandonRefundLeg(context.Background(), uuid.New(), "k", uuid.NewString(), 2000, "EUR")
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		_, err := NewHTTPAbandoner(srv.URL, "t", time.Second).AbandonRefundLeg(context.Background(), uuid.New(), "k", uuid.NewString(), 2000, "EUR")
		if !errors.Is(err, ErrAbandonUncertain) {
			t.Fatalf("err = %v, want ErrAbandonUncertain", err)
		}
	})
}

// A valid object padded with whitespace past the read cap, then trailing content (R1-5). A
// client that decodes a truncated body sees only whitespace after the object and accepts it.
// The padding runs one byte past the cap, so even the one extra byte read to detect an
// oversized body is whitespace: only the length check refuses it. Both clients must refuse the
// oversized body before decoding.
func oversizedEvidence() string {
	valid := `{"provider_ref":"re_x","amount":2000,"currency":"EUR"}`
	return valid + strings.Repeat(" ", maxProofBody+1-len(valid)) + `{"trailing":true}`
}

func TestEvidenceClientsRefuseABodyLongerThanTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversizedEvidence()))
	}))
	t.Cleanup(srv.Close)
	if got, err := NewHTTPPayments(srv.URL, "t", time.Second).ProveRefundFailure(context.Background(), uuid.New(), "k", uuid.NewString()); !errors.Is(err, ErrProofUnavailable) {
		t.Fatalf("proof of an oversized 200 = %+v err=%v, want ErrProofUnavailable", got, err)
	}
	if got, err := NewHTTPAbandoner(srv.URL, "t", time.Second).AbandonRefundLeg(context.Background(), uuid.New(), "k", uuid.NewString(), 2000, "EUR"); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("abandon answered by an oversized 200 = %+v err=%v, want ErrAbandonUncertain", got, err)
	}
}
