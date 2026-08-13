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
