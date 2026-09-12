package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/availability"
	"ticketing/services/inventory/internal/seatoccupancy"
	"ticketing/services/inventory/internal/store"
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

// splitProbeReader parks the DISABLE request between the handler's two
// collaborator writes. Keying the park on the value — rather than on a
// sync.Once — is what keeps the probe from blocking the enable request that has
// to run inside that window.
type splitProbeReader struct {
	mu      sync.Mutex
	enabled bool
	entries int
	enter   chan struct{}
	hold    chan struct{}
	parked  bool
}

func (r *splitProbeReader) SetEnabled(v bool) {
	r.mu.Lock()
	r.enabled = v
	park := !v && !r.parked
	if park {
		r.parked = true
	}
	r.mu.Unlock()
	if park {
		r.enter <- struct{}{}
		<-r.hold
	}
}

func (r *splitProbeReader) isEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

func (r *splitProbeReader) Status() availability.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return availability.Status{Enabled: r.enabled, Entries: r.entries}
}

func (r *splitProbeReader) Read(context.Context, uuid.UUID, uuid.UUID, string) (availability.Read, error) {
	return availability.Read{}, nil
}

// signallingOccupancyReader announces every SetEnabled on a channel, so the test
// waits on the write itself rather than on a clock. A wall-clock window would
// pass on a loaded machine simply because the second request never got
// scheduled — the test would go green with the defect live, which is the one
// outcome a concurrency test must not have.
type signallingOccupancyReader struct {
	mu      sync.Mutex
	enabled bool
	entries int
	writes  chan bool
}

func (c *signallingOccupancyReader) SetEnabled(v bool) {
	c.mu.Lock()
	c.enabled = v
	c.mu.Unlock()
	c.writes <- v
}

func (c *signallingOccupancyReader) isEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

func (c *signallingOccupancyReader) Status() seatoccupancy.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return seatoccupancy.Status{Enabled: c.enabled, Entries: c.entries}
}

func (c *signallingOccupancyReader) Read(_ context.Context, _, slot uuid.UUID) (seatoccupancy.Read, error) {
	return seatoccupancy.Read{Value: store.SeatOccupancy{SlotID: slot, Unavailable: []string{}}}, nil
}

// TestConcurrentTogglesCannotSplitTheCaches is the kill switch's atomicity, and
// it is the property an incident depends on.
//
// The handler sets two collaborators in sequence. With no lock spanning both,
// two overlapping requests interleave — a disable sets availability, an enable
// then sets BOTH caches, and the disable finally sets occupancy — and the
// process is left serving availability from memory while the status route,
// which ANDs the two flags, answers `enabled:false`. An operator reading "the
// cache is off" mid-incident while a cache still answers from memory is the
// precise failure this surface exists to prevent. Reported state must never be
// more reassuring than the read path.
//
// The probe parks the disable between the handler's two writes. Under the fixed
// handler the enable cannot enter at all, so it never reaches occupancy and the
// park releases on a timer; under the broken one it runs straight through and
// the two caches finish disagreeing.
func TestConcurrentTogglesCannotSplitTheCaches(t *testing.T) {
	availRd := &splitProbeReader{
		enabled: true,
		enter:   make(chan struct{}, 1),
		hold:    make(chan struct{}),
	}
	// Occupancy starts DISABLED and reports every write. Starting it enabled
	// would let the detector fire on the initial state and prove nothing; the
	// signal is what makes the observation an event rather than a poll.
	occRd := &signallingOccupancyReader{enabled: false, writes: make(chan bool, 4)}
	h := NewWithReaders(nil, "secret", nil, availRd, occRd).Router(nil, true)

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		cacheControlReq(t, h, http.MethodPut, "secret", `{"enabled":false}`)
	}()
	<-availRd.enter // the disable now holds availability=false, mid-handler

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		cacheControlReq(t, h, http.MethodPut, "secret", `{"enabled":true}`)
	}()

	// The disable is parked between its two writes. If any occupancy write lands
	// now, it came from the enable request, which means the two requests
	// interleaved: the switch is not moving both caches as one step.
	//
	// A correct handler holds the enable at the lock, so nothing arrives and the
	// disable's own write is the first — which is why the disable is released
	// only after this select settles.
	select {
	case v := <-occRd.writes:
		close(availRd.hold)
		<-aDone
		<-bDone
		t.Fatalf("an occupancy write (enabled=%v) landed while the disable request was parked "+
			"between its own two writes: a disable and an enable interleaved, so one cache can "+
			"be left serving from memory while the status route reports the switch as off", v)
	case <-time.After(100 * time.Millisecond):
		// Nothing got through: the enable is blocked at the lock, as intended.
	}

	close(availRd.hold)
	<-aDone
	<-bDone

	// Drain both requests' writes and confirm the caches agree at rest.
	for len(occRd.writes) > 0 {
		<-occRd.writes
	}
	if availRd.isEnabled() != occRd.isEnabled() {
		t.Fatalf("the caches split at rest: avail.enabled=%v, occ.enabled=%v",
			availRd.isEnabled(), occRd.isEnabled())
	}
}
