package exchangeunwind

// The WIRE tier: what each payments answer means.
//
// Separate from service_test.go because the questions are different. That file pins WHICH
// endpoint is asked; this one pins what the answer is taken to mean — and the dangerous
// direction is uniform: every reading that is not a clean, positive proof must refuse. A
// single answer misread as absence deletes a charged buyer's binding.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testOrg = "33333333-3333-3333-3333-333333333333"

// requestOf runs one lookup and returns the request payments actually received.
func requestOf(t *testing.T, status, body string, call func(HTTPPayments) (MoneyEvidence, error)) (MoneyEvidence, error, *http.Request) {
	t.Helper()
	var got *http.Request
	code := http.StatusOK
	if status == "404" {
		code = http.StatusNotFound
	} else if status == "500" {
		code = http.StatusInternalServerError
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	p := NewHTTPPayments(srv.URL, "test-token", 5*time.Second)
	ev, err := call(p)
	return ev, err, got
}

// A 404 is the ONLY proof of absence, and payments documents it as exactly that.
func TestA404IsTheProofOfAbsenceForACharge(t *testing.T) {
	ev, err, req := requestOf(t, "404", `{"error":"operation not found"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "exchange-charge:x")
		})
	if err != nil {
		t.Fatalf("LookupChargeOperation: %v", err)
	}
	if ev != Absent {
		t.Errorf("evidence = %v, want absent — payments answering 404 IS the evidence the charge "+
			"was never submitted", ev)
	}
	if got := req.URL.Path; got != "/internal/operations" {
		t.Errorf("path = %q, want /internal/operations", got)
	}
	if got := req.URL.Query().Get("idempotency_key"); got != "exchange-charge:x" {
		t.Errorf("idempotency_key = %q, want exchange-charge:x", got)
	}
	if got := req.Header.Get("X-Internal-Token"); got != "test-token" {
		t.Errorf("credential = %q; payments fails closed on a missing one and it would read as an outage", got)
	}
}

// A captured operation is presence. `captured_amount` is published ONLY when the provider
// state is `captured`, which makes its presence the least interpretive signal available.
func TestACapturedOperationIsPresence(t *testing.T) {
	ev, err, _ := requestOf(t, "200", `{"resolved":true,"status":"succeeded","captured_amount":1000,"currency":"EUR"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "k")
		})
	if err != nil {
		t.Fatalf("LookupChargeOperation: %v", err)
	}
	if ev != Present {
		t.Errorf("evidence = %v, want present — the buyer was charged", ev)
	}
}

// A 200 WITHOUT capture evidence is indeterminate, never absent.
//
// Two shapes reach here and both must refuse: an operation bound but unresolved (payments
// does not know either), and one resolved-but-declined (no money moved, but concluding that
// from a status string means reasoning about provider semantics, and the cost of being wrong
// is a charged buyer's binding deleted). Refusing is the safe answer for both; the operator
// is told which they are looking at.
func TestA200WithoutCaptureEvidenceIsIndeterminate(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"bound but unresolved", `{"resolved":false,"occurred_at":"2026-08-23T10:00:00Z"}`},
		{"resolved and declined", `{"resolved":true,"status":"declined","fact_id":"f"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err, _ := requestOf(t, "200", tc.body,
				func(p HTTPPayments) (MoneyEvidence, error) {
					return p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "k")
				})
			if ev == Absent {
				t.Fatalf("evidence = absent for %q. A 200 means an operation EXISTS; only a 404 "+
					"proves one does not. Reading this as absence permits deleting the binding of "+
					"a buyer whose charge may have moved", tc.name)
			}
			if ev != Indeterminate {
				t.Errorf("evidence = %v, want indeterminate", ev)
			}
			if err == nil {
				t.Error("no error returned; the operator needs to be told WHY this refused")
			}
		})
	}
}

// A 5xx is indeterminate. An outage must never read as permission.
func TestAServerErrorIsIndeterminate(t *testing.T) {
	ev, err, _ := requestOf(t, "500", `{"error":"lookup operation"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "k")
		})
	if ev == Absent {
		t.Fatal("a 500 read as ABSENT — an outage would permit unwinding a charged exchange")
	}
	if ev != Indeterminate || err == nil {
		t.Errorf("evidence = %v err = %v, want indeterminate with an error", ev, err)
	}
}

// A transport failure is indeterminate. Nothing was asked, so nothing is known.
func TestATransportFailureIsIndeterminate(t *testing.T) {
	// A URL that cannot connect: the server is created and immediately closed.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	p := NewHTTPPayments(url, "test-token", 2*time.Second)

	ev, err := p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "k")
	if ev == Absent {
		t.Fatal("an unreachable payments read as ABSENT")
	}
	if ev != Indeterminate || err == nil {
		t.Errorf("evidence = %v err = %v, want indeterminate with an error", ev, err)
	}
}

// A body with trailing content is not proof. Two concatenated JSON values, or a `}` trailer,
// were accepted as evidence by the recovery client until three review passes closed it; the
// same check lives here for the same reason.
func TestATrailingContentBodyIsNotProof(t *testing.T) {
	ev, err, _ := requestOf(t, "200", `{"resolved":true,"captured_amount":1000}} {"resolved":false}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupChargeOperation(context.Background(), uuid.MustParse(testOrg), "k")
		})
	if err == nil {
		t.Fatal("a body with trailing content was accepted; a body that cannot be read in full is not evidence")
	}
	if ev != Indeterminate {
		t.Errorf("evidence = %v, want indeterminate", ev)
	}
}

// The refund-leg read sends ALL THREE parameters payments requires, to the refund-leg path.
func TestTheRefundLegReadSendsBothKeysToTheRefundLegEndpoint(t *testing.T) {
	ev, err, req := requestOf(t, "404", `{"error":"refund leg not found"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupRefundLeg(context.Background(), uuid.MustParse(testOrg), "src-key", "exchange-refund:x")
		})
	if err != nil {
		t.Fatalf("LookupRefundLeg: %v", err)
	}
	if ev != Absent {
		t.Errorf("evidence = %v, want absent", ev)
	}
	if got := req.URL.Path; got != "/internal/refund-legs" {
		t.Fatalf("path = %q, want /internal/refund-legs. A downgrade's money is NOT in "+
			"payment_operations, and asking that endpoint returns 404 — which looks exactly like "+
			"proof of safety and is not", got)
	}
	q := req.URL.Query()
	if q.Get("organizer_id") != testOrg || q.Get("source_idempotency_key") != "src-key" ||
		q.Get("refund_idempotency_key") != "exchange-refund:x" {
		t.Errorf("query = %v; payments requires all three — (organizer, source key) identifies a "+
			"charge that may carry many legs, and only the refund key picks this one", q)
	}
}

// A completed refund leg is presence: the buyer got their money back.
func TestACompletedRefundLegIsPresence(t *testing.T) {
	ev, err, _ := requestOf(t, "200", `{"completed":true,"amount":1000,"currency":"EUR"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupRefundLeg(context.Background(), uuid.MustParse(testOrg), "s", "r")
		})
	if err != nil {
		t.Fatalf("LookupRefundLeg: %v", err)
	}
	if ev != Present {
		t.Errorf("evidence = %v, want present", ev)
	}
}

// A BOUND but uncompleted leg is indeterminate, not absent. Payments' own words: "a bound
// leg is money the buyer has not received back" — money is in flight either way.
func TestAnUncompletedRefundLegIsIndeterminateNotAbsent(t *testing.T) {
	ev, err, _ := requestOf(t, "200", `{"completed":false,"amount":1000,"currency":"EUR"}`,
		func(p HTTPPayments) (MoneyEvidence, error) {
			return p.LookupRefundLeg(context.Background(), uuid.MustParse(testOrg), "s", "r")
		})
	if ev == Absent {
		t.Fatal("a bound-but-uncompleted refund leg read as ABSENT. A leg exists, so money is " +
			"bound against this exchange; only a 404 means none ever was")
	}
	if ev != Indeterminate || err == nil {
		t.Errorf("evidence = %v err = %v, want indeterminate with an error", ev, err)
	}
}
