package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-365 (ADR-083). Access asks Catalog for each ticket's performance time zone when it
// serves the bundle. The bundle must load with or without an answer, so every failure
// here is an absent zone, never an error.

type resolverCall struct {
	ids    []string
	locale string
	token  string
}

// fakeResolver answers like catalog's /internal/performances/display-names. zones is the
// catalog state; it answers only the ids it is asked for, in REVERSE order, so a client
// that maps by position instead of by id fails.
type fakeResolver struct {
	mu     sync.Mutex
	calls  []resolverCall
	zones  map[string]string
	status int
	body   string
	delay  time.Duration
}

func (f *fakeResolver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	f.mu.Lock()
	f.calls = append(f.calls, resolverCall{ids: ids, locale: r.URL.Query().Get("locale"), token: r.Header.Get("X-Internal-Token")})
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if r.URL.Path != "/internal/performances/display-names" {
		http.NotFound(w, r)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	if f.body != "" {
		_, _ = w.Write([]byte(f.body))
		return
	}
	type entry struct {
		PerformanceID string  `json:"performance_id"`
		EventName     string  `json:"event_name"`
		StartsAt      *string `json:"starts_at"`
		Timezone      string  `json:"timezone"`
	}
	out := struct {
		Performances []entry `json:"performances"`
	}{Performances: []entry{}}
	for i := len(ids) - 1; i >= 0; i-- {
		if zone, ok := f.zones[ids[i]]; ok {
			out.Performances = append(out.Performances, entry{PerformanceID: ids[i], EventName: "e", Timezone: zone})
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func newClient(t *testing.T, f *fakeResolver) Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Client{HTTP: srv.Client(), BaseURL: srv.URL + "/", Token: "internal-token"}
}

func TestZonesMapByIDAndSendTheCredentialAndAFixedLocale(t *testing.T) {
	a, b, unknown := uuid.New(), uuid.New(), uuid.New()
	f := &fakeResolver{zones: map[string]string{a.String(): "Asia/Kolkata", b.String(): "Europe/Paris"}}

	got := newClient(t, f).PerformanceZones(context.Background(), []uuid.UUID{a, b, unknown})

	want := map[uuid.UUID]string{a: "Asia/Kolkata", b: "Europe/Paris"}
	if len(got) != len(want) || got[a] != want[a] || got[b] != want[b] {
		t.Fatalf("zones = %v, want %v (an unknown id is absent, not an error)", got, want)
	}
	if _, ok := got[unknown]; ok {
		t.Fatalf("an unknown performance has a zone: %v", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	if f.calls[0].token != "internal-token" {
		t.Fatalf("X-Internal-Token = %q", f.calls[0].token)
	}
	if f.calls[0].locale != "en" {
		t.Fatalf("locale = %q, want the fixed supported locale en", f.calls[0].locale)
	}
}

// The resolver accepts at most 20 ids. 25 distinct ids, each listed twice, must go out as
// two requests that name every id exactly once.
func TestZonesDeduplicateAndChunkByTwenty(t *testing.T) {
	zones := map[string]string{}
	ids := make([]uuid.UUID, 0, 50)
	for range 25 {
		id := uuid.New()
		zones[id.String()] = "Asia/Kolkata"
		ids = append(ids, id, id)
	}
	f := &fakeResolver{zones: zones}

	got := newClient(t, f).PerformanceZones(context.Background(), ids)

	if len(got) != 25 {
		t.Fatalf("resolved %d zones, want 25", len(got))
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(f.calls))
	}
	seen := map[string]int{}
	for _, call := range f.calls {
		if len(call.ids) > MaxIDsPerRequest {
			t.Fatalf("one request named %d ids, over the resolver's bound of %d", len(call.ids), MaxIDsPerRequest)
		}
		for _, id := range call.ids {
			seen[id]++
		}
	}
	for id := range zones {
		if seen[id] != 1 {
			t.Fatalf("id %s was requested %d times, want once", id, seen[id])
		}
	}
}

// A zone that does not belong to an id this client asked for is not trusted, and an
// empty zone is not a zone.
func TestZonesIgnoreUnrequestedAndEmptyEntries(t *testing.T) {
	asked, stranger := uuid.New(), uuid.New()
	f := &fakeResolver{body: `{"performances":[` +
		`{"performance_id":"` + stranger.String() + `","event_name":"e","starts_at":null,"timezone":"Europe/Paris"},` +
		`{"performance_id":"` + asked.String() + `","event_name":"e","starts_at":null,"timezone":""}]}`}

	got := newClient(t, f).PerformanceZones(context.Background(), []uuid.UUID{asked})

	if len(got) != 0 {
		t.Fatalf("zones = %v, want none", got)
	}
}

func TestZonesAreAbsentOnAnyFailure(t *testing.T) {
	id := uuid.New()
	for name, f := range map[string]*fakeResolver{
		"500":         {status: http.StatusInternalServerError, body: `{"error":"x"}`},
		"401":         {status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
		"undecodable": {body: `<html>`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := newClient(t, f).PerformanceZones(context.Background(), []uuid.UUID{id}); len(got) != 0 {
				t.Fatalf("zones = %v, want none", got)
			}
		})
	}
	unreachable := Client{HTTP: http.DefaultClient, BaseURL: "http://127.0.0.1:1", Token: "t"}
	if got := unreachable.PerformanceZones(context.Background(), []uuid.UUID{id}); len(got) != 0 {
		t.Fatalf("zones = %v, want none", got)
	}
}

// One failed chunk loses only its own ids.
func TestOneFailedChunkKeepsTheOthers(t *testing.T) {
	zones := map[string]string{}
	ids := make([]uuid.UUID, 0, 21)
	for range 21 {
		id := uuid.New()
		zones[id.String()] = "Europe/Paris"
		ids = append(ids, id)
	}
	f := &fakeResolver{zones: zones}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(strings.Split(r.URL.Query().Get("ids"), ",")) == MaxIDsPerRequest {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	got := Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}.PerformanceZones(context.Background(), ids)

	if len(got) != 1 {
		t.Fatalf("resolved %d zones, want the 1 from the chunk that answered", len(got))
	}
}

// The bundle is on the buyer's path to the gate. A slow Catalog must cost at most the
// client's own deadline, which stays well under the storefront's 5 s upstream bound.
func TestZonesGiveUpAtTheDeadline(t *testing.T) {
	if DefaultTimeout > 2*time.Second {
		t.Fatalf("DefaultTimeout = %s, want at most 2s (ADR-083)", DefaultTimeout)
	}
	id := uuid.New()
	f := &fakeResolver{zones: map[string]string{id.String(): "Asia/Kolkata"}, delay: 3 * time.Second}
	c := newClient(t, f)
	c.Timeout = 100 * time.Millisecond

	start := time.Now()
	got := c.PerformanceZones(context.Background(), []uuid.UUID{id})
	elapsed := time.Since(start)

	if len(got) != 0 {
		t.Fatalf("zones = %v, want none after the deadline", got)
	}
	if elapsed > time.Second {
		t.Fatalf("the call took %s; the deadline did not bound it", elapsed)
	}
}

// One deadline for the WHOLE call, not one per request (ADR-083). 25 ids are two requests
// in sequence, each answering after 400 ms, under a 600 ms deadline: the first request
// fits, and the second is cut off at the shared deadline. A per-request deadline would let
// both finish (each is under 600 ms), resolve all 25, and take 800 ms or more.
func TestOneDeadlineCoversEveryChunk(t *testing.T) {
	const timeout, delay = 600 * time.Millisecond, 400 * time.Millisecond
	zones := map[string]string{}
	ids := make([]uuid.UUID, 0, 25)
	for range 25 {
		id := uuid.New()
		zones[id.String()] = "Asia/Kolkata"
		ids = append(ids, id)
	}
	f := &fakeResolver{zones: zones, delay: delay}
	c := newClient(t, f)
	c.Timeout = timeout

	start := time.Now()
	got := c.PerformanceZones(context.Background(), ids)
	elapsed := time.Since(start)

	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (the fixture must reach the second chunk)", len(f.calls))
	}
	if len(got) != MaxIDsPerRequest {
		t.Fatalf("resolved %d zones, want the %d of the first chunk only: the second request must end at the shared deadline",
			len(got), MaxIDsPerRequest)
	}
	if elapsed >= 2*delay {
		t.Fatalf("the call took %s; one %s deadline must end it before two %s requests could finish",
			elapsed, timeout, delay)
	}
}

func TestNoIDsMakeNoRequest(t *testing.T) {
	f := &fakeResolver{}
	if got := newClient(t, f).PerformanceZones(context.Background(), nil); len(got) != 0 {
		t.Fatalf("zones = %v", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(f.calls))
	}
}
