//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// The ADR-004 incident kill-switch, end to end (TKT-210).
//
// Catalog's internal surface is not routed through the gateway, so this drives
// both services on their loopback ports — the same path an operator uses.
//
// The helpers here are the seam TKT-207 consumes for its control arm: it needs
// to disable both caches, verify both report disabled, run a load stage, and
// restore both even after a t.Fatalf.

// catalogURL is catalog's loopback port, published since TKT-210 so an operator
// (and this suite) can reach its internal surface. Every other service already
// had one.
var catalogURL = env("SMOKE_CATALOG_URL", "http://localhost:8090")

type cacheState struct {
	Enabled bool `json:"enabled"`
	Entries int  `json:"entries"`
}

func cacheControlGet(t *testing.T, baseURL string) cacheState {
	t.Helper()
	return cacheControlCall(t, http.MethodGet, baseURL, nil)
}

func cacheControlSet(t *testing.T, baseURL string, enabled bool) cacheState {
	t.Helper()
	return cacheControlCall(t, http.MethodPut, baseURL, map[string]bool{"enabled": enabled})
}

func cacheControlCall(t *testing.T, method, baseURL string, body any) cacheState {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+"/internal/cache-control", rd)
	if err != nil {
		t.Fatalf("cache-control request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", internalToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s cache-control: %v", method, baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s cache-control: %d %s", method, baseURL, resp.StatusCode, out)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("%s cache-control answered Cache-Control %q, want no-store", baseURL, cc)
	}
	var st cacheState
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("cache-control body %s: %v", out, err)
	}
	return st
}

// disableBothCaches takes both service caches out of service and proves they are
// out. The restore is registered BEFORE the first mutation, deliberately: a
// failure between the two disables would otherwise leave one service bypassing
// its cache for every later test in the run.
func disableBothCaches(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { restoreBothCaches(t) })
	for _, u := range []string{inventoryURL, catalogURL} {
		if st := cacheControlSet(t, u, false); st.Enabled {
			t.Fatalf("%s reported enabled after being disabled", u)
		}
	}
	for _, u := range []string{inventoryURL, catalogURL} {
		if st := cacheControlGet(t, u); st.Enabled || st.Entries != 0 {
			t.Fatalf("%s reads back %+v, want disabled and purged", u, st)
		}
	}
}

// restoreBothCaches re-enables both. Idempotent, and it attempts BOTH services
// even if the first fails — a half-restored stack is worse than a failed test,
// because the damage lands on whatever runs next.
func restoreBothCaches(t *testing.T) {
	t.Helper()
	for _, u := range []string{inventoryURL, catalogURL} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("restoring %s panicked: %v", u, r)
				}
			}()
			if st := cacheControlSet(t, u, true); !st.Enabled {
				t.Errorf("%s did not re-enable", u)
			}
		}()
	}
}

// TestCacheControlTogglesBothRunningServices is COS 1-3 against the real stack:
// a running process changes behaviour with no restart, and says so.
func TestCacheControlTogglesBothRunningServices(t *testing.T) {
	for _, u := range []string{inventoryURL, catalogURL} {
		if st := cacheControlGet(t, u); !st.Enabled {
			t.Fatalf("%s starts disabled (%+v) — the default must be enabled", u, st)
		}
	}

	disableBothCaches(t)

	// Still serving traffic while bypassed: a kill-switch that breaks the reads
	// it protects is not a kill-switch.
	for _, probe := range []string{
		gatewayURL + "/api/catalog/public/events?locale=en",
	} {
		resp, err := http.Get(probe)
		if err != nil {
			t.Fatalf("GET %s while bypassed: %v", probe, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s while bypassed: %d, want 200", probe, resp.StatusCode)
		}
	}

	restoreBothCaches(t)
	for _, u := range []string{inventoryURL, catalogURL} {
		if st := cacheControlGet(t, u); !st.Enabled {
			t.Fatalf("%s did not come back: %+v", u, st)
		}
	}
}

// TestCacheControlRefusesAnUnauthenticatedCaller: the surface is guarded on both
// services, and the refusal is 401 — matching every other internal route in each
// of them. The gateway separately answers 404 for /api/<svc>/internal/*, which
// is what conceals the surface from outside; this is the service-side guard.
func TestCacheControlRefusesAnUnauthenticatedCaller(t *testing.T) {
	for _, u := range []string{inventoryURL, catalogURL} {
		for _, tok := range []string{"", "wrong-token"} {
			req, err := http.NewRequest(http.MethodGet, u+"/internal/cache-control", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tok != "" {
				req.Header.Set("X-Internal-Token", tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", u, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s with token %q: %d, want 401", u, tok, resp.StatusCode)
			}
		}
	}
}

// TestCacheControlIsNotReachableThroughTheGateway: the edge denies the whole
// internal surface. This is the property that keeps the switch off the internet
// regardless of what the service-side guard does.
func TestCacheControlIsNotReachableThroughTheGateway(t *testing.T) {
	for _, svc := range []string{"catalog", "inventory"} {
		url := fmt.Sprintf("%s/api/%s/internal/cache-control", gatewayURL, svc)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Internal-Token", internalToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404 from the edge even WITH a valid token", url, resp.StatusCode)
		}
	}
}
