package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/store"
)

// fakeStore is an in-memory Store. It mirrors the referential/tenancy checks
// the SQL enforces; the real queries are exercised by the smoke suite.
type fakeStore struct {
	venues       map[uuid.UUID]store.Venue
	events       map[uuid.UUID]store.Event
	performances map[uuid.UUID]store.Performance
	ticketTypes  map[uuid.UUID]store.TicketType
	emitted      map[uuid.UUID]bool // performance id -> event_emitted_at set
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		venues:       map[uuid.UUID]store.Venue{},
		events:       map[uuid.UUID]store.Event{},
		performances: map[uuid.UUID]store.Performance{},
		ticketTypes:  map[uuid.UUID]store.TicketType{},
		emitted:      map[uuid.UUID]bool{},
	}
}

func (f *fakeStore) CreateVenue(_ context.Context, in store.VenueInput) (store.Venue, error) {
	v := store.Venue{ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name,
		GACapacity: in.GACapacity, CreatedAt: time.Now().UTC()}
	f.venues[v.ID] = v
	return v, nil
}

func (f *fakeStore) CreateEvent(_ context.Context, in store.EventInput) (store.Event, error) {
	e := store.Event{ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name,
		Description: in.Description, CreatedAt: time.Now().UTC()}
	f.events[e.ID] = e
	return e, nil
}

func (f *fakeStore) CreatePerformance(_ context.Context, in store.PerformanceInput) (store.Performance, error) {
	ev, ok := f.events[in.EventID]
	if !ok {
		return store.Performance{}, fmt.Errorf("event: %w", store.ErrNotFound)
	}
	v, ok := f.venues[in.VenueID]
	if !ok {
		return store.Performance{}, fmt.Errorf("venue: %w", store.ErrNotFound)
	}
	if ev.OrganizerID != in.OrganizerID || v.OrganizerID != in.OrganizerID {
		return store.Performance{}, store.ErrOrganizerMismatch
	}
	p := store.Performance{ID: uuid.New(), OrganizerID: in.OrganizerID, EventID: in.EventID,
		VenueID: in.VenueID, StartsAt: in.StartsAt, Timezone: in.Timezone,
		Status: "draft", CreatedAt: time.Now().UTC()}
	f.performances[p.ID] = p
	return p, nil
}

func (f *fakeStore) CreateTicketType(_ context.Context, in store.TicketTypeInput) (store.TicketType, error) {
	p, ok := f.performances[in.PerformanceID]
	if !ok {
		return store.TicketType{}, fmt.Errorf("performance: %w", store.ErrNotFound)
	}
	if p.OrganizerID != in.OrganizerID {
		return store.TicketType{}, store.ErrOrganizerMismatch
	}
	tt := store.TicketType{ID: uuid.New(), OrganizerID: in.OrganizerID,
		PerformanceID: in.PerformanceID, Name: in.Name,
		PriceAmount: in.PriceAmount, Currency: in.Currency, CreatedAt: time.Now().UTC()}
	f.ticketTypes[tt.ID] = tt
	return tt, nil
}

func (f *fakeStore) PublishPerformance(_ context.Context, id uuid.UUID) (store.Performance, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, store.ErrNotFound
	}
	if p.Status != "published" {
		now := time.Now().UTC()
		p.Status = "published"
		p.PublishedAt = &now
		f.performances[id] = p
	}
	return p, !f.emitted[id], nil
}

func (f *fakeStore) MarkPerformanceEventEmitted(_ context.Context, id uuid.UUID) error {
	f.emitted[id] = true
	return nil
}

func (f *fakeStore) aggregates() []store.EventAggregate {
	var aggs []store.EventAggregate
	for _, ev := range f.events {
		agg := store.EventAggregate{Event: ev}
		for _, p := range f.performances {
			if p.EventID != ev.ID || p.Status != "published" {
				continue
			}
			pa := store.PerformanceAggregate{Performance: p, Venue: f.venues[p.VenueID]}
			for _, tt := range f.ticketTypes {
				if tt.PerformanceID == p.ID {
					pa.TicketTypes = append(pa.TicketTypes, tt)
				}
			}
			if len(pa.TicketTypes) > 0 { // no sellable offer, no listing
				agg.Performances = append(agg.Performances, pa)
			}
		}
		if len(agg.Performances) > 0 {
			aggs = append(aggs, agg)
		}
	}
	return aggs
}

func (f *fakeStore) ListPublishedEvents(_ context.Context) ([]store.EventAggregate, error) {
	return f.aggregates(), nil
}

func (f *fakeStore) GetPublishedEvent(_ context.Context, id uuid.UUID) (store.EventAggregate, error) {
	for _, agg := range f.aggregates() {
		if agg.Event.ID == id {
			return agg, nil
		}
	}
	return store.EventAggregate{}, store.ErrNotFound
}

type fakePublisher struct {
	published []store.Performance
	failNext  bool
}

func (f *fakePublisher) PerformancePublished(_ context.Context, p store.Performance) error {
	if f.failNext {
		f.failNext = false
		return errors.New("nats down")
	}
	f.published = append(f.published, p)
	return nil
}

type env struct {
	store   *fakeStore
	pub     *fakePublisher
	handler http.Handler
	router  routers.Router // spec router for response validation
	t       *testing.T
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := newFakeStore()
	pub := &fakePublisher{}
	h, err := NewRouter(NewServer(st, pub, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("spec router: %v", err)
	}
	return &env{store: st, pub: pub, handler: h, router: router, t: t}
}

// do performs a request and validates the response against the spec
// (ADR-009 §3: conformance is tested in both directions).
func (e *env) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "http://catalog.local"+path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	e.validateResponse(req, rec)
	return rec
}

func (e *env) validateResponse(req *http.Request, rec *httptest.ResponseRecorder) {
	e.t.Helper()
	if req.URL.Path == "/openapi.yaml" {
		return // the YAML document is asserted byte-identical, not schema-validated
	}
	route, pathParams, err := e.router.FindRoute(req)
	if err != nil {
		return // route not in spec (spec middleware already rejected it)
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: pathParams, Route: route,
		},
		Status: rec.Code,
		Header: rec.Header(),
		Body:   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		e.t.Fatalf("response for %s %s violates the contract: %v", req.Method, req.URL.Path, err)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %T: %v (body: %s)", out, err, rec.Body.String())
	}
	return out
}

var orgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func (e *env) createFixture(publish bool) (eventID, performanceID uuid.UUID) {
	e.t.Helper()
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Le Zénith", GaCapacity: 500}))
	desc := LocalizedString{"fr": "Une soirée électro.", "en": "An electro night."}
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID,
		Name:        LocalizedString{"fr": "Nuit Électrique", "en": "Electric Night"},
		Description: &desc,
	}))
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: time.Date(2026, 9, 18, 19, 30, 0, 0, time.UTC), Timezone: "Europe/Paris",
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: perf.Id,
		Name:  LocalizedString{"fr": "Admission générale", "en": "General admission"},
		Price: Money{Amount: 4550, Currency: "EUR"},
	})
	if publish {
		rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
		}
	}
	return event.Id, perf.Id
}

func TestCreateVenue(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 1200})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("write endpoints must be no-store, got %q", cc)
	}
}

func TestCreateVenueRejectsInvalidBody(t *testing.T) {
	e := newEnv(t)
	// ga_capacity below minimum: rejected by the spec middleware, not handler code.
	rec := e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEventRequiresAllSupportedLocales(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Sans anglais"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decode[Error](t, rec).Error; !strings.Contains(msg, `"en"`) {
		t.Fatalf("error should name the missing locale: %q", msg)
	}
}

func TestCreatePerformanceValidations(t *testing.T) {
	e := newEnv(t)
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 100}))
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "F", "en": "E"},
	}))

	base := PerformanceCreate{OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: time.Now().UTC(), Timezone: "Europe/Paris"}

	unknownEvent := base
	unknownEvent.EventId = uuid.New()
	if rec := e.do("POST", "/performances", unknownEvent); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown event: status %d", rec.Code)
	}

	badTZ := base
	badTZ.Timezone = "Mars/Olympus"
	if rec := e.do("POST", "/performances", badTZ); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad timezone: status %d", rec.Code)
	}

	crossOrg := base
	crossOrg.OrganizerId = uuid.New()
	if rec := e.do("POST", "/performances", crossOrg); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-organizer: status %d", rec.Code)
	}

	if rec := e.do("POST", "/performances", base); rec.Code != http.StatusCreated {
		t.Fatalf("valid: status %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublishEmitsExactlyOnceOnTransition(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	if len(e.pub.published) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(e.pub.published))
	}
	// Idempotent re-publish: 200, no second emission.
	rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-publish: %d", rec.Code)
	}
	if len(e.pub.published) != 1 {
		t.Fatalf("re-publish must not re-emit, got %d emissions", len(e.pub.published))
	}
	if got := e.pub.published[0].ID; got != perfID {
		t.Fatalf("emitted performance %s, want %s", got, perfID)
	}
}

func TestPublishRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	e.pub.failNext = true
	rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed emission should 500, got %d", rec.Code)
	}
	// The performance is published but the event is still owed: retry emits.
	rec = e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: %d", rec.Code)
	}
	if len(e.pub.published) != 1 {
		t.Fatalf("retry should emit exactly once, got %d", len(e.pub.published))
	}
}

func TestPublishUnknownPerformance(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/performances/"+uuid.NewString()+"/publish", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestPublicListIsLocalizedAndCacheTiered(t *testing.T) {
	e := newEnv(t)
	e.createFixture(true)

	for locale, wantName := range map[string]string{"fr": "Nuit Électrique", "en": "Electric Night"} {
		rec := e.do("GET", "/public/events?locale="+locale, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("[%s] status %d: %s", locale, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != CacheControlPublicReads {
			t.Fatalf("[%s] public reads carry the minutes tier, got %q", locale, cc)
		}
		list := decode[PublicEventList](t, rec)
		if len(list.Events) != 1 {
			t.Fatalf("[%s] want 1 event, got %d", locale, len(list.Events))
		}
		ev := list.Events[0]
		if ev.Name != wantName {
			t.Fatalf("[%s] name %q, want %q", locale, ev.Name, wantName)
		}
		if len(ev.Performances) != 1 {
			t.Fatalf("[%s] want 1 performance, got %d", locale, len(ev.Performances))
		}
		p := ev.Performances[0]
		if p.FromPrice.Amount != 4550 || p.FromPrice.Currency != "EUR" {
			t.Fatalf("[%s] from_price %+v", locale, p.FromPrice)
		}
		if p.VenueName != "Le Zénith" {
			t.Fatalf("[%s] venue %q", locale, p.VenueName)
		}
	}
}

func TestPublicListExcludesDraftsAndUnpriced(t *testing.T) {
	e := newEnv(t)
	e.createFixture(false) // draft: not listed

	// Published but without a ticket type: no sellable offer, no listing.
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Halle B", GaCapacity: 50}))
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Brouillon", "en": "Draft"},
	}))
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: time.Now().UTC(), Timezone: "Europe/Paris",
	}))
	e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil)

	list := decode[PublicEventList](t, e.do("GET", "/public/events?locale=en", nil))
	if len(list.Events) != 0 {
		t.Fatalf("draft/unpriced performances must not be listed, got %d events", len(list.Events))
	}
}

func TestPublicListRejectsUnsupportedLocale(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/public/events?locale=xx", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	// Missing locale: rejected by the spec middleware (required param).
	if rec := e.do("GET", "/public/events", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing locale: status %d", rec.Code)
	}
}

func TestPublicDetail(t *testing.T) {
	e := newEnv(t)
	eventID, _ := e.createFixture(true)

	rec := e.do("GET", "/public/events/"+eventID.String()+"?locale=fr", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != CacheControlPublicReads {
		t.Fatalf("Cache-Control %q", cc)
	}
	detail := decode[PublicEventDetail](t, rec)
	if detail.Name != "Nuit Électrique" {
		t.Fatalf("name %q", detail.Name)
	}
	tts := detail.Performances[0].TicketTypes
	if len(tts) != 1 || tts[0].Name != "Admission générale" || tts[0].Price.Amount != 4550 {
		t.Fatalf("ticket types %+v", tts)
	}

	if rec := e.do("GET", "/public/events/"+uuid.NewString()+"?locale=fr", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown event: status %d", rec.Code)
	}
}

func TestOpenAPISpecServedVerbatim(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/openapi.yaml", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), apispec.Spec) {
		t.Fatal("served spec must be byte-identical to the committed contract (ADR-009)")
	}
}
