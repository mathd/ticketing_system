//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestInventoryContentionSafeHolds(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	inventory := gatewayURL + "/api/inventory"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Contention Hall", "ga_capacity": 10})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Test contention", "en": "Contention test"}})
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": "2026-10-01T20:00:00Z", "timezone": "UTC"})
	created(t, catalog+"/ticket-types", map[string]any{"organizer_id": organizerID, "performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 1000, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != 200 {
		t.Fatalf("publish %d %s", code, body)
	}
	slot := fmt.Sprint(perf["id"])
	retry(t, 20*time.Second, func() error {
		code, body, h := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s", inventory, slot, organizerID))
		if code != 200 {
			return fmt.Errorf("not provisioned: %d %s", code, body)
		}
		if h.Get("Cache-Control") != "public, max-age=5, s-maxage=5" {
			return fmt.Errorf("cache tier %q", h.Get("Cache-Control"))
		}
		return nil
	})

	knownKey := "known-" + slot
	code, body := postWithKey(t, inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("known hold %d %s", code, body)
	}
	var granted atomic.Int32
	granted.Store(1)
	var wg sync.WaitGroup
	for i := 0; i < 39; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postWithKeyAsync(t, inventory+"/holds", fmt.Sprintf("parallel-%s-%d", slot, i), map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
			if code == 201 {
				granted.Add(1)
			} else if code != 409 {
				t.Errorf("hold %d unexpected status %d", i, code)
			}
		}(i)
	}
	wg.Wait()
	if got := granted.Load(); got != 10 {
		t.Fatalf("granted %d, want exact capacity 10", got)
	}
	code, body = postWithKey(t, inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != 200 {
		t.Fatalf("idempotent replay %d %s", code, body)
	}
	code, _ = postWithKey(t, inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 2})
	if code != 409 {
		t.Fatalf("changed replay want 409 got %d", code)
	}
	code, body, h := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s", inventory, slot, organizerID))
	if code != 200 {
		t.Fatalf("availability %d %s", code, body)
	}
	_ = h
	var a struct{ Available, Held, Confirmed int32 }
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if a.Available != 0 || a.Held != 10 || a.Confirmed != 0 {
		t.Fatalf("bad accounting %+v", a)
	}
}

// TKT-78: the no-oversell proof extends to per-channel caps. With capacity 10 and a
// presale cap of 6, public claimable is exactly 4 whatever the interleaving — channel
// sales shrink the reservation one-for-one — so the grant counts are deterministic.
func TestChannelAllocationContention(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	inventory := gatewayURL + "/api/inventory"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Channel Hall", "ga_capacity": 10})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Test canaux", "en": "Channel test"}})
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": "2026-11-01T20:00:00Z", "timezone": "UTC"})
	created(t, catalog+"/ticket-types", map[string]any{"organizer_id": organizerID, "performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 1000, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != 200 {
		t.Fatalf("publish %d %s", code, body)
	}
	slot := fmt.Sprint(perf["id"])
	retry(t, 20*time.Second, func() error {
		code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s", inventory, slot, organizerID))
		if code != 200 {
			return fmt.Errorf("not provisioned: %d %s", code, body)
		}
		return nil
	})

	// The allocation endpoint is staff/internal: off the gateway, direct to the service.
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": "presale", "cap": 6}},
	}); code != 200 {
		t.Fatalf("allocate %d %s", code, body)
	}
	// Gateway blocking of /internal/ is pinned by TestGatewayDeniesGenericInternalRoutes.
	var presale, public atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1}
			counter := &public
			if i%2 == 0 {
				body["channel"] = "presale"
				counter = &presale
			}
			code, _ := postWithKeyAsync(t, inventory+"/holds", fmt.Sprintf("chan-%s-%d", slot, i), body)
			if code == 201 {
				counter.Add(1)
			} else if code != 409 {
				t.Errorf("hold %d unexpected status %d", i, code)
			}
		}(i)
	}
	wg.Wait()
	if presale.Load() != 6 || public.Load() != 4 {
		t.Fatalf("granted presale=%d public=%d, want exactly 6/4", presale.Load(), public.Load())
	}
	code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s&channel=presale", inventory, slot, organizerID))
	if code != 200 {
		t.Fatalf("channel availability %d %s", code, body)
	}
	var a struct{ Available, Held int32 }
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if a.Available != 0 || a.Held != 10 {
		t.Fatalf("channel availability %+v want available 0, pool held 10", a)
	}
}

// TKT-76 AC2: an adversarial adjust-during-holds interleaving stays oversell-free. A
// deterministic lock queue (blocker → hold → cut → burst, proven with pg_stat_activity
// handshakes, never sleeps) catches an adjustment that skips the pool lock, computes
// demand before acquiring it, or gates later claims against the clamp floor instead of
// the requested target.
func TestCapacityAdjustmentDuringHoldBurstStaysOversellFree(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	inventory := gatewayURL + "/api/inventory"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Adjustment Hall", "ga_capacity": 10})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Test ajustement", "en": "Adjustment test"}})
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": "2026-12-01T20:00:00Z", "timezone": "UTC"})
	created(t, catalog+"/ticket-types", map[string]any{"organizer_id": organizerID, "performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 1000, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != 200 {
		t.Fatalf("publish %d %s", code, body)
	}
	slot := fmt.Sprint(perf["id"])
	retry(t, 20*time.Second, func() error {
		code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s", inventory, slot, organizerID))
		if code != 200 {
			return fmt.Errorf("not provisioned: %d %s", code, body)
		}
		return nil
	})

	// Committed demand of 3 before anything queues.
	if code, body := postWithKey(t, inventory+"/holds", "adjq-base-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 3}); code != 201 {
		t.Fatalf("base hold %d %s", code, body)
	}

	ctx := t.Context()
	blocker, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(ctx) }()
	monitor, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
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

	// waitQueued blocks until `want` service transactions are observed waiting on the
	// pool lock with a query matching `like` — the FIFO lock queue is the ordering proof.
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
	// The hold path selects closure_status under FOR UPDATE; the adjust path selects
	// confirmed_quantity,lifecycle_status — distinguishable queue members.
	holdLike := "%closure_status FROM inventory_pools%FOR UPDATE%"
	adjustLike := "%confirmed_quantity,lifecycle_status FROM inventory_pools%FOR UPDATE%"

	// Every request goroutine below reports on t (a status assertion, or a contract
	// violation via the Async validators), and t.Error after the test function has
	// completed PANICS. The handshake between spawning them and joining them is full of
	// t.Fatal paths — waitQueued's deadline, the explicit tx.Rollback — and the workers
	// are queued on the pool lock this test's transaction holds, so a Goexit from any of
	// them would return from the test while they are still blocked, whereupon the
	// rollback defer at the top releases them into a completed test.
	//
	// So join them in a defer that rolls back FIRST (idempotent — the later error is
	// ignored) and waits second. It is registered after that top-level rollback defer,
	// so LIFO runs it before it, which is the order that terminates. On the happy path
	// everything is already joined and this is a no-op. All three channels are buffered,
	// so a worker's send never blocks even when nobody reads it.
	var workers sync.WaitGroup
	var burst sync.WaitGroup
	defer func() {
		_ = tx.Rollback(ctx)
		workers.Wait()
		burst.Wait()
	}()

	holdDone := make(chan int, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		code, _ := postWithKeyAsync(t, inventory+"/holds", "adjq-first-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
		holdDone <- code
	}()
	waitQueued(holdLike, 1)

	adjustDone := make(chan []byte, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, body := internalJSONAsync(t, http.MethodPost, fmt.Sprintf("%s/internal/slots/%s/capacity-adjustments", inventoryURL, slot), "adjq-cut-"+slot,
			map[string]any{"organizer_id": organizerID, "capacity": 2, "actor": "staff:amy", "reason": "storm damage"})
		adjustDone <- body
	}()
	waitQueued(adjustLike, 1)

	var rejected atomic.Int32
	for i := 0; i < 5; i++ {
		burst.Add(1)
		go func(i int) {
			defer burst.Done()
			code, _ := postWithKeyAsync(t, inventory+"/holds", fmt.Sprintf("adjq-burst-%s-%d", slot, i), map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
			if code == 409 {
				rejected.Add(1)
			} else {
				t.Errorf("burst hold %d unexpected status %d", i, code)
			}
		}(i)
	}
	waitQueued(holdLike, 6) // first hold + 5 burst holds, all queued behind the cut

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// FIFO: the first hold lands (demand 4 ≤ 10), the cut clamps to that demand
	// (target 2 < 4), every burst hold rejects against the target.
	if code := <-holdDone; code != 201 {
		t.Fatalf("first queued hold: %d", code)
	}
	var adj struct {
		Capacity       int32  `json:"capacity"`
		CapacityBefore int32  `json:"capacity_before"`
		TargetCapacity int32  `json:"target_capacity"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(<-adjustDone, &adj); err != nil {
		t.Fatal(err)
	}
	if adj.Status != "clamped" || adj.Capacity != 4 || adj.CapacityBefore != 10 || adj.TargetCapacity != 2 {
		t.Fatalf("adjustment outcome %+v", adj)
	}
	burst.Wait()
	if rejected.Load() != 5 {
		t.Fatalf("burst rejections %d, want 5", rejected.Load())
	}

	code, body, h := getWithHeaders(t, fmt.Sprintf("%s/slots/%s/availability?organizer_id=%s", inventory, slot, organizerID))
	if code != 200 {
		t.Fatalf("availability %d %s", code, body)
	}
	if h.Get("Cache-Control") != "public, max-age=5, s-maxage=5" {
		t.Fatalf("cache tier %q", h.Get("Cache-Control"))
	}
	var a struct{ Capacity, Available, Held, Confirmed int32 }
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	// Oversell-free: effective capacity equals live demand, nothing claimable.
	if a.Capacity != 4 || a.Held != 4 || a.Confirmed != 0 || a.Available != 0 {
		t.Fatalf("post-adjustment accounting %+v", a)
	}
	if code, _ := postWithKey(t, inventory+"/holds", "adjq-after-"+slot, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1}); code != 409 {
		t.Fatalf("hold above target admitted: %d", code)
	}
}
