package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// The three refused cases are ONE answer at the HTTP boundary, which is the only
// place the claim is about. Telling them apart hands a caller probing references
// an oracle for which are real, complete and unclaimed.
//
// Driven with THREE DISTINCT store outcomes, not one error injected twice
// (ai-review pass 2 [medium]): the handler maps via errors.Is, so an earlier
// version that stubbed the same error for both requests proved only that one
// error maps consistently to itself. A handler could differentiate wrapped
// variants and stay green.
func TestClaimRefusesEveryUnclaimableOrderIdentically(t *testing.T) {
	customer := uuid.New()
	assertion := mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour))
	ref := uuid.New()

	// What the three real cases look like coming out of the store: the sentinel
	// itself, and two wrapped forms a future implementation might plausibly
	// produce while still satisfying errors.Is.
	outcomes := []struct {
		name string
		err  error
	}{
		{"no such order", commercestore.ErrOrderNotClaimable},
		{"not completed", fmt.Errorf("order is not completed: %w", commercestore.ErrOrderNotClaimable)},
		{"already claimed by somebody else", fmt.Errorf("order belongs to %s: %w", uuid.New(), commercestore.ErrOrderNotClaimable)},
	}

	var answers []string
	for _, tc := range outcomes {
		s, _ := claimServer(t, uuid.Nil, tc.err)
		rec := postClaim(s, assertion, `{"guest_order_ref":"`+ref.String()+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404: %s", tc.name, rec.Code, rec.Body.String())
		}
		// Whatever the store said must not reach the caller.
		if strings.Contains(rec.Body.String(), ref.String()) || strings.Contains(rec.Body.String(), "belongs to") {
			t.Fatalf("%s: the refusal leaks the store's reason: %s", tc.name, rec.Body.String())
		}
		answers = append(answers, rec.Body.String())
	}
	for i := 1; i < len(answers); i++ {
		t.Run(outcomes[i].name, func(t *testing.T) {
			if answers[i] != answers[0] {
				t.Fatalf("this refusal reads %q where %q reads %q — three answers, not one",
					answers[i], outcomes[0].name, answers[0])
			}
		})
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
