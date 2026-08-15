//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TKT-250, on the wire. The allocation-set revision is required of the back office's
// credential and optional for the shared internal token, and the refusal is coded.
//
// The unit suite proves each half in isolation against a nil store, and the store suite
// proves the comparison against a real database. Neither can prove that the two travel
// correctly through the CONTRACT: `allocation_revision` is a new property on two schemas
// that are both `additionalProperties: false`, and inventory runs response validation, so
// a field the document does not declare fails here even when every handler is right.

// The back office's credential must present the revision, and gets a 400 without it.
func TestAStaffAllocationWriteWithoutARevisionIsRefusedOnTheWire(t *testing.T) {
	if inventoryStaffToken() == "" {
		t.Fatal("SMOKE_INVENTORY_STAFF_WRITE_TOKEN is not set: the credential could not be exercised")
	}
	slot, _ := publishedSlot(t, "Allocation Revision Staff Hall", 50)
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)

	// Otherwise VALID: the body is contract-clean and the credential is correct, so a
	// 400 can only be the missing revision. A probe the request validator rejects would
	// also answer 400 and would prove nothing about the rule under test.
	code, body := staffCredentialRequest(t, http.MethodPut, allocURL, map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": "rev-staff-missing", "cap": 5}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("staff write with no allocation_revision: %d %s, want 400", code, body)
	}
}

// The shared internal token keeps replacing unconditionally.
//
// This is the standing proof that the eight X-Internal-Token call sites across four smoke
// files did not have to change. If it ever fails, the compatibility half of TKT-250 is
// broken and those call sites are about to go red.
func TestTheInternalTokenStillReplacesWithoutARevision(t *testing.T) {
	slot, _ := publishedSlot(t, "Allocation Revision Internal Hall", 50)
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)

	code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": "rev-internal", "cap": 5}},
	})
	if code != http.StatusOK {
		t.Fatalf("internal replace without a revision: %d %s, want 200", code, body)
	}
}

// A stale revision is refused with the coded 409 the editor needs.
//
// Asserted on the WIRE because the Error schema is closed — `additionalProperties: false`
// and a closed `code` enum — so a code the document does not declare fails response
// validation even if the handler emits it. That is exactly how a coded refusal silently
// degrades to a generic one.
func TestAStaleRevisionIsRefusedWithItsCodeOnTheWire(t *testing.T) {
	if inventoryStaffToken() == "" {
		t.Fatal("SMOKE_INVENTORY_STAFF_WRITE_TOKEN is not set: the credential could not be exercised")
	}
	slot, _ := publishedSlot(t, "Allocation Revision Stale Hall", 50)
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	availabilityURL := fmt.Sprintf("%s/internal/slots/%s/availability?organizer_id=%s", inventoryURL, slot, organizerID)

	revision := readAllocationRevision(t, availabilityURL)

	// The first save wins, presenting the revision it read.
	code, body := staffCredentialRequest(t, http.MethodPut, allocURL, map[string]any{
		"organizer_id":        organizerID,
		"allocation_revision": revision,
		"allocations":         []map[string]any{{"channel": "rev-first", "cap": 10}},
	})
	if code != http.StatusOK {
		t.Fatalf("first save: %d %s", code, body)
	}

	// The second presents the SAME revision — the set has moved underneath it, which is
	// precisely the two-operator race.
	code, body = staffCredentialRequest(t, http.MethodPut, allocURL, map[string]any{
		"organizer_id":        organizerID,
		"allocation_revision": revision,
		"allocations":         []map[string]any{{"channel": "rev-second", "cap": 20}},
	})
	if code != http.StatusConflict {
		t.Fatalf("stale save: %d %s, want 409", code, body)
	}
	var refusal struct {
		Code    string `json:"code"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	if refusal.Code != "allocation_revision_mismatch" {
		t.Errorf("code=%q want allocation_revision_mismatch — the editor cannot tell a stale view "+
			"from an impossible cap without it", refusal.Code)
	}
	if refusal.Channel != "" {
		t.Errorf("the refusal named channel %q; staleness is a property of the whole set", refusal.Channel)
	}

	// And the stale set was NOT applied: the first save's row is still there and the
	// second's never arrived. Both directions, because a full-set replace deletes as
	// well as inserts.
	code, body = staffCredentialRequest(t, http.MethodGet, availabilityURL, nil)
	if code != http.StatusOK {
		t.Fatalf("re-read: %d %s", code, body)
	}
	var read struct {
		Channels []struct {
			Channel string `json:"channel"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode availability: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range read.Channels {
		seen[c.Channel] = true
	}
	if !seen["rev-first"] {
		t.Error("rev-first is gone: the refused replace deleted the committed set anyway")
	}
	if seen["rev-second"] {
		t.Error("rev-second is present: the refused replace was applied")
	}
}

// readAllocationRevision reads the set's current revision from the staff availability.
func readAllocationRevision(t *testing.T, availabilityURL string) int64 {
	t.Helper()
	code, body := staffCredentialRequest(t, http.MethodGet, availabilityURL, nil)
	if code != http.StatusOK {
		t.Fatalf("staff availability: %d %s", code, body)
	}
	var read struct {
		AllocationRevision *int64 `json:"allocation_revision"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode staff availability: %v", err)
	}
	if read.AllocationRevision == nil {
		t.Fatalf("no allocation_revision in the staff read: %s", body)
	}
	return *read.AllocationRevision
}
