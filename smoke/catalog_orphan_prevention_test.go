//go:build smoke

package smoke_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestOrphanPreventionPublishesSchema5AndEnforcesTheRule is the first end-to-end run of
// the whole ADR-041 chain: catalog forks the publication to schema 5 (TKT-183), inventory
// fetches the bound version's geometry and commits an adjacency projection (TKT-181), and
// the rule refuses a stranding selection inside the claim transaction (TKT-182).
//
// The refusal is the strongest available proof that the projection actually landed: a pool
// with the flag but no adjacency, or with adjacency but no flag, cannot produce it.
func TestOrphanPreventionPublishesSchema5AndEnforcesTheRule(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffixBytes := make([]byte, 4)
	_, _ = rand.Read(suffixBytes)
	suffix := hex.EncodeToString(suffixBytes)

	venue := created(t, catalog+"/venues", map[string]any{
		"name": "Orphan Hall " + suffix, "ga_capacity": 400,
	})
	event := created(t, catalog+"/events", map[string]any{
		"name":         map[string]string{"fr": "Concert " + suffix, "en": "Concert " + suffix},
	})

	// A seat map with the rule ON, and one row of FOUR seats. Three is the minimum in
	// which a selection can strand a seat, but it is too small to also demonstrate the
	// negative: in a row of three, EVERY pair strands the third, so a passing "this one
	// is allowed" case is impossible there.
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"name": "Stalls " + suffix,
		"orphan_prevention_enabled": true,
	})
	section := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"name": "Stalls", "position": 1,
	})
	row := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/rows", map[string]any{
		"section_id": section["id"], "label": "A", "position": 1,
	})
	for i := 1; i <= 4; i++ {
		created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/seats", map[string]any{
			"row_id": row["id"],
			"label": fmt.Sprint(i), "position": i,
		})
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx := t.Context()
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("PLATFORM stream: %v", err)
	}
	pubCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "smoke-orphan-published-" + suffix,
		FilterSubject: "platform.catalog.performance.published",
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("published consumer: %v", err)
	}

	if code, body := postJSON(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}

	seated := created(t, catalog+"/performances", map[string]any{
		"event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-11-01T20:00:00Z", "timezone": "Europe/Paris",
		"seat_map_id": seatMap["id"],
	})
	ticketType := created(t, catalog+"/ticket-types", map[string]any{
		"performance_id": seated["id"],
		"name":  map[string]string{"fr": "Place", "en": "Seat"},
		"price": map[string]any{"amount": 5000, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, seated["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish seated: %d %s", code, body)
	}

	env := awaitPublishedEnvelope(t, pubCons, seated["id"])
	if env.Schema != 5 {
		t.Fatalf("schema = %d, want 5 — a rule-enabled bound version must fork", env.Schema)
	}
	if env.Data.OrphanPrevention == nil || !*env.Data.OrphanPrevention {
		t.Fatalf("orphan_prevention_enabled = %v — schema 5 without the flag would provision a rule-enabled pool that says nothing is enabled", env.Data.OrphanPrevention)
	}
	if env.Data.SeatMapID != fmt.Sprint(seatMap["id"]) {
		t.Fatalf("seat_map_id = %s, want the exact bound version %v", env.Data.SeatMapID, seatMap["id"])
	}

	// Wait for inventory to provision the pool from that event.
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

	// Seats 1 + 3 strand 2 (and 4, which is then a row end with an occupied neighbour).
	// The refusal proves flag AND projection AND rule — no two of the three produce it.
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "orphan-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": ticketType["id"],
		"seat_identities": []string{"Stalls/A/1", "Stalls/A/3"},
	})
	if code != http.StatusConflict {
		t.Fatalf("stranding selection returned %d %s, want 409 — the rule is not enforced end to end", code, body)
	}
	var refusal struct {
		Code    string   `json:"code"`
		Seats   []string `json:"seats"`
		Details struct {
			Seats []string `json:"seats"`
		} `json:"details"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, body)
	}
	if refusal.Code != "orphaned_seats" {
		t.Fatalf("refusal code = %q, want orphaned_seats (%s)", refusal.Code, body)
	}

	// The un-stranding selection on the same pool still succeeds: the rule refuses a
	// shape, not seated claims.
	// 1 + 2 leaves 3 and 4 adjacent to each other, so nothing is stranded.
	if code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "orphan-ok-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": ticketType["id"],
		"seat_identities": []string{"Stalls/A/1", "Stalls/A/2"},
	}); code != http.StatusCreated {
		t.Fatalf("adjacent selection returned %d %s, want 201", code, body)
	}
}
