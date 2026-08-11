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
	"strings"
	"sync"
	"sync/atomic"
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

// A partner sells: availability, hold, confirm — the whole COS path, end to end.
//
// This is also the smoke DRIVER for all three documented 2xx partner operations.
// The coverage gate (smoke/coverage_test.go) fails the entire suite for any
// documented 2xx operation nothing exercises, and its allowlist is empty; a real
// driver was written rather than the first allowlist entry since it was drained.
func TestAPartnerSellsThroughItsOwnChannel(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set: the partner suite would silently prove nothing")
	}
	slot, tt := publishedSlot(t, "Partner Hall", 10)

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

	// 2. Hold against the partner's own channel allocation.
	code, body = partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", "partner-res-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("partner reservation: %d %s", code, body)
	}
	var res struct {
		ID string `json:"reservation_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.ID == "" {
		t.Fatalf("partner reservation carried no id: %s", body)
	}

	// 3. Confirm it into a sale.
	code, body = partnerDo(t, http.MethodPost, "/api/commerce/partners/orders", "partner-ord-"+slot,
		map[string]any{"organizer_id": organizerID, "reservation_id": res.ID})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("partner order: %d %s", code, body)
	}
}

// An unauthenticated partner call is refused AT THE EDGE, by the validator.
//
// The unit tier asserts the same refusal against a nil database; this asserts it
// survives the gateway, the real router and a real commerce with a real database
// behind it — which is the configuration where a mis-wired scope slot or a
// forgotten declaration would actually show up.
func TestPartnerRoutesAreClosedWithoutACredentialThroughTheGateway(t *testing.T) {
	slot, tt := publishedSlot(t, "Partner Closed Hall", 4)
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"availability", http.MethodGet, "/api/commerce/partners/availability?slot_id=" + slot, nil},
		{"reservation", http.MethodPost, "/api/commerce/partners/reservations",
			map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1}},
		{"order", http.MethodPost, "/api/commerce/partners/orders",
			map[string]any{"organizer_id": organizerID, "reservation_id": slot}},
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

// A partner naming SOMEBODY ELSE'S organizer is refused, and the refusal comes
// from comparing against the credential rather than from the row not existing.
//
// The fixture uses a real, valid organizer id that is simply not the credential's
// — not a nonsense uuid — because a nonsense one would also fail for want of a
// ticket type, and the test would pass without the comparison existing at all.
func TestAPartnerCannotNameAnotherOrganizer(t *testing.T) {
	slot, tt := publishedSlot(t, "Partner Scope Hall", 4)
	_ = slot
	other := "00000000-0000-0000-0000-0000000000ff"
	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", "partner-scope-"+slot,
		map[string]any{"organizer_id": other, "ticket_type_id": tt, "quantity": 1})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the request names an organizer the credential was "+
			"not issued for: %s", code, body)
	}
	if !strings.Contains(string(body), "partner_scope_mismatch") {
		t.Fatalf("the refusal carries no machine-readable code: %s", body)
	}
}

// The no-oversell guarantee holds WITH A RESELLER PARTICIPATING.
//
// The counts are DERIVED from the capacity and the cap rather than written as
// literals, and the invariant is asserted alongside them. A test that pinned "6"
// and "4" would pin the behaviour; what the requirement actually says is that the
// pool never grants more than its capacity and the channel never grants more than
// its cap, and that is what fails if either bound breaks.
//
// One slot is sufficient here BECAUSE this design adds no per-reseller quota and
// no cross-slot counter: every count the decision reads is pool-scoped, which is
// what makes the single pool row lock enough (ADR-055 § presale codes). If a
// per-reseller quota is ever added, this fixture stops being adequate and the
// proof needs three slots.
func TestNoOversellWithAResellerParticipating(t *testing.T) {
	const capacity, cap = 10, 6
	slot, tt := publishedSlot(t, "Partner Contention Hall", capacity)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": partnerChannel(), "cap": cap}},
	}); code != http.StatusOK {
		t.Fatalf("allocate: %d %s", code, body)
	}

	var partnerGranted, publicGranted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				code, _ := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations",
					fmt.Sprintf("cont-partner-%s-%d", slot, i),
					map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1})
				if code == http.StatusCreated {
					partnerGranted.Add(1)
				} else if code != http.StatusConflict {
					t.Errorf("partner reserve %d: unexpected status %d", i, code)
				}
				return
			}
			code, _ := postWithKeyAsync(t, gatewayURL+"/api/inventory/holds",
				fmt.Sprintf("cont-public-%s-%d", slot, i),
				map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
			if code == http.StatusCreated {
				publicGranted.Add(1)
			} else if code != http.StatusConflict {
				t.Errorf("public hold %d: unexpected status %d", i, code)
			}
		}(i)
	}
	wg.Wait()

	partner, public := int(partnerGranted.Load()), int(publicGranted.Load())

	// The invariants, said without naming the implementation.
	if partner > cap {
		t.Errorf("the reseller channel granted %d claims against a cap of %d: a channel sold past "+
			"its own allocation", partner, cap)
	}
	if partner+public > capacity {
		t.Errorf("the pool granted %d claims against a capacity of %d: the no-oversell guarantee "+
			"does not hold with a reseller participating", partner+public, capacity)
	}
	// And the allocation really is RESERVED for the channel — the public share is
	// bounded by what the allocation does not hold. Without this, a run that
	// granted nothing to the partner would satisfy every bound above.
	if public > capacity-cap {
		t.Errorf("public claims took %d of a pool whose channel allocation reserves %d of %d: "+
			"unchannelled demand ate capacity held for a channel", public, cap, capacity)
	}
	if partner == 0 {
		t.Error("the reseller was granted nothing; this run proves no bound at all")
	}
}
