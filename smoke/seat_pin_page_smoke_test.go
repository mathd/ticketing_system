//go:build smoke

package smoke_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestSeatPinPageLive501WorstCaseRows exercises the full 500-plus-one page cap boundary against live services (TKT-143).
// It seeds a published seat map with 501 seats, pins all 501 seats with exact-limit values (200-char seat identities
// and 45-char pinned_by, constructed from '&'), fetches a full page of 500 rows, asserts the raw body contains '&'
// (HTML-escaped into 6-byte \u0026 sequences), confirms the payload fits under maxSeatPinPageBytes (828511),
// verifies decoding through the consumer model, and fetches page 2 using the keyset cursor to assert row 501 is reachable.
//
// Mutation that makes this test red:
// Setting maxSeatPinPageBytes expectation below 828511 (e.g. 800000) causes the 500-row page size check to fail.
func TestSeatPinPageLive501WorstCaseRows(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)

	// Seed venue and draft seat map
	venue := created(t, catalog+"/venues", map[string]any{
		"name": "Live Pin Cap Venue " + suffix, "ga_capacity": 1000,
	})
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"name": "Live Pin Cap Map " + suffix,
	})
	mapID := fmt.Sprint(seatMap["id"])

	// Section name is "&" (1 char), row label is "&" (1 char).
	// Identity format is section + "/" + row + "/" + seatLabel.
	// Prefix "&/&/" is 4 chars.
	// To reach the exact 200-char limit for seat_identity:
	// seatLabel must be 196 chars: 191 '&' + 5 digit index (e.g. "%05d").
	section := created(t, catalog+"/seat-maps/"+mapID+"/sections", map[string]any{
		"name": "&", "position": 1,
	})
	row := created(t, catalog+"/seat-maps/"+mapID+"/rows", map[string]any{
		"section_id": section["id"], "label": "&", "position": 1,
	})

	const rowCount = 501
	identities := make([]string, rowCount)
	for i := 1; i <= rowCount; i++ {
		seatLabel := fmt.Sprintf("%s%05d", strings.Repeat("&", 191), i)
		created(t, catalog+"/seat-maps/"+mapID+"/seats", map[string]any{
			"row_id":   row["id"],
			"label":    seatLabel,
			"position": i,
		})
		identities[i-1] = fmt.Sprintf("&/&/%s", seatLabel)
	}

	// Publish the seat map so seats can be pinned
	if code, body := postJSON(t, catalog+"/seat-maps/"+mapID+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}

	// Create 501 pins with exact-limit 45-char pinned_by built entirely from '&'
	pinnedBy := strings.Repeat("&", 45)
	pinURL := fmt.Sprintf("%s/internal/seat-maps/%s/pins", catalogURL, mapID)
	code, body := internalJSON(t, http.MethodPost, pinURL, "", map[string]any{
		"organizer_id":    organizerID,
		"seat_identities": identities,
		"pinned_by":       pinnedBy,
	})
	if code != http.StatusOK {
		t.Fatalf("pin seats: %d %s", code, body)
	}

	// Clean up created pins when test finishes
	t.Cleanup(func() {
		unpinURL := fmt.Sprintf("%s/internal/seat-maps/%s/unpins", catalogURL, mapID)
		internalJSON(t, http.MethodPost, unpinURL, "", map[string]any{
			"organizer_id":    organizerID,
			"seat_identities": identities,
			"pinned_by":       pinnedBy,
		})
	})

	// 1. Fetch full page of 500 pins
	listURL := fmt.Sprintf("%s/internal/seat-map-pins?limit=500", catalogURL)
	code, rawBody := internalJSON(t, http.MethodGet, listURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("list pins: %d %s", code, rawBody)
	}

	// Assert the raw body contains '&' (or HTML-escaped \u0026)
	if !strings.Contains(string(rawBody), `\u0026`) && !strings.Contains(string(rawBody), "&") {
		t.Fatalf("expected raw body to contain & or \\u0026")
	}

	// Assert the raw body does not exceed the derived cap (828511 bytes)
	const maxSeatPinPageBytes = 828511
	if len(rawBody) > maxSeatPinPageBytes {
		t.Fatalf("raw body length %d exceeds maxSeatPinPageBytes %d", len(rawBody), maxSeatPinPageBytes)
	}

	// Assert decodes through the real consumer path
	var firstPage struct {
		Pins []struct {
			ID           uuid.UUID `json:"id"`
			OrganizerID  uuid.UUID `json:"organizer_id"`
			SeatMapID    uuid.UUID `json:"seat_map_id"`
			SeatIdentity string    `json:"seat_identity"`
			PinnedBy     string    `json:"pinned_by"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(rawBody, &firstPage); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if len(firstPage.Pins) != 500 {
		t.Fatalf("first page pins = %d, want 500", len(firstPage.Pins))
	}
	if len(firstPage.Pins[0].SeatIdentity) != 200 {
		t.Fatalf("seat identity length = %d, want 200", len(firstPage.Pins[0].SeatIdentity))
	}
	if len(firstPage.Pins[0].PinnedBy) != 45 {
		t.Fatalf("pinned_by length = %d, want 45", len(firstPage.Pins[0].PinnedBy))
	}

	// 2. Fetch the next page and assert row 501 is reachable
	after := firstPage.Pins[499].ID
	nextURL := fmt.Sprintf("%s/internal/seat-map-pins?after=%s&limit=500", catalogURL, after)
	code2, rawBody2 := internalJSON(t, http.MethodGet, nextURL, "", nil)
	if code2 != http.StatusOK {
		t.Fatalf("second page GET: %d %s", code2, rawBody2)
	}
	var secondPage struct {
		Pins []struct {
			ID           uuid.UUID `json:"id"`
			OrganizerID  uuid.UUID `json:"organizer_id"`
			SeatMapID    uuid.UUID `json:"seat_map_id"`
			SeatIdentity string    `json:"seat_identity"`
			PinnedBy     string    `json:"pinned_by"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(rawBody2, &secondPage); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(secondPage.Pins) < 1 {
		t.Fatalf("second page pins = %d, want >= 1", len(secondPage.Pins))
	}

	found501 := false
	for _, p := range secondPage.Pins {
		if p.SeatIdentity == identities[500] && p.PinnedBy == pinnedBy {
			found501 = true
			break
		}
	}
	if !found501 {
		t.Fatalf("row 501 (%s) not found on second page", identities[500])
	}
}
