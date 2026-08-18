//go:build smoke

package smoke_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// postBestAvailable POSTs a best-available hold through the gateway with an idempotency key
// and contract-validates the response — which is also what records this operation's smoke
// coverage (TKT-47's gate: a documented 2xx operation with no driving smoke test fails the
// suite).
func postBestAvailable(t *testing.T, key string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, gatewayURL+"/api/inventory/holds/seats/best-available", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST best-available hold: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

// TestBestAvailableSelectsAContiguousRunEndToEnd is TKT-81 across the whole chain, and the
// only tier that can prove the part unit tests structurally cannot: that the ordering
// metadata SURVIVES the trip from catalog's authoring API, through the schema-5 publication,
// through inventory's geometry fetch and derivation, into the projection the selection query
// reads.
//
// Every layer below this one supplies its own adjacency fixture, so every layer below can
// pass while the wire between catalog and inventory drops row_key or position on the floor —
// which is exactly what the code did before this ticket, deliberately, since ADR-041 had no
// use for either. A store test cannot see that, and neither can a handler test.
//
// The seat map is authored as TWO rows of five, and the assertions turn on the boundary
// between them: it is the one property that cannot be checked without real geometry, because
// inventory's notion of a "row" is whatever catalog said it was.
func TestBestAvailableSelectsAContiguousRunEndToEnd(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)

	venue := created(t, catalog+"/venues", map[string]any{
		"name": "Best Available Hall " + suffix, "ga_capacity": 400,
	})
	event := created(t, catalog+"/events", map[string]any{
		"name": map[string]string{"fr": "Concert " + suffix, "en": "Concert " + suffix},
	})

	// Rule ON, because best-available reads the projection ADR-041 provisions and this
	// ticket deliberately did not decouple the two (ADR-061 records the deferral).
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"name": "Stalls " + suffix, "orphan_prevention_enabled": true,
	})
	section := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"name": "Stalls", "position": 1,
	})
	rows := make([]any, 0, 2)
	for r := 1; r <= 2; r++ {
		row := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/rows", map[string]any{
			"section_id": section["id"], "label": fmt.Sprint("ABCDEFGH"[r-1 : r]), "position": r,
		})
		rows = append(rows, row["id"])
		for i := 1; i <= 5; i++ {
			created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/seats", map[string]any{
				"row_id": row["id"], "label": fmt.Sprint(i), "position": i,
			})
		}
	}

	if code, body := postJSON(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}
	seated := created(t, catalog+"/performances", map[string]any{
		"event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-12-01T20:00:00Z", "timezone": "Europe/Paris",
		"seat_map_id": seatMap["id"],
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, seated["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish seated performance: %d %s", code, body)
	}

	// Wait for inventory to provision the pool and its projection from that publication.
	occupancy := fmt.Sprintf("%s/api/inventory/slots/%v/seat-occupancy?organizer_id=%s", gatewayURL, seated["id"], organizerID)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, gerr := http.Get(occupancy)
		if gerr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("inventory never provisioned the seated pool for the schema-5 publication")
		}
		time.Sleep(500 * time.Millisecond)
	}

	ticketType := "11111111-1111-1111-1111-111111111111"
	hold := func(key string, count int) (int, []byte) {
		return postBestAvailable(t, key, map[string]any{
			"organizer_id": organizerID, "slot_id": seated["id"], "seat_count": count,
			"ticket_type_id": ticketType, "unit_amount": 5000, "currency": "EUR",
		})
	}
	var out struct {
		HoldID string   `json:"hold_id"`
		Seats  []string `json:"seats"`
	}

	// A party of three lands in row A, in position order — which is the assertion that
	// fails if position never reached inventory.
	code, body := hold("ba-"+suffix, 3)
	if code != http.StatusCreated {
		t.Fatalf("best-available 3 = %d %s want 201", code, body)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response: %v (%s)", err, body)
	}
	want := []string{"Stalls/A/1", "Stalls/A/2", "Stalls/A/3"}
	if fmt.Sprint(out.Seats) != fmt.Sprint(want) {
		t.Fatalf("seats = %v want %v — a contiguous run in projected order", out.Seats, want)
	}

	// The replay returns the SAME seats rather than selecting a second run. On this path
	// the request cannot express the seats, so nothing but the persisted row can supply
	// them, and a re-selection here would silently double-sell.
	first := out
	rcode, rbody := hold("ba-"+suffix, 3)
	if rcode != http.StatusOK {
		t.Fatalf("replay = %d %s want 200", rcode, rbody)
	}
	if err := json.Unmarshal(rbody, &out); err != nil {
		t.Fatalf("replay response: %v (%s)", err, rbody)
	}
	if out.HoldID != first.HoldID || fmt.Sprint(out.Seats) != fmt.Sprint(first.Seats) {
		t.Fatalf("replay returned %s/%v, original was %s/%v", out.HoldID, out.Seats, first.HoldID, first.Seats)
	}

	// Row A now has seats 4 and 5 free; row B is untouched. A party of FOUR therefore has
	// no run in row A and must land wholly in row B — the property that proves the row
	// boundary survived the wire. A projection that lost row_key would happily answer
	// A/4, A/5, B/1, B/2 and call them adjacent.
	ccode, cbody := hold("ba-cross-"+suffix, 4)
	if ccode != http.StatusCreated {
		t.Fatalf("best-available 4 = %d %s want 201", ccode, cbody)
	}
	if err := json.Unmarshal(cbody, &out); err != nil {
		t.Fatalf("response: %v (%s)", err, cbody)
	}
	for _, s := range out.Seats {
		if len(s) < 8 || s[6] != 'B' {
			t.Fatalf("seats = %v — a four-seat run must be wholly within row B; row A has only two free seats left, so any answer containing an A seat means the row boundary was lost between catalog and inventory", out.Seats)
		}
	}

	// And the pool that can no longer seat four says so with the RETRYABLE code, not the
	// one that means "this slot can never do this" (ADR-061). Everything is now taken
	// except A/4 and A/5.
	ucode, ubody := hold("ba-none-"+suffix, 4)
	if ucode != http.StatusConflict {
		t.Fatalf("exhausted best-available = %d %s want 409", ucode, ubody)
	}
	var refusal struct {
		Code    string `json:"code"`
		Details struct {
			Code string `json:"code"`
		} `json:"details"`
	}
	if err := json.Unmarshal(ubody, &refusal); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, ubody)
	}
	got := refusal.Code
	if got == "" {
		got = refusal.Details.Code
	}
	if got != "best_available_unavailable" {
		t.Fatalf("refusal code = %q want best_available_unavailable — a pool that cannot seat this party is not a pool that lacks the capability (%s)", got, ubody)
	}
}
