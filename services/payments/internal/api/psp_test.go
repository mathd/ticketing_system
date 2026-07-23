package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/store"
)

// The compensation state checks are the money-path guard: the caller selects the
// endpoint, payments validates it is the CORRECT compensation for the stored durable
// evidence — it must not silently turn a void into a refund or trust the caller's
// claimed state (plan §void-vs-refund). Pure over store.Operation so the full matrix is
// testable without a database; the composed smoke suite drives the DB-backed flow.
func TestCompensationAllowed(t *testing.T) {
	captured := store.Operation{
		Resolved: true, Status: "captured",
		OrderID: uuid.New(), BuyerID: uuid.New(),
		RequestAmount: 1250, RequestCurrency: "EUR",
		ProviderState: "captured", AuthorizedAmount: 1250, CapturedAmount: 1250,
	}
	authorized := captured
	authorized.Resolved, authorized.Status = false, ""
	authorized.ProviderState, authorized.CapturedAmount = "authorized", 0

	legacy := store.Operation{Resolved: true, Status: "captured"} // pre-0002 row: no evidence

	unresolvedNoEvidence := store.Operation{
		OrderID: uuid.New(), BuyerID: uuid.New(), RequestAmount: 1250, RequestCurrency: "EUR",
	}

	cases := []struct {
		name string
		op   store.Operation
		kind string
		ok   bool
	}{
		{"refund captured money", captured, "refund", true},
		{"void captured money rejected", captured, "void", false},
		{"void authorized-uncaptured", authorized, "void", true},
		{"refund authorized-uncaptured rejected", authorized, "refund", false},
		{"refund without evidence rejected", unresolvedNoEvidence, "refund", false},
		{"void without evidence rejected", unresolvedNoEvidence, "void", false},
		{"refund legacy row without request data rejected", legacy, "refund", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compensationAllowed(tc.op, tc.kind)
			if tc.ok && err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejection, got allowed")
			}
		})
	}
}

// The compensation basis comes from the STORED row, never the caller: a refund refunds
// the captured amount, a void releases the authorized amount (plan-final A5).
func TestCompensationBasisFromStoredRow(t *testing.T) {
	op := store.Operation{AuthorizedAmount: 2000, CapturedAmount: 1250, RequestCurrency: "EUR"}
	if amt, cur := compensationBasis(op, "refund"); amt != 1250 || cur != "EUR" {
		t.Fatalf("refund basis = %d %s, want captured 1250 EUR", amt, cur)
	}
	if amt, _ := compensationBasis(op, "void"); amt != 2000 {
		t.Fatalf("void basis = %d, want authorized 2000", amt)
	}
}

// All three PSP endpoints are internal: no token, no answer — and they never reach the
// journal (nil here), proving auth is checked first.
func TestPSPEndpointsRequireInternalToken(t *testing.T) {
	server := New(nil, "secret")
	body := `{"organizer_id":"00000000-0000-0000-0000-000000000001","idempotency_key":"k1"}`
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/internal/psp/status?organizer_id=00000000-0000-0000-0000-000000000001&idempotency_key=k1", nil),
		httptest.NewRequest(http.MethodPost, "/internal/psp/void", bytes.NewBufferString(body)),
		httptest.NewRequest(http.MethodPost, "/internal/psp/refund", bytes.NewBufferString(body)),
	}
	for _, request := range requests {
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Router(nil).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", request.Method, request.URL.Path, recorder.Code)
		}
	}
}

// Deterministic compensation fact IDs (plan-final A3): the same compensation always
// derives the same fact ID, so the journal's fact-id replay dedupe absorbs the
// append/complete crash boundary; distinct fact types derive distinct IDs.
func TestCompensationFactIDDeterministic(t *testing.T) {
	org := uuid.MustParse("00000000-0000-0000-0000-000000000042")
	a := compensationFactID(org, "charge-key-1", "payment.voided")
	b := compensationFactID(org, "charge-key-1", "payment.voided")
	if a != b {
		t.Fatal("fact ID not deterministic")
	}
	if compensationFactID(org, "charge-key-1", "payment.refunded") == a {
		t.Fatal("distinct fact types must derive distinct IDs")
	}
}
