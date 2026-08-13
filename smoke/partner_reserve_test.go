//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
)

func partnerReseller() string { return os.Getenv("SMOKE_PARTNER_RESELLER_ID") }

// A partner sells from its OWN channel allocation, end to end (TKT-246).
//
// The whole point of the ticket, driven through the gateway against real services:
// a bound allocation is consumed by the reseller it belongs to and by nobody else.
// It is also the smoke DRIVER for createPartnerReservation — the coverage gate fails
// the suite for any documented 2xx operation nothing exercises, and its allowlist is
// empty.
//
// The two halves are inseparable and this test asserts BOTH, because either alone is
// a defect that has already shipped here: consuming the channel without authorization
// is what got TKT-240 reverted, and authorization without consumption is the seam
// still being open.
func TestAPartnerSellsFromItsOwnBoundAllocation(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set: the partner suite would silently prove nothing")
	}
	if partnerReseller() == "" {
		t.Fatal("SMOKE_PARTNER_RESELLER_ID is not set: the binding could not be asserted")
	}
	slot, ticketType := publishedSlot(t, "Partner Reserve Hall", 20)

	// An allocation BOUND to this reseller, set through the internal surface as an
	// operator would. sold_by is what makes this the partner's stock rather than
	// anyone's.
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": partnerChannel(), "cap": 6, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	// The credentialled partner sells.
	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", "partner-reserve-1",
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("partner reserve: %d %s — the reseller was refused its OWN bound allocation", code, body)
	}
	var reservation struct {
		ID string `json:"reservation_id"`
	}
	_ = json.Unmarshal(body, &reservation)

	// And the sale consumed THAT CHANNEL's allocation, not public stock. This is the
	// assertion the whole ticket exists for: before it, a channelled sale took the
	// channel's fees while draining the public pool.
	availCode, availBody := partnerDo(t, http.MethodGet,
		"/api/commerce/partners/availability?slot_id="+slot, "", nil)
	if availCode != http.StatusOK {
		t.Fatalf("partner availability: %d %s", availCode, availBody)
	}
	var avail struct {
		Available int `json:"available"`
	}
	if err := json.Unmarshal(availBody, &avail); err != nil {
		t.Fatal(err)
	}
	if avail.Available != 4 {
		t.Fatalf("channel availability = %d after selling 2 of a 6 cap, want 4 — if this is still "+
			"6 the sale consumed PUBLIC inventory and the channel seam is open", avail.Available)
	}

	// A RETRY of the same channelled reserve replays rather than 409s.
	//
	// The persisted-reservation replay omitted the channel, so inventory saw a
	// different request than the first attempt and refused it against its
	// fingerprint. A partner retrying a timeout would get a hard failure on a hold it
	// already owns.
	retryCode, retryBody := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations",
		"partner-reserve-1", map[string]any{
			"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2,
		})
	if retryCode != http.StatusCreated {
		t.Fatalf("retry of a channelled partner reserve: %d %s — want 201 replaying the SAME "+
			"reservation; a 409 here is the fingerprint mismatch the replay path caused", retryCode, retryBody)
	}
	var replayed struct {
		ID string `json:"reservation_id"`
	}
	_ = json.Unmarshal(retryBody, &replayed)
	if replayed.ID != reservation.ID {
		t.Fatalf("retry produced reservation %s, want the original %s — a second hold was placed "+
			"against the same allocation", replayed.ID, reservation.ID)
	}
	// And the replay consumed nothing further.
	_, availBody2 := partnerDo(t, http.MethodGet, "/api/commerce/partners/availability?slot_id="+slot, "", nil)
	var avail2 struct {
		Available int `json:"available"`
	}
	_ = json.Unmarshal(availBody2, &avail2)
	if avail2.Available != 4 {
		t.Fatalf("channel availability = %d after a REPLAY, want 4 — the retry consumed capacity "+
			"a second time", avail2.Available)
	}
}

// Inventory's internal hold consumes a BOUND allocation for its own seller, and
// refuses every other caller (TKT-246, ai-review pass 3).
//
// Driven service-direct rather than through commerce, for two reasons. It is the smoke
// DRIVER for `createInternalHold` — the coverage gate observes the operation being
// called, and a request that reaches inventory via commerce records commerce's
// operation, not inventory's. And it is the only place the route's own contract is
// exercised: through commerce, a bug in inventory's guard and a bug in commerce's
// forwarding look identical.
//
// The three callers mirror the store-tier test one layer up, because the store test
// cannot see the route: it drives the store directly, which is exactly how the
// [critical] in pass 3 survived a green suite.
func TestTheInternalHoldRouteEnforcesTheSellerBinding(t *testing.T) {
	if partnerReseller() == "" {
		t.Fatal("SMOKE_PARTNER_RESELLER_ID is not set: the binding could not be asserted")
	}
	slot, ticketType := publishedSlot(t, "Internal Hold Hall", 20)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": partnerChannel(), "cap": 6, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	hold := func(key string, reseller any) (int, []byte) {
		t.Helper()
		body := map[string]any{
			"organizer_id": organizerID, "slot_id": slot, "ticket_type_id": ticketType,
			"quantity": 2, "unit_amount": 2500, "currency": "EUR", "channel": partnerChannel(),
		}
		if reseller != nil {
			body["reseller_id"] = reseller
		}
		return internalJSON(t, http.MethodPost, inventoryURL+"/internal/holds", key, body)
	}

	// The bound seller sells.
	if code, body := hold("internal-own", partnerReseller()); code != http.StatusCreated {
		t.Fatalf("the bound reseller was refused its own allocation through /internal/holds: %d %s",
			code, body)
	}
	// A different reseller does not.
	if code, body := hold("internal-other", "00000000-0000-0000-0000-0000000009e9"); code != http.StatusConflict {
		t.Fatalf("a DIFFERENT reseller consumed the allocation: %d %s — want 409", code, body)
	}
	// And a caller presenting no reseller at all does not.
	if code, body := hold("internal-anon", nil); code != http.StatusConflict {
		t.Fatalf("a caller with NO reseller identity consumed a bound allocation: %d %s — want 409",
			code, body)
	}
}

// THE BYPASS PROBE. An unauthenticated caller cannot consume a bound allocation.
//
// Executed, not asserted. AGENTS.md: a security claim is a hypothesis until it is
// executed, and this exact claim has been wrong twice on this codebase — TKT-240's
// probe reached inventory with channel=reseller-acme and no credential presented, and
// two consecutive adversarial passes rejected two different plausible claims about the
// same guard. What settled it was running the sequence and watching the result.
//
// So this drives the real request an attacker would send, through the gateway, and
// then checks the allocation itself rather than trusting the status code: a 201 that
// consumed public stock and a 201 that drained the reseller are indistinguishable
// from the response alone.
func TestAnUnauthenticatedCallerCannotConsumeABoundAllocation(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set: the probe could not be scoped")
	}
	slot, ticketType := publishedSlot(t, "Bypass Probe Hall", 20)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": partnerChannel(), "cap": 6, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	before := partnerChannelAvailable(t, slot)

	// The probe: the PUBLIC reserve, naming the reseller's channel in the body, with
	// no credential of any kind.
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "bypass-probe",
		map[string]any{
			"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2,
			"channel_code": partnerChannel(),
		})

	// The request may well SUCCEED — it is a legitimate public sale, and it consumes
	// PUBLIC stock. What must not happen is that it moves the reseller's allocation.
	// Asserting a refusal here would be asserting the wrong thing and would go green
	// for the wrong reason if the channel simply stopped being forwarded.
	after := partnerChannelAvailable(t, slot)
	if after != before {
		t.Fatalf("an UNAUTHENTICATED request naming channel %q moved the reseller's allocation: "+
			"%d -> %d (probe answered %d %s). This is exactly the bypass TKT-240 was reverted "+
			"for: forwarding a body-supplied channel from an unauthenticated route lets any "+
			"caller drain a reseller's stock with no credential.",
			partnerChannel(), before, after, code, body)
	}
}

// The reseller-bearing hold is refused AT THE EDGE (TKT-246, ai-review pass 3).
//
// The first of the two guards, executed rather than asserted from the route table. The
// [critical] this closes was that `reseller_id` was reachable from the internet on the
// PUBLIC hold; the fix moves it behind /internal/, and this is the test that the move
// actually put it somewhere the gateway refuses.
//
// Both halves matter: the internal path must be edge-denied, and the public path must
// still work — a fix that closed the hole by breaking public holds would pass the first
// assertion alone.
func TestTheInternalHoldRouteIsUnreachableFromTheEdge(t *testing.T) {
	slot, ticketType := publishedSlot(t, "Edge Denial Hall", 10)
	body := map[string]any{
		"organizer_id": organizerID, "slot_id": slot, "ticket_type_id": ticketType,
		"quantity": 1, "unit_amount": 2500, "currency": "EUR",
	}

	code, out := postWithKey(t, gatewayURL+"/api/inventory/internal/holds", "edge-internal-hold", body)
	if code != http.StatusNotFound {
		t.Fatalf("POST /api/inventory/internal/holds answered %d %s, want 404 — the route that "+
			"accepts a reseller identity is reachable from the edge, which is the [critical] "+
			"this ticket closed", code, out)
	}

	// The PUBLIC hold still works through the gateway, so the assertion above is about
	// the /internal/ prefix and not about inventory being unreachable.
	if code, out := postWithKey(t, gatewayURL+"/api/inventory/holds", "edge-public-hold", body); code != http.StatusCreated {
		t.Fatalf("the public hold answered %d %s through the gateway, want 201 — if this is also "+
			"refused, the edge-denial assertion above proves nothing", code, out)
	}
}

// partnerChannelAvailable reads the bound channel's remaining allocation.
//
// Through the partner's own credentialled read, which is the only surface that
// answers per-channel from outside — and reading it with the credential also proves
// the number is the reseller's own, not a public aggregate that happens to match.
func partnerChannelAvailable(t *testing.T, slot string) int {
	t.Helper()
	code, body := partnerDo(t, http.MethodGet, "/api/commerce/partners/availability?slot_id="+slot, "", nil)
	if code != http.StatusOK {
		t.Fatalf("partner availability: %d %s", code, body)
	}
	var avail struct {
		Available int `json:"available"`
	}
	if err := json.Unmarshal(body, &avail); err != nil {
		t.Fatal(err)
	}
	return avail.Available
}
