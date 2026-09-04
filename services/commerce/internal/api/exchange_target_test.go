package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// What the exchange target hold sends to inventory (TKT-246, ai-review [high] F2).
//
// The decision under test is one condition, and it is an authorization decision: the
// channel is forwarded only when the source carried a RESELLER. Its own test because
// the store-tier projection test can only prove the two facts are read independently —
// what is done with them happens here.

// exchangeHoldRequest is what holdExchangeTarget actually sent: the body, and — added
// after ai-review pass 4 — WHERE and with what credential.
//
// The body alone was not enough, and that gap hid a real regression: the stub answered
// 201 whatever the URL, so routing a reseller-bearing hold to the PUBLIC route (whose
// schema now refuses reseller_id, and whose handler passes uuid.Nil) left this test
// green while every reseller exchange was broken.
type exchangeHoldRequest struct {
	body  map[string]any
	path  string
	token string
}

// capturedExchangeHold drives holdExchangeTarget against a stub inventory.
func capturedExchangeHold(t *testing.T, src commercestore.ExchangeSource) exchangeHoldRequest {
	t.Helper()
	var got exchangeHoldRequest
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.body = map[string]any{}
		_ = json.Unmarshal(raw, &got.body)
		got.path = r.URL.Path
		got.token = r.Header.Get("X-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"hold_id":"` + uuid.New().String() + `"}`))
	}))
	defer inventory.Close()

	srv := newTestServer(nil, http.DefaultClient, "", inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/exchanges", nil)
	res := priceResolution{
		PerformanceID: uuid.New(),
		ResolvedPrice: resolvedMoney{Amount: 2500, Currency: "EUR"},
	}
	if _, err := srv.holdExchangeTarget(req, uuid.New(), uuid.New(), uuid.New(), 2, res, src); err != nil {
		t.Fatalf("hold exchange target: %v", err)
	}
	if got.body == nil {
		t.Fatal("holdExchangeTarget never called inventory")
	}
	return got
}

// A RESELLER-attributed source forwards both its channel and its reseller.
//
// The positive half: an exchange of a partner sale must consume the SAME allocation the
// source came from, or the replacement is repriced on the channel while eating public
// stock — the original seam, arriving one exchange later.
func TestAnAttributedExchangeTargetCarriesChannelAndReseller(t *testing.T) {
	channel := "reseller-acme"
	reseller := uuid.New()
	got := capturedExchangeHold(t, commercestore.ExchangeSource{
		ChannelCode: &channel, ResellerID: &reseller, Currency: "EUR",
	})

	if got.body["channel"] != channel {
		t.Fatalf("exchange target sent channel=%v, want %q — without it the replacement consumes "+
			"PUBLIC stock while pricing on the source's channel", got.body["channel"], channel)
	}
	if got.body["reseller_id"] != reseller.String() {
		t.Fatalf("exchange target sent reseller_id=%v, want %s — inventory compares it to sold_by, "+
			"so the partner's own bound allocation would refuse its own exchange",
			got.body["reseller_id"], reseller)
	}
	// WHERE it went, and with what credential (ai-review pass 4).
	//
	// A reseller-bearing hold on the PUBLIC route is refused by the contract —
	// HoldCreate has no reseller_id and is additionalProperties:false — and even past
	// the validator the public handler passes uuid.Nil, so a bound allocation would
	// refuse the partner its own exchange. Asserting only the body missed exactly this.
	if got.path != "/internal/holds" {
		t.Fatalf("a reseller-bearing exchange target went to %q, want /internal/holds — the public "+
			"route's schema REFUSES reseller_id, so every reseller exchange fails at the "+
			"validator", got.path)
	}
	if got.token == "" {
		t.Fatal("the internal exchange hold carried no X-Internal-Token — the route requires the " +
			"service credential and would answer 401")
	}
}

// A PUBLIC source with a channel forwards NEITHER. The load-bearing case.
//
// `channel_code != NULL, reseller_id == NULL` is a legal and routine row: the public
// reserve is unauthenticated and persists whatever channel its body named, because fee
// resolution reads it. Only the inventory forward was withheld.
//
// If the exchange forwards that channel, an unauthenticated buyer picks a reseller's
// channel today and the exchange consumes that reseller's allocation tomorrow, with no
// credential anywhere in the story. That is the TKT-240 bypass with a delay, and since
// every allocation is currently unbound it would have been reachable on day one.
//
// The reseller is the authority; a channel in a request body is not evidence of one.
func TestAPublicExchangeTargetCarriesNoChannelEvenWhenTheSourceNamedOne(t *testing.T) {
	channel := "reseller-acme" // typed by an anonymous buyer, never authenticated
	got := capturedExchangeHold(t, commercestore.ExchangeSource{
		ChannelCode: &channel, ResellerID: nil, Currency: "EUR",
	})

	if v, present := got.body["channel"]; present {
		t.Fatalf("an UNAUTHENTICATED source's channel (%v) reached inventory on the exchange "+
			"target. A public buyer naming a reseller's channel would then consume that "+
			"reseller's allocation through an exchange, with no credential — the bypass this "+
			"ticket closes, one step later in time", v)
	}
	if v, present := got.body["reseller_id"]; present {
		t.Fatalf("a public source sent reseller_id=%v, want the field absent entirely", v)
	}
	// And it stays on the PUBLIC route: routing a public exchange internally would work,
	// but it would spend the service credential on a request that has no seller to
	// authorize and quietly widen what the internal route is used for.
	if got.path != "/holds" {
		t.Fatalf("a public exchange target went to %q, want /holds", got.path)
	}
	if got.token != "" {
		t.Fatalf("a public exchange target carried an internal token (%q)", got.token)
	}
}

// And a source with neither stays exactly as it was.
func TestAnUnchannelledExchangeTargetIsUnchanged(t *testing.T) {
	got := capturedExchangeHold(t, commercestore.ExchangeSource{Currency: "EUR"})
	if _, present := got.body["channel"]; present {
		t.Fatal("an unchannelled source sent a channel")
	}
	if _, present := got.body["reseller_id"]; present {
		t.Fatal("an unchannelled source sent a reseller_id")
	}
}

// An exchange reprices on the source's channel ONLY when a reseller vouched for it
// (TKT-248, ai-review pass 2 [medium]; wiring fixed after pass 3 [medium]).
//
// The sibling of TestAPublicExchangeTargetCarriesNoChannelEvenWhenTheSourceNamedOne
// above, one step earlier in the request: that one stops a legacy channel reaching
// INVENTORY, this one stops it reaching catalog's PRICE resolution. Different
// decisions on different values -- one decides whose allocation is consumed, the
// other what the buyer is charged for a ticket type named in THIS request against
// CURRENT rules -- so deleting either guard leaves the other's test green.
//
// IT ASSERTS THE QUERY CATALOG ACTUALLY RECEIVED, not what a helper returned. An
// earlier version called repricingChannel() directly, which merely restated that
// function: reverting the call site to pass src.ChannelCode left it green while
// reopening the hole. Driving repriceExchangeTarget -- the one place the decision
// and the resolve call live together -- is what makes that revert fail here.
//
// `channel_code != NULL, reseller_id == NULL` is a legal historical row: before
// ADR-060 a public reserve persisted whatever channel its unauthenticated body
// named. The row keeps that attribution; this only stops it deciding money.
func TestExchangeRepricingTakesTheChannelOnlyFromAResellerSource(t *testing.T) {
	channel := "reseller-acme"
	reseller := uuid.New()

	for name, tc := range map[string]struct {
		src       commercestore.ExchangeSource
		wantQuery string
	}{
		"a public source's channel does not price the target": {
			// Typed by an anonymous buyer, never authenticated.
			src:       commercestore.ExchangeSource{ChannelCode: &channel, ResellerID: nil, Quantity: 2, Currency: "EUR"},
			wantQuery: "",
		},
		"a reseller source keeps pricing on its own channel": {
			src:       commercestore.ExchangeSource{ChannelCode: &channel, ResellerID: &reseller, Quantity: 2, Currency: "EUR"},
			wantQuery: "channel_code=reseller-acme",
		},
		"a public source with no channel is unchanged": {
			src:       commercestore.ExchangeSource{ChannelCode: nil, ResellerID: nil, Quantity: 2, Currency: "EUR"},
			wantQuery: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var asked string
			var called bool
			catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked, called = r.URL.RawQuery, true
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				// Echo whichever channel was asked about: commerce validates the
				// echo (TKT-237), so a stub answering null would fail every
				// channelled reprice before this test could observe anything.
				var ch *string
				if q := r.URL.Query().Get("channel_code"); q != "" {
					c := q
					ch = &c
				}
				_, _ = w.Write([]byte(resolutionBodyFor(2500, true, ch)))
			}))
			defer catalog.Close()

			srv := newTestServer(nil, http.DefaultClient, catalog.URL, "", "", "secret")
			req := httptest.NewRequest(http.MethodPost, "/exchanges", nil)
			if _, err := srv.repriceExchangeTarget(req, uuid.MustParse(pricingTT),
				uuid.MustParse(pricingOrg), tc.src); err != nil {
				t.Fatalf("reprice: %v", err)
			}
			if !called {
				t.Fatal("catalog was never asked to price the target: this test cannot observe " +
					"the decision it exists to check")
			}
			if asked != tc.wantQuery {
				if tc.wantQuery == "" {
					t.Fatalf("catalog was asked %q, want no channel. The target is a DIFFERENT "+
						"ticket type priced against CURRENT rules, so pricing on a channel no "+
						"credential vouched for lets a long-past unauthenticated choice pick the "+
						"basis for a new sale (ADR-060).", asked)
				}
				t.Fatalf("catalog was asked %q, want %q — an authorized channelled sale must "+
					"still exchange onto its own channel's economics", asked, tc.wantQuery)
			}
		})
	}
}
