//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Staff-triggered redelivery, end to end (TKT-203, ADR-068).
//
// The fixture is a REAL purchase rather than seeded rows, for the reason the order
// console's own fixture records: issuance is the only path that produces a ticket, its
// `delivered` lifecycle event and its delivery_attempts row together, and a resend is
// defined entirely in terms of what that path already left behind. Hand-inserted
// lifecycle rows would also be read as tampering by the verifier this gate runs.

// redeliver posts one resend with a given credential, DIRECT to access — the route is
// under /internal/, which the gateway edge-denies by construction (ADR-002).
func redeliver(t *testing.T, orderID, key, token string) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/internal/orders/%s/redeliveries", accessURL, orderID)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"organizer_id":"`+organizerID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	if token != "" {
		req.Header.Set("X-Access-Staff-Write-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// countAccessRows reads one count from the access database. The assertions that matter
// here are about what the resend LEFT BEHIND, not about the response alone: a handler
// that answered 200 and wrote nothing would satisfy a status-only check.
func countAccessRows(t *testing.T, ctx context.Context, query string, args ...any) int {
	t.Helper()
	conn := accessConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func staffRedeliveryToken(t *testing.T) string {
	t.Helper()
	token := os.Getenv("SMOKE_ACCESS_STAFF_WRITE_TOKEN")
	if token == "" {
		t.Fatal("SMOKE_ACCESS_STAFF_WRITE_TOKEN is unset: scripts/stack-env.sh must generate it")
	}
	return token
}

// TKT-203 COS-1/3/4/6, against the running stack.
func TestStaffRedeliveryResendsEveryTicketAndRecordsEachSend(t *testing.T) {
	ctx := context.Background()
	staff := staffRedeliveryToken(t)
	orderID, _, ticketIDs, _ := consoleFixture(t, "redelivery")
	if len(ticketIDs) == 0 {
		t.Fatal("fixture produced no tickets")
	}

	// The tickets were delivered by issuance, so each carries a `delivered` event.
	// That is the state a resend exists to serve and the state PendingDeliveries
	// excludes — asserted so the fixture's relevance is a fact of the run rather than
	// an assumption about it.
	delivered := countAccessRows(t, ctx,
		`SELECT count(*) FROM lifecycle_events WHERE event_type='delivered' AND ticket_id = ANY($1)`, ticketIDs)
	if delivered != len(ticketIDs) {
		t.Fatalf("fixture has %d delivered events for %d tickets, so this run would not be "+
			"exercising the already-delivered case the ticket is about", delivered, len(ticketIDs))
	}

	code, body := redeliver(t, orderID, "smoke-redelivery-"+orderID, staff)
	if code != 200 {
		t.Fatalf("redelivery %d %s", code, body)
	}
	var out struct {
		OrderID     string `json:"order_id"`
		TicketCount int    `json:"ticket_count"`
		Replay      bool   `json:"replay"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.OrderID != orderID || out.Replay {
		t.Fatalf("resend answered %+v, want a fresh send for %s", out, orderID)
	}
	if out.TicketCount != len(ticketIDs) {
		t.Fatalf("resend covered %d tickets, want all %d of the order — an already-delivered "+
			"ticket was skipped", out.TicketCount, len(ticketIDs))
	}

	// COS-6, asserted at the boundary the value would cross on its way out, on the raw
	// bytes: a field added under another name still passes a struct decode.
	for _, forbidden := range []string{"@", "en/tickets", "email", "guest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the resend response contains %q — it must carry a count, never the "+
				"recipient or a ticket link: %s", forbidden, body)
		}
	}

	// COS-3: one NEW attempt row per ticket, none reusing the original message id.
	attempts := countAccessRows(t, ctx,
		`SELECT count(*) FROM redelivery_attempts WHERE ticket_id = ANY($1)`, ticketIDs)
	if attempts != len(ticketIDs) {
		t.Fatalf("%d resend attempt rows, want one per ticket (%d)", attempts, len(ticketIDs))
	}
	collisions := countAccessRows(t, ctx, `SELECT count(*) FROM redelivery_attempts r
		JOIN delivery_attempts d ON d.ticket_id = r.ticket_id AND d.message_id = r.message_id
		WHERE r.ticket_id = ANY($1)`, ticketIDs)
	if collisions != 0 {
		t.Fatalf("%d resends reused the ORIGINAL delivery's message id: a transport that "+
			"deduplicates on message id drops those as replays of the first send", collisions)
	}

	// COS-4: the trail records the resend per ticket, and says the same as before about
	// the first delivery.
	redelivered := countAccessRows(t, ctx,
		`SELECT count(*) FROM lifecycle_events WHERE event_type='redelivered' AND ticket_id = ANY($1)`, ticketIDs)
	if redelivered != len(ticketIDs) {
		t.Fatalf("%d redelivered events after one resend, want one per ticket (%d)",
			redelivered, len(ticketIDs))
	}
	if still := countAccessRows(t, ctx,
		`SELECT count(*) FROM lifecycle_events WHERE event_type='delivered' AND ticket_id = ANY($1)`,
		ticketIDs); still != delivered {
		t.Fatalf("the original delivered events changed from %d to %d: a resend adds to the "+
			"trail, it does not rewrite what the trail says about the first delivery",
			delivered, still)
	}
}

// COS-7 on the wire: one key, one send, however many times it is presented.
func TestStaffRedeliveryReplaysOneKeyRatherThanSendingTwice(t *testing.T) {
	ctx := context.Background()
	staff := staffRedeliveryToken(t)
	orderID, _, ticketIDs, _ := consoleFixture(t, "redelivery-replay")
	key := "smoke-replay-" + orderID

	if code, body := redeliver(t, orderID, key, staff); code != 200 {
		t.Fatalf("first resend %d %s", code, body)
	}
	code, body := redeliver(t, orderID, key, staff)
	if code != 200 {
		t.Fatalf("second resend %d %s", code, body)
	}
	var out struct {
		Replay bool `json:"replay"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Replay {
		t.Fatal("the second request under one key was not reported as a replay: a caller " +
			"cannot then tell a double-click from a second deliberate send")
	}
	if n := countAccessRows(t, ctx,
		`SELECT count(*) FROM redelivery_attempts WHERE ticket_id = ANY($1)`, ticketIDs); n != len(ticketIDs) {
		t.Fatalf("%d attempt rows after two submissions of one key, want one per ticket (%d)",
			n, len(ticketIDs))
	}
	if n := countAccessRows(t, ctx,
		`SELECT count(*) FROM lifecycle_events WHERE event_type='redelivered' AND ticket_id = ANY($1)`,
		ticketIDs); n != len(ticketIDs) {
		t.Fatalf("%d redelivered events after a double submit, want one per ticket (%d)",
			n, len(ticketIDs))
	}
}

// The credential is the boundary, and each wrong one refuses on its own. 404 throughout:
// access's answer on its internal surface, so a caller without the credential cannot even
// learn the route exists (ADR-043).
//
// The three sibling staff credentials are the cases that matter most: ADR-068 chose a NEW
// credential precisely so that a compromise of one service's staff token does not become
// the power to re-emit ticket capabilities. That is executed here rather than argued.
func TestStaffRedeliveryRefusesEveryOtherCredential(t *testing.T) {
	ctx := context.Background()
	orderID, _, ticketIDs, _ := consoleFixture(t, "redelivery-auth")
	for _, c := range []struct{ name, token string }{
		{"no credential", ""},
		{"a wrong value", "not-the-credential-0f3d1c9a8b7e6f5d4c3b"},
		{"the catalog staff credential", os.Getenv("SMOKE_CATALOG_STAFF_WRITE_TOKEN")},
		{"the commerce staff credential", os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN")},
		{"the inventory staff credential", os.Getenv("SMOKE_INVENTORY_STAFF_WRITE_TOKEN")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.token == "" && c.name != "no credential" {
				t.Fatalf("%s is not exported to this run, so this case would pass without "+
					"testing anything", c.name)
			}
			code, body := redeliver(t, orderID, "smoke-auth-"+c.name+"-"+orderID, c.token)
			if code != http.StatusNotFound {
				t.Fatalf("%s answered %d, want 404: another service's staff credential must "+
					"not open access's resend — that is the cross-service grant ADR-068 refuses "+
					"(%s)", c.name, code, body)
			}
		})
	}
	// And nothing was written by any of them. A refusal that still moved the trail
	// would be a refusal in name only.
	if n := countAccessRows(t, ctx,
		`SELECT count(*) FROM redelivery_attempts WHERE ticket_id = ANY($1)`, ticketIDs); n != 0 {
		t.Fatalf("the refused requests left %d attempt rows behind", n)
	}
}
