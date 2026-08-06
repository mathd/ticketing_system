package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// The wallet handler (TKT-222). What is under test here is the AUTHORIZATION and
// the assembly — the query itself is proven against real Postgres.

var walletKey = customerAssertionKey("tkt-222 test signing key")

func walletServer(t *testing.T, page []commercestore.WalletOrder, next commercestore.WalletCursor) *Server {
	t.Helper()
	prev := listCustomerOrdersFn
	listCustomerOrdersFn = func(context.Context, *sql.DB, uuid.UUID, commercestore.WalletCursor, int) ([]commercestore.WalletOrder, commercestore.WalletCursor, error) {
		return page, next, nil
	}
	t.Cleanup(func() { listCustomerOrdersFn = prev })
	return &Server{assertionKey: walletKey}
}

func walletGet(s *Server, customer uuid.UUID, assertion, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/customers/"+customer.String()+"/orders?locale=en"+query, nil)
	if assertion != "" {
		req.Header.Set(assertionHeader, assertion)
	}
	// chi's URL param, supplied directly: the handler is under test, not the router.
	ctx := chi.NewRouteContext()
	ctx.URLParams.Add("id", customer.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx))
	rec := httptest.NewRecorder()
	s.listCustomerOrders(rec, req)
	return rec
}

// The ticket's headline requirement, and the test is written the way the AC asks:
// B asks for A's data, holding a perfectly valid assertion of their own.
//
// The answer is 404 — the same as an id that does not exist — because a 403 would
// confirm that customer A exists.
func TestWalletRefusesOneCustomerAskingForAnothers(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	s := walletServer(t, []commercestore.WalletOrder{{OrderID: uuid.New()}}, commercestore.WalletCursor{})
	bobsAssertion := mintCustomerAssertion(walletKey, bob, time.Now().Add(time.Hour))

	rec := walletGet(s, alice, bobsAssertion, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// And nothing about Alice leaks into the refusal.
	if body := rec.Body.String(); len(body) > 0 && (jsonContains(t, body, alice.String())) {
		t.Fatalf("the refusal names the customer that was asked for: %s", body)
	}
}

// An unknown customer answers identically, which is what makes the refusal above
// disclose nothing: a caller cannot tell "not yours" from "no such account".
func TestWalletAnswersTheSameForAMismatchAndAnUnknownCustomer(t *testing.T) {
	s := walletServer(t, nil, commercestore.WalletCursor{})
	mismatch := walletGet(s, uuid.New(), mintCustomerAssertion(walletKey, uuid.New(), time.Now().Add(time.Hour)), "")
	unknown := walletGet(s, uuid.New(), mintCustomerAssertion(walletKey, uuid.New(), time.Now().Add(time.Hour)), "")

	if mismatch.Code != unknown.Code || mismatch.Body.String() != unknown.Body.String() {
		t.Fatalf("the two answers differ:\n mismatch: %d %s unknown:  %d %s",
			mismatch.Code, mismatch.Body.String(), unknown.Code, unknown.Body.String())
	}
}

func TestWalletRequiresAnAssertion(t *testing.T) {
	customer := uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})

	for _, tc := range []struct{ name, assertion string }{
		{"absent", ""},
		{"forged", "v1." + customer.String() + ".99999999999.not-a-mac"},
		{"expired", mintCustomerAssertion(walletKey, customer, time.Now().Add(-time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := walletGet(s, customer, tc.assertion, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWalletReturnsThePageAndItsCursor(t *testing.T) {
	customer, order := uuid.New(), uuid.New()
	ref := uuid.New()
	at := time.Date(2026, 3, 18, 17, 30, 0, 0, time.UTC)
	s := walletServer(t,
		[]commercestore.WalletOrder{{OrderID: order, GuestOrderRef: ref, CreatedAt: at, Quantity: 2, TotalAmount: 9100, Currency: "EUR"}},
		commercestore.WalletCursor{CreatedAt: at, OrderID: order})

	rec := walletGet(s, customer, mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour)), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store — a wallet is per-customer", got)
	}
	var out struct {
		Orders []struct {
			OrderID       string  `json:"order_id"`
			GuestOrderRef string  `json:"guest_order_ref"`
			TotalAmount   int64   `json:"total_amount"`
			Currency      string  `json:"currency"`
			EventName     *string `json:"event_name"`
		} `json:"orders"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Orders) != 1 || out.Orders[0].OrderID != order.String() || out.Orders[0].GuestOrderRef != ref.String() {
		t.Fatalf("page = %+v", out.Orders)
	}
	// Money is integer minor units, never divided or formatted here (ADR-001).
	if out.Orders[0].TotalAmount != 9100 || out.Orders[0].Currency != "EUR" {
		t.Fatalf("money = %d %s", out.Orders[0].TotalAmount, out.Orders[0].Currency)
	}
	// Catalog is unreachable in this test, so the name is explicitly null rather
	// than absent — and the row is still usable, which is the point.
	if out.Orders[0].EventName != nil {
		t.Fatalf("event_name = %v, want null when catalog cannot be reached", *out.Orders[0].EventName)
	}
	if out.NextCursor == nil || *out.NextCursor == "" {
		t.Fatal("a page with more behind it must carry a cursor")
	}
}

// The empty wallet is an acceptance criterion in its own right, and it is the
// case every fixture skips because the fixture buys something first.
func TestWalletAnswersAnEmptyPageWithANullCursor(t *testing.T) {
	customer := uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})

	rec := walletGet(s, customer, mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour)), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Orders     []json.RawMessage `json:"orders"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Orders) != 0 {
		t.Fatalf("orders = %v, want empty", out.Orders)
	}
	if out.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null on the last page", *out.NextCursor)
	}
	// An empty array, not null — a client that iterates must not have to
	// null-check the collection.
	if !jsonContains(t, rec.Body.String(), `"orders":[]`) {
		t.Fatalf("body = %s, want an empty ARRAY", rec.Body.String())
	}
}

// A malformed cursor is a client error, not a reason to serve page one: silently
// restarting makes a paging client loop over the same rows for ever with no error
// to notice.
func TestWalletRefusesAMalformedCursorRatherThanRestarting(t *testing.T) {
	customer := uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})

	rec := walletGet(s, customer, mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour)), "&after=not-a-cursor")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestWalletCursorRoundTrips(t *testing.T) {
	want := commercestore.WalletCursor{CreatedAt: time.Date(2026, 3, 18, 17, 30, 0, 123456789, time.UTC), OrderID: uuid.New()}

	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.OrderID != want.OrderID {
		t.Fatalf("round trip lost precision: %v != %v", got, want)
	}
	// An empty cursor is the first page, not an error.
	if _, err := decodeCursor(""); err != nil {
		t.Fatalf("the first page must not need a cursor: %v", err)
	}
}

func jsonContains(t *testing.T, body, needle string) bool {
	t.Helper()
	return len(body) > 0 && len(needle) > 0 && bytesContains(body, needle)
}

func bytesContains(haystack, needle string) bool {
	return len(needle) <= len(haystack) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
