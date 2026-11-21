package refundoutcome

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
