package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// Bulk performance display names (TKT-222 / US-A3).
//
// This is the read commerce's wallet uses to turn slot ids into a name a buyer
// recognises. Two properties carry the weight: it resolves in ONE call, and it
// answers for performances that are no longer on sale.

func displayNames(t *testing.T, e *env, ids []uuid.UUID, locale string) (*httptest.ResponseRecorder, PerformanceDisplayNames) {
	t.Helper()
	raw := make([]string, 0, len(ids))
	for _, id := range ids {
		raw = append(raw, id.String())
	}
	req := httptest.NewRequest(http.MethodGet,
		"http://catalog.local/internal/performances/display-names?locale="+locale+"&ids="+strings.Join(raw, ","), nil)
	req.Header.Set("X-Internal-Token", "test-internal-token")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	e.validateResponse(req, rec)

	var out PerformanceDisplayNames
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
	}
	return rec, out
}

// The property the wallet depends on and that a published-only resolver would
// break: a purchase is usually for a show that has already happened, and an
// archived or unpublished performance must still have a name.
//
// The fixture therefore holds a DRAFT performance deliberately. A fixture where
// everything is published cannot distinguish a resolver that filters from one
// that does not.
func TestDisplayNamesResolveRegardlessOfPublicationState(t *testing.T) {
	e := newEnv(t)
	published := e.store.seedPerformance(t, "published")
	draft := e.store.seedPerformance(t, "draft")

	rec, out := displayNames(t, e, []uuid.UUID{published, draft}, "en")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(out.Performances) != 2 {
		t.Fatalf("resolved %d of 2 — publication state must not decide whether a purchase can be named: %+v",
			len(out.Performances), out.Performances)
	}
	for _, p := range out.Performances {
		if p.EventName == "" {
			t.Fatalf("performance %s resolved with no name", p.PerformanceId)
		}
	}
}

// One unknown member must not fail the other nineteen: a wallet with one
// unnameable row beats a wallet that will not load.
func TestDisplayNamesOmitUnknownIdsRatherThanFailing(t *testing.T) {
	e := newEnv(t)
	known := e.store.seedPerformance(t, "published")

	rec, out := displayNames(t, e, []uuid.UUID{known, uuid.New()}, "en")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(out.Performances) != 1 || out.Performances[0].PerformanceId != known {
		t.Fatalf("want only the known id, got %+v", out.Performances)
	}
}

func TestDisplayNamesRefuseAnUnsupportedLocale(t *testing.T) {
	e := newEnv(t)
	id := e.store.seedPerformance(t, "published")

	rec, _ := displayNames(t, e, []uuid.UUID{id}, "zz")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Catalog's whole /internal/ surface is guarded by guardInternalSurface
// (ADR-043); this asserts the new route inherited it rather than being an
// exception nobody noticed.
func TestDisplayNamesRequireTheInternalCredential(t *testing.T) {
	e := newEnv(t)
	id := e.store.seedPerformance(t, "published")

	req := httptest.NewRequest(http.MethodGet,
		"http://catalog.local/internal/performances/display-names?locale=en&ids="+id.String(), nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("the route answered without a credential — it is outside the internal guard")
	}
}

// seedPerformance puts one event + one performance in the fake store and returns
// the performance id. `status` is honoured so a fixture can hold a DRAFT — which
// is the only way to observe whether the resolver filters on publication.
func (f *fakeStore) seedPerformance(t *testing.T, status string) uuid.UUID {
	t.Helper()
	eventID, performanceID := uuid.New(), uuid.New()
	starts := time.Date(2026, 9, 18, 17, 30, 0, 0, time.UTC)
	f.events[eventID] = store.Event{
		ID:   eventID,
		Name: store.LocalizedText{"en": "Electric Night " + performanceID.String()[:8], "fr": "Nuit Électrique"},
	}
	f.performances[performanceID] = store.Performance{
		ID: performanceID, EventID: eventID, Status: status, StartsAt: &starts,
	}
	return performanceID
}
