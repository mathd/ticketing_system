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

// A provider that answers a cursor with a page that holds the same cursor cannot reach the rest
// of the listing (R2-2). The proof must refuse that page before it accepts any of its entries:
// when the page says more follow (a third page would end the listing with nothing more), and
// when it says it is the last one (RA3-2). Either way the listing never advanced past re_a.
func TestStripeProveRefundFailureRefusesACursorThatDoesNotAdvance(t *testing.T) {
	foreign := `{"id":"re_a","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`
	first := proofList(true, proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "eur"), foreign)
	for _, c := range []struct {
		name  string
		pages []string
	}{
		{"the repeated page says more follow", []string{first, proofList(true, foreign), proofList(false)}},
		{"the repeated page is the last one", []string{first, proofList(false, foreign)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var queries []url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/refunds" {
					t.Errorf("the proof made a %s %s; it may only list refunds", r.Method, r.URL.Path)
				}
				queries = append(queries, r.URL.Query())
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.pages[min(len(queries), len(c.pages))-1]))
			}))
			t.Cleanup(srv.Close)
			got, err := NewStripe("sk_test_dummy", srv.URL, srv.Client()).ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
			if !errors.Is(err, ErrRefundProofInconclusive) || got.ProviderRef != "" {
				t.Fatalf("proof = %+v err=%v, want inconclusive for a repeated cursor", got, err)
			}
			if len(queries) != 2 || queries[1].Get("starting_after") != "re_a" {
				t.Fatalf("listing queries = %v, want two pages, the second after re_a", queries)
			}
		})
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
		// Every entry must be classifiable (R2-2): an id and a metadata object. A malformed entry
		// beside a valid failed match makes the whole listing inconclusive, in either position.
		{"a corroborated match beside a null entry", 200, proofList(false, ok, `null`), ErrRefundProofInconclusive},
		{"a null entry before a corroborated match", 200, proofList(false, `null`, ok), ErrRefundProofInconclusive},
		{"a corroborated match beside an empty object", 200, proofList(false, ok, `{}`), ErrRefundProofInconclusive},
		{"a corroborated match beside an entry with null metadata", 200, proofList(false, ok, `{"id":"re_x","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":null}`), ErrRefundProofInconclusive},
		{"a corroborated match beside an entry with no metadata", 200, proofList(false, ok, `{"id":"re_x","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof"}`), ErrRefundProofInconclusive},
		{"a corroborated match beside an unstamped entry with no id", 200, proofList(false, ok, `{"id":"","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`), ErrRefundProofInconclusive},
		{"more pages after an entry with no id", 200, proofList(true, ok, `{"object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`), ErrRefundProofInconclusive},
		{"an incomplete page", 200, `{"object":"list","data":[` + ok + `]}`, ErrRefundProofInconclusive},
		{"a malformed page", 200, `{"object":"list",`, ErrRefundProofInconclusive},
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

// pagedStripe serves the given listing pages in order (the last one repeats) and answers every
// POST with a succeeded refund. It records the listing queries and counts the POSTs.
func pagedStripe(t *testing.T, pages []string) (*Stripe, *[]url.Values, *int) {
	t.Helper()
	var queries []url.Values
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts++
			_, _ = w.Write([]byte(proofRefund("re_posted", "succeeded", proofPI, proofKey, 1000, "eur")))
			return
		}
		// The adapter re-reads a POSTed success before it accepts it (RA5-2).
		if r.URL.Path == "/v1/refunds/re_posted" {
			_, _ = w.Write([]byte(proofRefund("re_posted", "succeeded", proofPI, proofKey, 1000, "eur")))
			return
		}
		queries = append(queries, r.URL.Query())
		_, _ = w.Write([]byte(pages[min(len(queries), len(pages))-1]))
	}))
	t.Cleanup(srv.Close)
	return NewStripe("sk_test_dummy", srv.URL, srv.Client()), &queries, &posts
}

// R4-1, R4-2: the whole listing must be valid before any entry counts as evidence. Each case is
// one malformed input. The proof variant holds a valid failed match that the old parser
// accepted; the dispatch variant holds no match, so a parser that accepts the listing POSTs.
// Both must refuse: the proof with no reference, the dispatch with zero POSTs.
func TestStripeListingRefusesAMalformedListingForProofAndDispatch(t *testing.T) {
	foreign := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`, id)
	}
	ours := proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "eur")
	nullStamp := `{"id":"re_null","object":"refund","amount":1000,"currency":"eur","status":"failed","payment_intent":"pi_proof","metadata":{"compensation_key":null}}`
	for _, c := range []struct {
		name          string
		proofPages    []string
		dispatchPages []string
	}{
		{
			"an older cursor repeated on the final page",
			[]string{proofList(true, foreign("re_a")), proofList(true, foreign("re_b")), proofList(false, foreign("re_a"), ours)},
			[]string{proofList(true, foreign("re_a")), proofList(true, foreign("re_b")), proofList(false, foreign("re_a"))},
		},
		{
			"a repeated non-cursor id on the final page",
			[]string{proofList(true, foreign("re_x"), foreign("re_a")), proofList(false, foreign("re_x"), ours)},
			[]string{proofList(true, foreign("re_x"), foreign("re_a")), proofList(false, foreign("re_x"))},
		},
		{
			"a null stamp (beside a valid failed match for the proof, alone for dispatch)",
			[]string{proofList(false, ours, nullStamp)},
			[]string{proofList(false, nullStamp)},
		},
		// RA5-1: a page can claim the listing is complete while it contradicts the page before
		// it. An empty page after a page that promised more holds nothing the cursor led to.
		{
			"an empty final page after a page that promised more",
			[]string{proofList(true, ours), proofList(false)},
			[]string{proofList(true, foreign("re_a")), proofList(false)},
		},
		{
			"an empty continuation page that promises more",
			[]string{proofList(true, ours), proofList(true)},
			[]string{proofList(true, foreign("re_a")), proofList(true)},
		},
		{
			"an empty first page that promises more",
			[]string{proofList(true)},
			[]string{proofList(true)},
		},
		{
			"a continuation page with no has_more",
			[]string{proofList(true, ours), `{"object":"list","data":[` + foreign("re_b") + `]}`},
			[]string{proofList(true, foreign("re_a")), `{"object":"list","data":[` + foreign("re_b") + `]}`},
		},
		{
			"a continuation page with null data",
			[]string{proofList(true, ours), `{"object":"list","has_more":false,"data":null}`},
			[]string{proofList(true, foreign("re_a")), `{"object":"list","has_more":false,"data":null}`},
		},
		{
			"a continuation page that is not a list",
			[]string{proofList(true, ours), `{"object":"search_result","has_more":false,"data":[` + foreign("re_b") + `]}`},
			[]string{proofList(true, foreign("re_a")), `{"object":"search_result","has_more":false,"data":[` + foreign("re_b") + `]}`},
		},
		{
			"an entry whose object is a charge",
			[]string{proofList(false, `{"id":"re_ours","object":"charge","amount":1000,"currency":"eur","status":"failed","payment_intent":"pi_proof","metadata":{"compensation_key":"psp-refund-leg-v1:proof"}}`)},
			[]string{proofList(false, `{"id":"re_c","object":"charge","amount":1000,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`)},
		},
	} {
		t.Run("proof: "+c.name, func(t *testing.T) {
			s, _, posts := pagedStripe(t, c.proofPages)
			got, err := s.ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
			if !errors.Is(err, ErrRefundProofInconclusive) || got.ProviderRef != "" {
				t.Fatalf("proof = %+v err=%v, want inconclusive with no reference", got, err)
			}
			if *posts != 0 {
				t.Fatalf("the proof sent %d POSTs", *posts)
			}
		})
		t.Run("dispatch: "+c.name, func(t *testing.T) {
			s, _, posts := pagedStripe(t, c.dispatchPages)
			got, err := s.Refund(context.Background(), proofPI, proofKey, 1000, "EUR")
			if err == nil || got.Outcome == Refunded {
				t.Fatalf("dispatch = %+v err=%v, want an inconclusive refusal", got, err)
			}
			if *posts != 0 {
				t.Fatalf("dispatch sent %d POSTs on a malformed listing", *posts)
			}
		})
	}
}

// The whole listing is validated before any entry is used: a stamped failed match on page one
// is not adopted when page two repeats it. Dispatch must answer inconclusive, not the terminal
// failure that the first page alone shows.
func TestStripeDispatchDoesNotAdoptAMatchBeforeTheListingIsValid(t *testing.T) {
	ours := proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "eur")
	s, queries, posts := pagedStripe(t, []string{proofList(true, ours), proofList(false, ours)})
	got, err := s.Refund(context.Background(), proofPI, proofKey, 1000, "EUR")
	if err == nil || errors.Is(err, ErrRefundFailed) || got.Outcome != Unknown {
		t.Fatalf("dispatch = %+v err=%v, want an inconclusive refusal, not a terminal failure", got, err)
	}
	if *posts != 0 || len(*queries) != 2 {
		t.Fatalf("posts=%d listing pages=%d, want 0 POSTs after reading both pages", *posts, len(*queries))
	}
}

// The control: an unstamped refund with an explicit empty metadata map is a valid foreign
// refund. The proof still accepts the failed match beside it, and dispatch still submits once
// when it is the only refund.
func TestStripeListingAcceptsAnUnstampedRefundWithEmptyMetadata(t *testing.T) {
	foreign := `{"id":"re_dash","object":"refund","amount":5,"currency":"eur","status":"succeeded","payment_intent":"pi_proof","metadata":{}}`
	s, _, posts := pagedStripe(t, []string{proofList(true, foreign), proofList(false, proofRefund("re_ours", "failed", proofPI, proofKey, 1000, "eur"))})
	got, err := s.ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
	if err != nil || got.ProviderRef != "re_ours" || *posts != 0 {
		t.Fatalf("proof = %+v err=%v posts=%d, want re_ours and no POST", got, err, *posts)
	}
	d, _, dposts := pagedStripe(t, []string{proofList(false, foreign)})
	res, err := d.Refund(context.Background(), proofPI, proofKey, 1000, "EUR")
	if err != nil || res.Outcome != Refunded || *dposts != 1 {
		t.Fatalf("dispatch = %+v err=%v posts=%d, want one POST", res, err, *dposts)
	}
}

// The control for RA5-1: an empty FIRST page that says it is the last one is a valid empty
// listing. Dispatch submits once; the proof finds no stamped refund, which is inconclusive for
// that reason and not because the listing was refused.
func TestStripeListingAcceptsAnEmptyFirstAndFinalPage(t *testing.T) {
	s, queries, posts := pagedStripe(t, []string{proofList(false)})
	got, err := s.ProveRefundFailure(context.Background(), proofPI, proofKey, 1000, "EUR")
	if !errors.Is(err, ErrRefundProofInconclusive) || err.Error() != ErrRefundProofInconclusive.Error()+": no refund carries this key" || got.ProviderRef != "" || *posts != 0 {
		t.Fatalf("proof = %+v err=%v posts=%d, want inconclusive because no refund carries the key", got, err, *posts)
	}
	if len(*queries) != 1 {
		t.Fatalf("listing pages = %d, want 1", len(*queries))
	}
	d, _, dposts := pagedStripe(t, []string{proofList(false)})
	res, err := d.Refund(context.Background(), proofPI, proofKey, 1000, "EUR")
	if err != nil || res.Outcome != Refunded || *dposts != 1 {
		t.Fatalf("dispatch = %+v err=%v posts=%d, want one POST", res, err, *dposts)
	}
}
