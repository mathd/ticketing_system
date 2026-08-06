package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Claiming a guest order (TKT-223). Under test here is the authorization and the
// refusal mapping; the atomicity is proven against real Postgres.

func claimServer(t *testing.T, order uuid.UUID, storeErr error) (*Server, *int) {
	t.Helper()
	calls := 0
	prev := claimGuestOrderFn
	claimGuestOrderFn = func(context.Context, *sql.DB, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
		calls++
		return order, storeErr
	}
	t.Cleanup(func() { claimGuestOrderFn = prev })
	return &Server{assertionKey: walletKey}, &calls
}

func postClaim(s *Server, assertion, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/orders/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if assertion != "" {
		req.Header.Set(assertionHeader, assertion)
	}
	rec := httptest.NewRecorder()
	s.claimGuestOrder(rec, req)
	return rec
}

func TestClaimAttributesTheOrderToTheAssertedCustomer(t *testing.T) {
	customer, order, ref := uuid.New(), uuid.New(), uuid.New()
	s, _ := claimServer(t, order, nil)

	rec := postClaim(s, mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour)),
		`{"guest_order_ref":"`+ref.String()+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["order_id"] != order.String() || out["guest_order_ref"] != ref.String() || out["customer_id"] != customer.String() {
		t.Fatalf("result = %v", out)
	}
	// No `status` field: there is no order status called "claimed", and one here
	// would send the next reader looking for a state ADR-016 does not have.
	if _, present := out["status"]; present {
		t.Fatalf("the response carries a status field: %v", out)
	}
}

// The three refused cases are ONE answer. Telling them apart hands a caller
// probing references an oracle for which are real, complete and unclaimed.
func TestClaimRefusesEveryUnclaimableOrderIdentically(t *testing.T) {
	customer, ref := uuid.New(), uuid.New()
	assertion := mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour))
	s, _ := claimServer(t, uuid.Nil, commercestore.ErrOrderNotClaimable)

	// The store cannot tell them apart either — it reports one error for all
	// three — so this asserts the mapping stays a single answer.
	first := postClaim(s, assertion, `{"guest_order_ref":"`+ref.String()+`"}`)
	second := postClaim(s, assertion, `{"guest_order_ref":"`+uuid.New().String()+`"}`)

	if first.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", first.Code, first.Body.String())
	}
	if first.Code != second.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("two refusals differ:\n %d %s %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	// And the refusal names nothing.
	if strings.Contains(first.Body.String(), ref.String()) {
		t.Fatalf("the refusal echoes the reference: %s", first.Body.String())
	}
}

// A forged assertion must cost nothing: the store is never reached.
func TestClaimRequiresAnAssertionAndReachesNoStoreWithoutOne(t *testing.T) {
	ref := uuid.New()
	body := `{"guest_order_ref":"` + ref.String() + `"}`

	for _, tc := range []struct{ name, assertion string }{
		{"absent", ""},
		{"forged", "v1." + uuid.New().String() + ".99999999999.not-a-mac"},
		{"expired", mintCustomerAssertion(walletKey, uuid.New(), time.Now().Add(-time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := claimServer(t, uuid.New(), nil)
			rec := postClaim(s, tc.assertion, body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if *calls != 0 {
				t.Fatalf("the store was reached %d times by an unauthenticated claim", *calls)
			}
		})
	}
}

// A malformed reference is a 400, NOT the generic 404: it is request validation,
// and a caller who cannot spell a uuid has learned nothing about the order book.
func TestClaimSeparatesAMalformedReferenceFromARefusal(t *testing.T) {
	s, calls := claimServer(t, uuid.New(), nil)

	rec := postClaim(s, mintCustomerAssertion(walletKey, uuid.New(), time.Now().Add(time.Hour)),
		`{"guest_order_ref":"not-a-uuid"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if *calls != 0 {
		t.Fatal("a malformed reference reached the store")
	}
}

// The body cannot name a customer at all — it is REFUSED, not ignored, which is
// the stronger property and the one the contract states with
// `additionalProperties: false`.
//
// The distinction is worth a test rather than a comment: "ignored" means a future
// decoder change quietly starts honouring it, while "refused" fails loudly the
// moment anyone tries. Identity is the assertion and the body has no say.
func TestClaimRefusesABodyThatNamesACustomer(t *testing.T) {
	victim, attacker, ref := uuid.New(), uuid.New(), uuid.New()
	s, calls := claimServer(t, uuid.New(), nil)

	rec := postClaim(s, mintCustomerAssertion(walletKey, attacker, time.Now().Add(time.Hour)),
		`{"guest_order_ref":"`+ref.String()+`","customer_id":"`+victim.String()+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a body naming a customer must be refused outright: %s",
			rec.Code, rec.Body.String())
	}
	if *calls != 0 {
		t.Fatal("a body naming a customer reached the store")
	}
}
