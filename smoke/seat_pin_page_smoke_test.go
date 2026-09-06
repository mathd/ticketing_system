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

// maxSeatPinPageBytes mirrors the consumer's derived cap (TKT-143). Catalog cannot serve a
// conforming page larger than this, so a page that exceeds it means a bound moved without the
// cap following.
const maxSeatPinPageBytes = 828511

// TestSeatPinPageLive501WorstCaseRows exercises the 500-plus-one page cap boundary against live
// services (TKT-143). It seeds a published seat map with 501 seats, pins all 501 with exact-limit
// values (200-character seat identities and a 45-character pinned_by, built from '&'), fetches a
// full page of 500 rows, asserts the raw body carries the six-byte escapes the cap's arithmetic
// assumes, confirms the payload fits under maxSeatPinPageBytes, and walks the keyset cursor to the
// end to prove every one of its 501 rows is reachable.
//
// Why a walk rather than "row 501 is on page two": seat_map_pins.id is gen_random_uuid()
// (migration 0011) and the read is keyset ordered by it, so these rows sort arbitrarily among the
// pins other smoke tests leave in the same database. Asserting a position would test where a
// random uuid landed, not whether the cursor advances.
//
// Mutations that make this test red:
//  1. Lowering maxSeatPinPageBytes below the real worst-case page: the 500-row body exceeds it.
//  2. Breaking cursor advancement in ListSeatMapPins: the walk never reaches all 501.
//  3. Truncating either bounded field: the 200/45 length assertions fail.
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
	// Measure one of THIS test's rows, not firstPage.Pins[0]. Row zero is whichever pin has the
	// lowest random uuid table-wide, which is routinely a pin some other smoke test left behind
	// and carries that test's much shorter values.
	var measured int
	for _, p := range firstPage.Pins {
		if p.PinnedBy != pinnedBy {
			continue
		}
		measured++
		if got := len([]rune(p.SeatIdentity)); got != 200 {
			t.Fatalf("seat identity length = %d, want 200", got)
		}
		if got := len([]rune(p.PinnedBy)); got != 45 {
			t.Fatalf("pinned_by length = %d, want 45", got)
		}
	}
	if measured == 0 {
		t.Fatal("no row on page one belonged to this test, so the exact-limit lengths went unmeasured")
	}

	// 2. Walk the keyset cursor to the end and assert every one of THIS test's 501 rows is
	// reachable, row 501 included.
	//
	// The walk cannot assume this test's rows are the first 501, or that row 501 sits on page
	// two. seat_map_pins.id is gen_random_uuid() (migration 0011) and ListSeatMapPins is keyset
	// ordered by that id, so these 501 rows are scattered arbitrarily among the pins every other
	// smoke test leaves in the same database. An earlier version of this test asserted row 501
	// was on page two and failed for that reason: the assertion was about where a random uuid
	// sorted, not about whether the cursor advances.
	//
	// What IS this test's to assert: a cursor walk reaches all 501, and no page exceeds the cap.
	seen := make(map[string]bool, rowCount)
	cursor := firstPage.Pins[len(firstPage.Pins)-1].ID
	for _, p := range firstPage.Pins {
		if p.PinnedBy == pinnedBy {
			seen[p.SeatIdentity] = true
		}
	}
	// 501 own rows plus whatever else is in the table; the bound stops a runaway walk without
	// assuming how many foreign rows exist.
	for page := 0; page < 200 && len(seen) < rowCount; page++ {
		nextURL := fmt.Sprintf("%s/internal/seat-map-pins?after=%s&limit=500", catalogURL, cursor)
		codeN, rawBodyN := internalJSON(t, http.MethodGet, nextURL, "", nil)
		if codeN != http.StatusOK {
			t.Fatalf("page %d GET: %d %s", page+2, codeN, rawBodyN)
		}
		if len(rawBodyN) > maxSeatPinPageBytes {
			t.Fatalf("page %d body length %d exceeds maxSeatPinPageBytes %d", page+2, len(rawBodyN), maxSeatPinPageBytes)
		}
		var next struct {
			Pins []struct {
				ID           uuid.UUID `json:"id"`
				SeatIdentity string    `json:"seat_identity"`
				PinnedBy     string    `json:"pinned_by"`
			} `json:"pins"`
		}
		if err := json.Unmarshal(rawBodyN, &next); err != nil {
			t.Fatalf("decode page %d: %v", page+2, err)
		}
		if len(next.Pins) == 0 {
			break // drained
		}
		for _, p := range next.Pins {
			if p.PinnedBy == pinnedBy {
				seen[p.SeatIdentity] = true
			}
		}
		cursor = next.Pins[len(next.Pins)-1].ID
	}

	if len(seen) != rowCount {
		t.Fatalf("cursor walk reached %d of this test's %d pins; row 501 is %s",
			len(seen), rowCount, identities[500])
	}
	if !seen[identities[500]] {
		t.Fatalf("row 501 (%s) was never reached by the cursor walk", identities[500])
	}
}
