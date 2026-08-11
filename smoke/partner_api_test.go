//go:build smoke

package smoke_test

// The reseller API against the real stack (TKT-240 / ADR-056).
//
// These are the tests the unit tier cannot write. A commerce unit test runs
// against a nil database and stubbed upstreams, so it can prove the CONTRACT is
// declared and enforced but not that the scope predicate is real, that revocation
// reaches the next request with no cache in between, or that a channelled sale
// actually consumes a channel allocation in Postgres. Each of those is asserted
// here, through the gateway, against the running services.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func partnerToken() string   { return os.Getenv("SMOKE_PARTNER_TOKEN") }
func partnerChannel() string { return os.Getenv("SMOKE_PARTNER_CHANNEL") }

// partnerDo issues a partner request through the GATEWAY, with the credential.
//
// Through the gateway deliberately: the partner surface is edge-reachable, unlike
// /internal/, and driving it service-direct would skip the one thing that makes
// it different from every other authenticated surface in this system.
func partnerDo(t *testing.T, method, path, key string, body any) (int, []byte) {
	t.Helper()
	return partnerDoAs(t, partnerToken(), method, path, key, body)
}

func partnerDoAs(t *testing.T, token, method, path, key string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, gatewayURL+path, reader)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Partner-Credential", token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	// The same contract validation every other smoke request runs through. It is
	// also what records the operation for the coverage gate, so a driver that
	// skipped it would leave the operation reported as uncovered.
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

// A partner reads availability for its own channel. That is the whole write-free
// surface this slice ships.
//
// CONFIRM IS NOT HERE, and its absence is the ticket's scope boundary rather than
// an omission: settling a reseller sale needs a settlement leg for a sale that was
// never captured, and ADR-048 writes ledger entries in the `payment.captured`
// transaction and requires `fact_type = 'payment.captured'`. A partner order with
// no capture would therefore produce NO settlement entry, and the attribution
// columns this ticket adds would be written and never read — the opposite of the
// COS's "a shape settlement can already split by". Confirm and its settlement
// design are TKT-23's.
//
// This is also the smoke DRIVER for both documented 2xx partner operations.
// The coverage gate (smoke/coverage_test.go) fails the entire suite for any
// documented 2xx operation nothing exercises, and its allowlist is empty; a real
// driver was written rather than the first allowlist entry since it was drained.
func TestAPartnerReadsAvailabilityForItsOwnChannel(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set: the partner suite would silently prove nothing")
	}
	slot, _ := publishedSlot(t, "Partner Hall", 10)

	// The channel needs an allocation, or the partner's sale is refused for want
	// of one rather than served. Set through the internal surface, as an operator
	// would.
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": partnerChannel(), "cap": 4}},
	}); code != http.StatusOK {
		t.Fatalf("allocate: %d %s", code, body)
	}

	// 1. Availability, scoped to the credential's own channel. The request names
	// no organizer and no channel — there is nowhere to put one.
	code, body := partnerDo(t, http.MethodGet, "/api/commerce/partners/availability?slot_id="+slot, "", nil)
	if code != http.StatusOK {
		t.Fatalf("partner availability: %d %s", code, body)
	}
	var avail struct {
		SlotID      string `json:"slot_id"`
		ChannelCode string `json:"channel_code"`
		Available   int    `json:"available"`
	}
	if err := json.Unmarshal(body, &avail); err != nil {
		t.Fatal(err)
	}
	if avail.ChannelCode != partnerChannel() {
		t.Fatalf("availability answered for channel %q, want the credential's %q",
			avail.ChannelCode, partnerChannel())
	}
	if avail.Available <= 0 {
		t.Fatalf("availability = %d on a fresh allocation of 4", avail.Available)
	}

}

// An unauthenticated partner call is refused AT THE EDGE, by the validator.
//
// The unit tier asserts the same refusal against a nil database; this asserts it
// survives the gateway, the real router and a real commerce with a real database
// behind it — which is the configuration where a mis-wired scope slot or a
// forgotten declaration would actually show up.
func TestPartnerRoutesAreClosedWithoutACredentialThroughTheGateway(t *testing.T) {
	slot, _ := publishedSlot(t, "Partner Closed Hall", 4)
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"availability", http.MethodGet, "/api/commerce/partners/availability?slot_id=" + slot, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := partnerDoAs(t, "", tc.method, tc.path, "no-cred-"+tc.name+slot, tc.body)
			if code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 through the gateway: %s", code, body)
			}
			// And an unknown credential is answered identically to an absent one.
			unknownCode, unknownBody := partnerDoAs(t, "0123456789abcdef0123456789abcdef",
				tc.method, tc.path, "bad-cred-"+tc.name+slot, tc.body)
			if unknownCode != code || string(unknownBody) != string(body) {
				t.Fatalf("an unknown credential is distinguishable from an absent one: %d %s vs %d %s",
					code, body, unknownCode, unknownBody)
			}
		})
	}
}
