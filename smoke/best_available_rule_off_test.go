//go:build smoke

package smoke_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func createRuleOffSeatedPerformance(t *testing.T, rows, seatsPerRow int) (string, string, string) {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	suffix := hex.EncodeToString(random)
	venue := created(t, catalog+"/venues", map[string]any{"name": "Rule Off Hall " + suffix, "ga_capacity": 400})
	event := created(t, catalog+"/events", map[string]any{
		"name": map[string]string{"en": "Show " + suffix, "fr": "Show " + suffix},
	})
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"name": "Stalls " + suffix, "orphan_prevention_enabled": false,
	})
	section := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"name": "Stalls", "position": 1,
	})
	for rowNumber := 1; rowNumber <= rows; rowNumber++ {
		label := string(rune('A' + rowNumber - 1))
		row := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/rows", map[string]any{
			"section_id": section["id"], "label": label, "position": rowNumber,
		})
		for seat := 1; seat <= seatsPerRow; seat++ {
			created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/seats", map[string]any{
				"row_id": row["id"], "label": fmt.Sprint(seat), "position": seat,
			})
		}
	}
	if code, body := postJSON(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}
	performance := created(t, catalog+"/performances", map[string]any{
		"event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2027-02-01T20:00:00Z", "timezone": "America/Toronto", "seat_map_id": seatMap["id"],
	})
	ticket := created(t, catalog+"/ticket-types", map[string]any{
		"performance_id": performance["id"], "name": map[string]string{"en": "Seat", "fr": "Place"},
		"price": map[string]any{"amount": 5000, "currency": "EUR"},
	})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, performance["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish seated performance: %d %s", code, body)
	}
	return fmt.Sprint(performance["id"]), fmt.Sprint(seatMap["id"]), fmt.Sprint(ticket["id"])
}

func inventoryConnection(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatalf("connect inventory database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func awaitOrderingProjection(t *testing.T, slotID string, conn *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	retry(t, 30*time.Second, func() error {
		var count int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM seat_claim_adjacency WHERE pool_id=$1 AND row_key IS NOT NULL`, slotID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("ordering projection is not present")
		}
		return nil
	})
}

func assertExactOrderingProjection(t *testing.T, slotID string, conn *pgx.Conn, want []string) {
	t.Helper()
	rows, err := conn.Query(t.Context(), `SELECT seat_identity, row_key IS NOT NULL, position IS NOT NULL, row_rank IS NOT NULL
		FROM seat_claim_adjacency WHERE pool_id=$1 ORDER BY seat_identity`, slotID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var identity string
		var hasRowKey, hasPosition, hasRank bool
		if err := rows.Scan(&identity, &hasRowKey, &hasPosition, &hasRank); err != nil {
			t.Fatal(err)
		}
		if !hasRowKey || !hasPosition || !hasRank {
			t.Fatalf("seat %q ordering fields present = %v/%v/%v, want all present", identity, hasRowKey, hasPosition, hasRank)
		}
		got = append(got, identity)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ordering projection identities = %v, want exact set %v", got, want)
	}
}

func TestRuleOffPublicationSupportsBestAvailableAndKeepsOrphanRuleOff(t *testing.T) {
	slotID, _, ticketID := createRuleOffSeatedPerformance(t, 2, 4)
	conn := inventoryConnection(t)
	awaitOrderingProjection(t, slotID, conn)
	assertExactOrderingProjection(t, slotID, conn, []string{
		"Stalls/A/1", "Stalls/A/2", "Stalls/A/3", "Stalls/A/4",
		"Stalls/B/1", "Stalls/B/2", "Stalls/B/3", "Stalls/B/4",
	})

	var enabled bool
	if err := conn.QueryRow(t.Context(), `SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slotID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("rule-off publication enabled orphan prevention")
	}

	base := map[string]any{
		"organizer_id": organizerID, "slot_id": slotID,
		"ticket_type_id": ticketID, "unit_amount": 5000, "currency": "EUR",
	}
	best := make(map[string]any, len(base)+1)
	for key, value := range base {
		best[key] = value
	}
	best["seat_count"] = 2
	code, body := postBestAvailable(t, "rule-off-ba-"+slotID, best)
	if code != http.StatusCreated {
		t.Fatalf("best-available returned %d %s, want 201", code, body)
	}
	var result struct {
		Seats []string `json:"seats"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode best-available response: %v (%s)", err, body)
	}
	if fmt.Sprint(result.Seats) != fmt.Sprint([]string{"Stalls/A/1", "Stalls/A/2"}) {
		t.Fatalf("best-available seats = %v, want the first ordered run", result.Seats)
	}

	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "rule-off-strand-"+slotID, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": ticketID,
		"seat_identities": []string{"Stalls/B/1", "Stalls/B/3"},
	})
	if code != http.StatusCreated {
		t.Fatalf("rule-off stranding selection returned %d %s, want 201", code, body)
	}
	if err := conn.QueryRow(t.Context(), `SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slotID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("named-seat claim changed the rule-off pool flag")
	}
}

func TestRuleOffCorrectionAddsOrderingToExistingPool(t *testing.T) {
	slotID, _, ticketID := createRuleOffSeatedPerformance(t, 1, 4)
	conn := inventoryConnection(t)
	awaitOrderingProjection(t, slotID, conn)
	if _, err := conn.Exec(t.Context(), `DELETE FROM seat_claim_adjacency WHERE pool_id=$1`, slotID); err != nil {
		t.Fatalf("delete legacy pool adjacency: %v", err)
	}
	if code, body := postBestAvailable(t, "before-correction-"+slotID, map[string]any{
		"organizer_id": organizerID, "slot_id": slotID, "seat_count": 2,
		"ticket_type_id": ticketID, "unit_amount": 5000, "currency": "EUR",
	}); code != http.StatusConflict || !strings.Contains(string(body), "best_available_unsupported") {
		t.Fatalf("legacy pool best-available = %d %s, want best_available_unsupported", code, body)
	}

	runBestAvailableOrderingCorrection(t)
	awaitOrderingProjection(t, slotID, conn)
	assertExactOrderingProjection(t, slotID, conn, []string{
		"Stalls/A/1", "Stalls/A/2", "Stalls/A/3", "Stalls/A/4",
	})
	var enabled bool
	if err := conn.QueryRow(t.Context(), `SELECT orphan_prevention_enabled FROM inventory_pools WHERE slot_id=$1`, slotID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("correction enabled orphan prevention on a rule-off pool")
	}
	code, body := postBestAvailable(t, "after-correction-"+slotID, map[string]any{
		"organizer_id": organizerID, "slot_id": slotID, "seat_count": 2,
		"ticket_type_id": ticketID, "unit_amount": 5000, "currency": "EUR",
	})
	if code != http.StatusCreated {
		t.Fatalf("best-available after correction = %d %s, want 201", code, body)
	}
	var result struct {
		Seats []string `json:"seats"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode corrected best-available response: %v (%s)", err, body)
	}
	if fmt.Sprint(result.Seats) != fmt.Sprint([]string{"Stalls/A/1", "Stalls/A/2"}) {
		t.Fatalf("best-available after correction chose %v, want the first repaired run", result.Seats)
	}
}

func runBestAvailableOrderingCorrection(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--network", project+"_default",
		"-e", "DATABASE_URL="+containerDSN("catalog", "catalog"),
		"-e", "NATS_URL="+containerNATSURL("catalog"),
		project+"-catalog", "reemit-best-available-ordering").CombinedOutput()
	if err != nil {
		t.Fatalf("reemit-best-available-ordering failed: %v: %s", err, out)
	}
}
