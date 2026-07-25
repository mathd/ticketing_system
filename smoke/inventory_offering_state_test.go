//go:build smoke

// Pool offering state end-to-end (TKT-75 / US-012): closing or archiving a slot through
// Catalog stops Inventory from granting NEW holds — through the real stream, durable,
// resolver, store transaction and gateway contract — while live and confirmed claims
// keep their lifecycle untouched. Closure is reversible; archival is terminal.
package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func offeringAvailability(t *testing.T, slot string) (status string, available int32) {
	t.Helper()
	code, body := get(t, fmt.Sprintf("%s/api/inventory/slots/%s/availability?organizer_id=%s", gatewayURL, slot, organizerID), nil)
	if code != 200 {
		t.Fatalf("availability %d %s", code, body)
	}
	var a struct {
		OfferingStatus string `json:"offering_status"`
		Available      int32  `json:"available"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	return a.OfferingStatus, a.Available
}

func waitForOffering(t *testing.T, slot, want string) {
	t.Helper()
	retry(t, 20*time.Second, func() error {
		if status, _ := offeringAvailability(t, slot); status != want {
			return fmt.Errorf("offering_status=%s, want %s", status, want)
		}
		return nil
	})
}

func conflictCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode conflict body %s: %v", body, err)
	}
	return e.Code
}

func TestInventoryStopsOfferingArchivedAndClosedSlots(t *testing.T) {
	slot, _ := publishedSlot(t, "Offering State Hall", 10)
	holds := gatewayURL + "/api/inventory/holds"

	if status, _ := offeringAvailability(t, slot); status != "open" {
		t.Fatalf("fresh slot offering_status=%s, want open", status)
	}

	// A live hold taken before the closure — the buyer already in checkout.
	code, body := holdRequest(holds, "offering-live-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 2})
	if code != 201 {
		t.Fatalf("pre-closure hold: %d %s", code, body)
	}
	var live struct {
		ID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatal(err)
	}

	// Weather-close through Catalog; Inventory converges to closed with zero availability.
	if code, body := postJSON(t, fmt.Sprintf("%s/api/catalog/performances/%s/close", gatewayURL, slot), map[string]any{"reason": "storm"}); code != 200 {
		t.Fatalf("close: %d %s", code, body)
	}
	waitForOffering(t, slot, "closed")
	if _, available := offeringAvailability(t, slot); available != 0 {
		t.Fatalf("closed slot available=%d, want 0", available)
	}

	// A NEW hold is refused with the distinguishable reason — not "sold out".
	code, body = holdRequest(holds, "offering-closed-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != 409 || conflictCode(t, body) != "slot_closed" {
		t.Fatalf("hold on closed slot: %d %s, want 409/slot_closed", code, body)
	}

	// The pre-closure hold still finalizes and confirms: closure revokes nothing.
	for _, step := range []string{"finalize", "confirm"} {
		if code, body := internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/%s?organizer_id=%s", inventoryURL, live.ID, step, organizerID), "", nil); code != 200 {
			t.Fatalf("%s pre-closure hold: %d %s", step, code, body)
		}
	}

	// Reopen restores claimability.
	if code, body := postJSON(t, fmt.Sprintf("%s/api/catalog/performances/%s/reopen", gatewayURL, slot), nil); code != 200 {
		t.Fatalf("reopen: %d %s", code, body)
	}
	waitForOffering(t, slot, "open")
	if code, body := holdRequest(holds, "offering-reopened-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1}); code != 201 {
		t.Fatalf("hold on reopened slot: %d %s", code, body)
	}

	// Archival is terminal.
	if code, body := postJSON(t, fmt.Sprintf("%s/api/catalog/performances/%s/archive", gatewayURL, slot), nil); code != 200 {
		t.Fatalf("archive: %d %s", code, body)
	}
	waitForOffering(t, slot, "archived")
	code, body = holdRequest(holds, "offering-archived-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != 409 || conflictCode(t, body) != "slot_archived" {
		t.Fatalf("hold on archived slot: %d %s, want 409/slot_archived", code, body)
	}

	// Nothing was revoked along the way: the confirmed admission survives it all.
	_, _, confirmed, _ := staffAvailability(t, slot)
	if confirmed != 2 {
		t.Fatalf("confirmed=%d after close/reopen/archive, want 2", confirmed)
	}
}
