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

// capturedExchangeHold drives holdExchangeTarget against a stub inventory and returns
// the body it sent.
func capturedExchangeHold(t *testing.T, src commercestore.ExchangeSource) map[string]any {
	t.Helper()
	var got map[string]any
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = map[string]any{}
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"hold_id":"` + uuid.New().String() + `"}`))
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, "", inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/exchanges", nil)
	res := priceResolution{
		PerformanceID: uuid.New(),
		ResolvedPrice: resolvedMoney{Amount: 2500, Currency: "EUR"},
	}
	if _, err := srv.holdExchangeTarget(req, uuid.New(), uuid.New(), uuid.New(), 2, res, src); err != nil {
		t.Fatalf("hold exchange target: %v", err)
	}
	if got == nil {
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
	body := capturedExchangeHold(t, commercestore.ExchangeSource{
		ChannelCode: &channel, ResellerID: &reseller, Currency: "EUR",
	})

	if body["channel"] != channel {
		t.Fatalf("exchange target sent channel=%v, want %q — without it the replacement consumes "+
			"PUBLIC stock while pricing on the source's channel", body["channel"], channel)
	}
	if body["reseller_id"] != reseller.String() {
		t.Fatalf("exchange target sent reseller_id=%v, want %s — inventory compares it to sold_by, "+
			"so the partner's own bound allocation would refuse its own exchange",
			body["reseller_id"], reseller)
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
	body := capturedExchangeHold(t, commercestore.ExchangeSource{
		ChannelCode: &channel, ResellerID: nil, Currency: "EUR",
	})

	if v, present := body["channel"]; present {
		t.Fatalf("an UNAUTHENTICATED source's channel (%v) reached inventory on the exchange "+
			"target. A public buyer naming a reseller's channel would then consume that "+
			"reseller's allocation through an exchange, with no credential — the bypass this "+
			"ticket closes, one step later in time", v)
	}
	if v, present := body["reseller_id"]; present {
		t.Fatalf("a public source sent reseller_id=%v, want the field absent entirely", v)
	}
}

// And a source with neither stays exactly as it was.
func TestAnUnchannelledExchangeTargetIsUnchanged(t *testing.T) {
	body := capturedExchangeHold(t, commercestore.ExchangeSource{Currency: "EUR"})
	if _, present := body["channel"]; present {
		t.Fatal("an unchannelled source sent a channel")
	}
	if _, present := body["reseller_id"]; present {
		t.Fatal("an unchannelled source sent a reseller_id")
	}
}
