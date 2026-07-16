//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func holdRequest(url, key string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

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
	code, body := holdRequest(inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
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
			code, _ := holdRequest(inventory+"/holds", fmt.Sprintf("parallel-%s-%d", slot, i), map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
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
	code, body = holdRequest(inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != 200 {
		t.Fatalf("idempotent replay %d %s", code, body)
	}
	code, _ = holdRequest(inventory+"/holds", knownKey, map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 2})
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
			code, _ := holdRequest(inventory+"/holds", fmt.Sprintf("chan-%s-%d", slot, i), body)
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
