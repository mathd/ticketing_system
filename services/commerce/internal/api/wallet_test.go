package api

import (
	"context"
	"database/sql"
	"encoding/base64"
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

// What a valid assertion naming an id that has no account actually gets.
//
// The first version of this test claimed to compare a MISMATCH against an UNKNOWN
// customer and compared two mismatches: both calls used a random path uuid and an
// assertion for a *different* random uuid (ai-review [medium]). It could not fail.
//
// The real answer is 200 with an empty wallet, and that is correct rather than a
// gap: an assertion is only ever minted for an account that authenticated, so
// "valid assertion, no such account" needs the account to have been deleted, and
// there is no delete path. Adding an existence check would put a database round
// trip on every wallet read to defend an unreachable case. The contract says this
// in as many words now — the 404 is for a MISMATCH, not an existence check.
func TestWalletAnswersAnEmptyPageForAValidAssertionNamingItself(t *testing.T) {
	unknown := uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})

	rec := walletGet(s, unknown, mintCustomerAssertion(walletKey, unknown, time.Now().Add(time.Hour)), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty page: %s", rec.Code, rec.Body.String())
	}
	if !jsonContains(t, rec.Body.String(), `"orders":[]`) {
		t.Fatalf("body = %s, want an empty array", rec.Body.String())
	}
}

// A cursor issued for another customer is refused, not applied. It could never
// have read their rows, but applied to this customer it silently suppresses their
// own — a failure with no symptom (ai-review [medium]).
func TestWalletRefusesACursorIssuedForSomebodyElse(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})
	bobsCursor := s.encodeCursor(commercestore.WalletCursor{
		CreatedAt: time.Now(), OrderID: uuid.New(), CustomerID: bob,
	})

	rec := walletGet(s, alice, mintCustomerAssertion(walletKey, alice, time.Now().Add(time.Hour)), "&after="+bobsCursor)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a foreign cursor must be refused, not applied: %s",
			rec.Code, rec.Body.String())
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
	want := commercestore.WalletCursor{
		CreatedAt: time.Date(2026, 3, 18, 17, 30, 0, 123456789, time.UTC),
		OrderID:   uuid.New(),
		CustomerID: uuid.New(),
	}

	s := &Server{assertionKey: walletKey}
	got, err := s.decodeCursor(s.encodeCursor(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.OrderID != want.OrderID || got.CustomerID != want.CustomerID {
		t.Fatalf("round trip lost precision: %v != %v", got, want)
	}
	// An empty cursor is the first page, not an error.
	if _, err := s.decodeCursor(""); err != nil {
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

// ai-review pass 2 [medium]: comparing the cursor's customer field was not enough
// on its own. A forged cursor naming the NIL customer slipped past the guard, and
// one naming the caller's own customer with an arbitrary timestamp could still
// skip or repeat their rows — which renders as a genuine empty wallet with no
// error anywhere. The cursor is signed now; these are the shapes that must fail.
func TestWalletRefusesEveryForgedCursor(t *testing.T) {
	customer := uuid.New()
	s := walletServer(t, nil, commercestore.WalletCursor{})
	assertion := mintCustomerAssertion(walletKey, customer, time.Now().Add(time.Hour))

	forge := func(payload string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	future := time.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339Nano)

	for _, tc := range []struct{ name, cursor string }{
		{"nil customer, no signature", forge(future + "|" + uuid.New().String() + "|" + uuid.Nil.String())},
		{"own customer, no signature", forge(future + "|" + uuid.New().String() + "|" + customer.String())},
		{"own customer, wrong signature", forge(future + "|" + uuid.New().String() + "|" + customer.String() + "|not-a-mac")},
		{"three fields, as the unsigned format used to be", forge(future + "|" + uuid.New().String() + "|" + customer.String())},
		{"not base64", "%%%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := walletGet(s, customer, assertion, "&after="+tc.cursor)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 — a forged cursor must be refused, not applied "+
					"(it renders as an empty wallet): %s", rec.Code, rec.Body.String())
			}
		})
	}

	// And a cursor this server genuinely issued still works.
	real := s.encodeCursor(commercestore.WalletCursor{
		CreatedAt: time.Now(), OrderID: uuid.New(), CustomerID: customer,
	})
	if rec := walletGet(s, customer, assertion, "&after="+real); rec.Code != http.StatusOK {
		t.Fatalf("a genuine cursor was refused: %d %s", rec.Code, rec.Body.String())
	}
}
