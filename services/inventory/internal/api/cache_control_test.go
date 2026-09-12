package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func cacheControlReq(t *testing.T, h http.Handler, method, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "http://inventory.local/internal/cache-control", rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCacheControlRequiresTheInternalCredential.
//
// The PUT bodies here are deliberately SCHEMA-VALID. With an invalid body the
// contract validator answers 400 before the guard ever runs, and the test would
// pass while proving nothing about authentication — a green light for an
// unguarded operator surface.
func TestCacheControlRequiresTheInternalCredential(t *testing.T) {
	h := NewWithAvailability(nil, "secret", nil, &countingReader{}).Router(nil, true)

	for _, tc := range []struct{ name, method, token, body string }{
		{"GET without a token", http.MethodGet, "", ""},
		{"GET with the wrong token", http.MethodGet, "wrong", ""},
		{"PUT without a token", http.MethodPut, "", `{"enabled":false}`},
		{"PUT with the wrong token", http.MethodPut, "wrong", `{"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := cacheControlReq(t, h, tc.method, tc.token, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d %s, want 401 — matching every other internal route in this service", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestCacheControlReportsAndTogglesTheLiveCollaborator pins that the switch
// addresses the same object the public read uses. A control surface that
// reported state from somewhere else would look correct and change nothing.
func TestCacheControlReportsAndTogglesTheLiveCollaborator(t *testing.T) {
	availRd := &countingReader{enabled: true, entries: 3}
	occRd := &countingOccupancyReader{enabled: true, entries: 2}
	h := NewWithReaders(nil, "secret", nil, availRd, occRd).Router(nil, true)

	rec := cacheControlReq(t, h, http.MethodGet, "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"entries":5`) {
		t.Fatalf("GET reported %s, want enabled:true entries:5 (summed across both caches)", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store — a cached answer about whether a cache is on is the wrong thing to hand an operator", cc)
	}

	rec = cacheControlReq(t, h, http.MethodPut, "secret", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if availRd.enabled || occRd.enabled {
		t.Fatalf("PUT did not toggle both collaborators: avail=%v, occ=%v", availRd.enabled, occRd.enabled)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("PUT returned %s, want the resulting state", body)
	}

	// Idempotent, and re-enabling works.
	rec = cacheControlReq(t, h, http.MethodPut, "secret", `{"enabled":false}`)
	if rec.Code != http.StatusOK || availRd.enabled || occRd.enabled {
		t.Fatalf("repeat disable: %d, avail.enabled=%v, occ.enabled=%v", rec.Code, availRd.enabled, occRd.enabled)
	}
	rec = cacheControlReq(t, h, http.MethodPut, "secret", `{"enabled":true}`)
	if rec.Code != http.StatusOK || !availRd.enabled || !occRd.enabled {
		t.Fatalf("re-enable: %d, avail.enabled=%v, occ.enabled=%v", rec.Code, availRd.enabled, occRd.enabled)
	}
}

// TestCacheControlRejectsAnAmbiguousBodyWithoutChangingState: a missing
// `enabled` must not be read as "disable". An operator surface that infers a
// destructive default from an absent field is how a malformed script takes a
// cache down.
func TestCacheControlRejectsAnAmbiguousBodyWithoutChangingState(t *testing.T) {
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"false"}`, `{"enabled":0}`} {
		rd := &countingReader{enabled: true}
		h := NewWithAvailability(nil, "secret", nil, rd).Router(nil, true)
		rec := cacheControlReq(t, h, http.MethodPut, "secret", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: got %d, want 400", body, rec.Code)
		}
		if !rd.enabled {
			t.Errorf("PUT %s changed the cache state despite being refused", body)
		}
	}
}

// TestCacheControlIsNotReachableWithAPublicRoute guards the one thing the
// gateway cannot: that this stayed under /internal/, which the edge denies.
func TestCacheControlIsNotReachableWithAPublicRoute(t *testing.T) {
	h := NewWithAvailability(nil, "secret", nil, &countingReader{}).Router(nil, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://inventory.local/cache-control?organizer_id="+uuid.NewString(), nil))
	if rec.Code == http.StatusOK {
		t.Fatal("a non-internal cache-control path answered 200 — the switch must live under /internal/")
	}
}
