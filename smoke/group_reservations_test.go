//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Group/agency reservations (TKT-79 / ADR-027): staff surface off the gateway, like the
// operational holds it is built on.

type grpStaffView struct {
	BuyerHeld       int32 `json:"buyer_held"`
	OperationalHeld int32 `json:"operational_held"`
	ReservationHeld int32 `json:"reservation_held"`
	Confirmed       int32 `json:"confirmed"`
	Available       int32 `json:"available"`
	PublicAvailable int32 `json:"public_available"`
}

func grpStaff(t *testing.T, slot string) grpStaffView {
	t.Helper()
	code, body := internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/slots/%s/availability?organizer_id=%s", inventoryURL, slot, organizerID), "", nil)
	if code != 200 {
		t.Fatalf("staff availability %d %s", code, body)
	}
	var a grpStaffView
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestGroupReservationDrawDownAndCheckout proves the story end to end: a 200-seat
// reservation drawn down twice (10, then 50) through the existing commerce checkout, the
// remainder never publicly claimable, then lazily returned to sale at expiry.
func TestGroupReservationDrawDownAndCheckout(t *testing.T) {
	slot, tt := publishedSlot(t, "Agency Block Hall", 200)

	code, body := internalJSON(t, http.MethodPost, inventoryURL+"/internal/group-reservations", "grp-place-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 200, "counterparty": "Acme Travel", "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "actor": "staff:smoke", "reason": "contract allotment"})
	if code != 201 {
		t.Fatalf("place reservation: %d %s", code, body)
	}
	var res struct {
		ID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	drawURL := fmt.Sprintf("%s/internal/group-reservations/%s/draw-down", commerceURL, res.ID)

	// Two draws, two checkouts, weeks apart in real life — distinct keys.
	for i, draw := range []struct {
		qty        int
		remaining  int32
		wantAmount int64
	}{{10, 190, 25000}, {50, 140, 125000}} {
		key := fmt.Sprintf("grp-draw-%s-%d", slot, i)
		code, body = internalJSON(t, http.MethodPost, drawURL, key,
			map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": draw.qty, "actor": "staff:smoke", "reason": "agency batch"})
		if code != 201 {
			t.Fatalf("draw %d: %d %s", draw.qty, code, body)
		}
		var conv struct {
			ReservationID   string `json:"reservation_id"`
			Amount          int64  `json:"amount"`
			SourceRemaining int32  `json:"source_remaining"`
		}
		if err := json.Unmarshal(body, &conv); err != nil {
			t.Fatal(err)
		}
		if conv.Amount != draw.wantAmount || conv.SourceRemaining != draw.remaining {
			t.Fatalf("draw %d result %+v, want amount %d remaining %d", draw.qty, conv, draw.wantAmount, draw.remaining)
		}
		code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", fmt.Sprintf("grp-order-%s-%d", slot, i),
			map[string]any{"reservation_id": conv.ReservationID, "name": "Acme Group Lead", "email": "groups@acme.test", "payment_token": "fake-ok"})
		if code != 200 || !bytes.Contains(body, []byte(`"completed"`)) {
			t.Fatalf("checkout of draw %d: %d %s", draw.qty, code, body)
		}
		// Replay after checkout returns the original outcome (child confirmed), never a
		// second carve.
		if code, body = internalJSON(t, http.MethodPost, drawURL, key,
			map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": draw.qty, "actor": "staff:smoke", "reason": "agency batch"}); code != 200 {
			t.Fatalf("draw replay after checkout: %d %s", code, body)
		}
	}
	a := grpStaff(t, slot)
	if a.Confirmed != 60 || a.ReservationHeld != 140 || a.BuyerHeld != 0 || a.Available != 0 || a.PublicAvailable != 0 {
		t.Fatalf("accounting after draws: %+v, want confirmed=60 reservation_held=140 available=0", a)
	}
	// The audit trail records the reservation and each draw.
	code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/group-reservations/%s/history?organizer_id=%s", inventoryURL, res.ID, organizerID), "", nil)
	if code != 200 {
		t.Fatalf("history: %d %s", code, body)
	}
	var entries []struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Action != "reserve" || entries[1].Action != "draw_down" || entries[2].Action != "draw_down" {
		t.Fatalf("history = %+v, want reserve then two draw_downs", entries)
	}

	// Lazy give-back at expiry: force the deadline into the past (equivalent to waiting),
	// then the unconverted 140 are publicly sellable — the confirmed 60 are not.
	ctx := t.Context()
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	if tag, err := db.Exec(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1::uuid`, res.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("force expiry: %v (%d rows)", err, tag.RowsAffected())
	}
	if code, body = postWithKey(t, gatewayURL+"/api/inventory/holds", "grp-giveback-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 50}); code != 201 {
		t.Fatalf("public hold after reservation expiry: %d %s", code, body)
	}
	a = grpStaff(t, slot)
	if a.ReservationHeld != 0 || a.BuyerHeld != 50 || a.Confirmed != 60 || a.PublicAvailable != 90 {
		t.Fatalf("accounting after give-back: %+v, want reservation=0 buyer=50 confirmed=60 public=90", a)
	}

	// The staff surface must not exist at the public edge.
	for _, edge := range []struct{ method, url string }{
		{http.MethodPost, gatewayURL + "/api/inventory/internal/group-reservations"},
		{http.MethodPost, fmt.Sprintf("%s/api/inventory/internal/group-reservations/%s/draw-down", gatewayURL, res.ID)},
		{http.MethodGet, fmt.Sprintf("%s/api/inventory/internal/group-reservations/%s/history?organizer_id=%s", gatewayURL, res.ID, organizerID)},
		{http.MethodPost, fmt.Sprintf("%s/api/commerce/internal/group-reservations/%s/draw-down", gatewayURL, res.ID)},
	} {
		if code, _ := internalJSON(t, edge.method, edge.url, "k", map[string]any{}); code != 404 {
			t.Fatalf("gateway must 404 %s %s, got %d", edge.method, edge.url, code)
		}
	}
}

// TestGroupReservationDrawDownRacesPublicHolds is the AC4 contention proof: with the pool
// fully reserved, a draw-down and a burst of public holds all queue on the pool lock; the
// draw-down (queued first — FIFO) carves 3, and no public hold ever lands. Deterministic:
// a manual transaction holds the pool lock, and the pg_stat_activity handshake pins the
// draw-down's own marked lock statement before the burst starts
// (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md).
func TestGroupReservationDrawDownRacesPublicHolds(t *testing.T) {
	slot, tt := publishedSlot(t, "Agency Race Hall", 10)

	code, body := internalJSON(t, http.MethodPost, inventoryURL+"/internal/group-reservations", "grp-race-place-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 10, "counterparty": "Acme Travel", "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "actor": "staff:smoke", "reason": "race test"})
	if code != 201 {
		t.Fatalf("place reservation: %d %s", code, body)
	}
	var res struct {
		ID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	blocker, err := pgx.Connect(ctx, fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(ctx) }()
	monitor, err := pgx.Connect(ctx, fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = monitor.Close(ctx) }()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1::uuid FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	waitQueued := func(like string, want int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			var n int
			if err := monitor.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE wait_event_type='Lock' AND state='active'
					  AND query LIKE $1 AND pid <> pg_backend_pid()`, like).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d/%d waiters matching %q queued on the pool lock", n, want, like)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	drawDone := make(chan int, 1)
	go func() {
		code, _ := internalJSONAsync(t, http.MethodPost, fmt.Sprintf("%s/internal/group-reservations/%s/draw-down", commerceURL, res.ID), "grp-race-draw-"+slot,
			map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 3, "actor": "staff:smoke", "reason": "race draw"})
		drawDone <- code
	}()
	// The draw-down's own marked statement — if the implementation ever splits the swap
	// or drops the pool-first lock, no waiter matches and this fails the setup.
	waitQueued("%grp-draw pool lock%", 1)

	var publicGrants atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postWithKeyAsync(t, gatewayURL+"/api/inventory/holds", fmt.Sprintf("grp-race-%s-%d", slot, i),
				map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
			if code == 201 {
				publicGrants.Add(1)
			} else if code != 409 {
				t.Errorf("public hold %d unexpected status %d", i, code)
			}
		}(i)
	}
	// All public holds observed queued behind the draw-down before anything is released.
	waitQueued("%closure_status FROM inventory_pools%FOR UPDATE%", 12)

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if code := <-drawDone; code != 201 {
		t.Fatalf("draw-down: %d", code)
	}
	if got := publicGrants.Load(); got != 0 {
		t.Fatalf("public holds granted during draw-down: %d, want 0", got)
	}
	a := grpStaff(t, slot)
	if a.ReservationHeld != 7 || a.BuyerHeld != 3 || a.Confirmed != 0 || a.Available != 0 {
		t.Fatalf("accounting after race: %+v, want reservation=7 buyer=3 available=0", a)
	}
}
