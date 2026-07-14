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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/store"
)

// fakeStore is an in-memory Store. It mirrors the referential/tenancy checks
// the SQL enforces; the real queries are exercised by the smoke suite.
type fakeStore struct {
	venues         map[uuid.UUID]store.Venue
	events         map[uuid.UUID]store.Event
	performances   map[uuid.UUID]store.Performance
	ticketTypes    map[uuid.UUID]store.TicketType
	emitted        map[uuid.UUID]bool // performance id -> event_emitted_at set
	archiveEmitted map[uuid.UUID]bool
	closureEmitted map[uuid.UUID]int32 // performance id -> closure_emitted_version
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		venues:         map[uuid.UUID]store.Venue{},
		events:         map[uuid.UUID]store.Event{},
		performances:   map[uuid.UUID]store.Performance{},
		ticketTypes:    map[uuid.UUID]store.TicketType{},
		emitted:        map[uuid.UUID]bool{},
		archiveEmitted: map[uuid.UUID]bool{},
		closureEmitted: map[uuid.UUID]int32{},
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
	kind := in.Kind
	if kind == "" {
		kind = store.KindPerformance
	}
	re := in.ReEntry
	if re.Mode == "" {
		re.Mode = "single"
	}
	p := store.Performance{ID: uuid.New(), OrganizerID: in.OrganizerID, EventID: in.EventID,
		VenueID: in.VenueID, Kind: kind, StartsAt: in.StartsAt, OperatingDate: in.OperatingDate,
		OpensAt: in.OpensAt, ClosesAt: in.ClosesAt, Timezone: in.Timezone, ReEntry: re,
		Closure: store.Closure{Status: "open"},
		Status:  "draft", Capacity: v.GACapacity, CreatedAt: time.Now().UTC()}
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

func (f *fakeStore) GetTicketType(_ context.Context, id uuid.UUID) (store.TicketType, error) {
	tt, ok := f.ticketTypes[id]
	if !ok {
		return store.TicketType{}, store.ErrNotFound
	}
	return tt, nil
}

func (f *fakeStore) GetPublishedPerformance(_ context.Context, id uuid.UUID) (store.Performance, error) {
	p, ok := f.performances[id]
	if !ok || p.Status != "published" {
		return store.Performance{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) PublishPerformance(_ context.Context, id uuid.UUID) (store.Performance, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, store.ErrNotFound
	}
	if p.Status == "draft" && !f.hasTicketType(id) {
		return store.Performance{}, false, store.ErrNotSellable
	}
	if p.Status == "archived" {
		return store.Performance{}, false, store.ErrIllegalTransition
	}
	if p.Status == "draft" {
		now := time.Now().UTC()
		p.Status = "published"
		p.PublishedAt = &now
		f.performances[id] = p
	}
	return p, !f.emitted[id], nil
}

func (f *fakeStore) ArchivePerformance(_ context.Context, id uuid.UUID) (store.Performance, bool, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, false, store.ErrNotFound
	}
	if p.Status == "draft" {
		return store.Performance{}, false, false, store.ErrIllegalTransition
	}
	if p.Status == "published" {
		if f.closureEmitted[id] < p.Closure.Version {
			return store.Performance{}, false, false, store.ErrClosurePending
		}
		now := time.Now().UTC()
		p.Status = "archived"
		p.ArchivedAt = &now
		f.performances[id] = p
	}
	return p, !f.emitted[id], !f.archiveEmitted[id], nil
}

func (f *fakeStore) hasTicketType(performanceID uuid.UUID) bool {
	for _, tt := range f.ticketTypes {
		if tt.PerformanceID == performanceID {
			return true
		}
	}
	return false
}

func (f *fakeStore) MarkPerformanceEventEmitted(_ context.Context, id uuid.UUID) error {
	f.emitted[id] = true
	return nil
}

func (f *fakeStore) MarkPerformanceArchiveEmitted(_ context.Context, id uuid.UUID) error {
	f.archiveEmitted[id] = true
	return nil
}

func (f *fakeStore) CloseSlot(_ context.Context, id uuid.UUID, reason *string) (store.Performance, bool, error) {
	return f.toggleClosure(id, "closed", reason)
}

func (f *fakeStore) ReopenSlot(_ context.Context, id uuid.UUID) (store.Performance, bool, error) {
	return f.toggleClosure(id, "open", nil)
}

func (f *fakeStore) toggleClosure(id uuid.UUID, target string, reason *string) (store.Performance, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, store.ErrNotFound
	}
	if p.Status != "published" {
		return store.Performance{}, false, store.ErrIllegalTransition
	}
	if p.Closure.Status == target {
		return p, f.closureEmitted[id] < p.Closure.Version, nil
	}
	if f.closureEmitted[id] < p.Closure.Version {
		return store.Performance{}, false, store.ErrClosurePending
	}
	now := time.Now().UTC()
	p.Closure.Version++
	if target == "closed" {
		p.Closure.Status = "closed"
		p.Closure.ClosedAt = &now
		p.Closure.Reason = reason
	} else {
		p.Closure.Status = "open"
		p.Closure.ClosedAt = nil
		p.Closure.Reason = nil
	}
	f.performances[id] = p
	return p, true, nil
}

func (f *fakeStore) MarkClosureEmitted(_ context.Context, id uuid.UUID, version int32) error {
	if version > f.closureEmitted[id] {
		f.closureEmitted[id] = version
	}
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
	published       []store.Performance
	archived        []store.Performance
	closed          []store.Performance
	reopened        []store.Performance
	calls           []string // ordered emission log: "published"|"archived"|"closed"|"reopened"
	failNext        bool
	failArchiveNext bool
	failClosureNext bool
}

func (f *fakePublisher) SlotClosed(_ context.Context, p store.Performance) error {
	if f.failClosureNext {
		f.failClosureNext = false
		return errors.New("nats down")
	}
	f.closed = append(f.closed, p)
	f.calls = append(f.calls, "closed")
	return nil
}

func (f *fakePublisher) SlotReopened(_ context.Context, p store.Performance) error {
	if f.failClosureNext {
		f.failClosureNext = false
		return errors.New("nats down")
	}
	f.reopened = append(f.reopened, p)
	f.calls = append(f.calls, "reopened")
	return nil
}

func (f *fakePublisher) PerformanceArchived(_ context.Context, p store.Performance) error {
	if f.failArchiveNext {
		f.failArchiveNext = false
		return errors.New("nats down")
	}
	f.archived = append(f.archived, p)
	f.calls = append(f.calls, "archived")
	return nil
}

func (f *fakePublisher) PerformancePublished(_ context.Context, p store.Performance) error {
	if f.failNext {
		f.failNext = false
		return errors.New("nats down")
	}
	f.published = append(f.published, p)
	f.calls = append(f.calls, "published")
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
	h, err := NewRouter(NewServer(st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-internal-token"))
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

func TestInternalTicketTypeRequiresCredential(t *testing.T) {
	st := newFakeStore()
	pub := &fakePublisher{}
	h, err := NewRouter(NewServer(st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), "secret"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", token: "wrong", want: http.StatusUnauthorized},
		{name: "valid", token: "secret", want: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/ticket-types/"+id.String(), nil)
			req.Header.Set("X-Internal-Token", tt.token)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d", res.Code, tt.want)
			}
		})
	}
}

func TestInternalPublishedPerformanceLookup(t *testing.T) {
	e := newEnv(t)
	_, performanceID := e.createFixture(true)
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{name: "missing credential", want: http.StatusUnauthorized},
		{name: "valid credential", token: "test-internal-token", want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/performances/"+performanceID.String(), nil)
			req.Header.Set("X-Internal-Token", tt.token)
			res := httptest.NewRecorder()
			e.handler.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d", res.Code, tt.want)
			}
			if tt.want == http.StatusOK && !bytes.Contains(res.Body.Bytes(), []byte(`"capacity":500`)) {
				t.Fatalf("lookup response %s", res.Body.String())
			}
		})
	}
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
	startsAt := time.Date(2026, 9, 18, 19, 30, 0, 0, time.UTC)
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris",
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

	startsAt := time.Now().UTC()
	base := PerformanceCreate{OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris"}

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

func TestArchiveEmitsOnceAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Status != Archived || p.ArchivedAt == nil {
		t.Fatalf("archived response = %+v", p)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("expected 1 archive emission, got %d", len(e.pub.archived))
	}
	if rec = e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("re-archive: %d", rec.Code)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("re-archive must not re-emit, got %d", len(e.pub.archived))
	}
}

func TestArchiveRejectsDraftUnknownAndRepublish(t *testing.T) {
	e := newEnv(t)
	_, draftID := e.createFixture(false)
	if rec := e.do("POST", "/performances/"+draftID.String()+"/archive", nil); rec.Code != http.StatusConflict {
		t.Fatalf("archive draft: want 409, got %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+uuid.NewString()+"/archive", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("archive unknown: want 404, got %d", rec.Code)
	}
	_, publishedID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+publishedID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive published: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+publishedID.String()+"/publish", nil); rec.Code != http.StatusConflict {
		t.Fatalf("republish archived: want 409, got %d", rec.Code)
	}
}

func TestArchiveRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failArchiveNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed archive emission: want 500, got %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive retry: %d", rec.Code)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("retry should emit archive once, got %d", len(e.pub.archived))
	}
}

func TestArchiveEmitsOwedPublishBeforeArchive(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	e.pub.failNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed publish emission: want 500, got %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.published) != 1 || len(e.pub.archived) != 1 {
		t.Fatalf("owed events: published=%d archived=%d", len(e.pub.published), len(e.pub.archived))
	}
	// The owed publication must be emitted BEFORE the archive event — asserting
	// on the ordered call log, not just slice lengths, so a reversal is caught.
	if want := []string{"published", "archived"}; !slices.Equal(e.pub.calls, want) {
		t.Fatalf("emission order = %v, want %v", e.pub.calls, want)
	}
}

// TestArchiveRetryReplaysOwedPublishBeforeArchive covers the interleaving where
// the owed publication emits but the archive emission then fails: the retry
// must replay the still-owed publication (same deterministic id) again before
// the archive, never emitting the archive first.
func TestArchiveRetryReplaysOwedPublishBeforeArchive(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	// Publish with a failed emission so the publication stays owed.
	e.pub.failNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed publish emission: want 500, got %d", rec.Code)
	}
	// First archive: the owed publication emits, then the archive emission fails.
	e.pub.failArchiveNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed archive emission: want 500, got %d", rec.Code)
	}
	// Retry archive: the archive emission failed before the publish marker was
	// written, so the still-owed publication is replayed (safe: its deterministic
	// id de-duplicates at the stream) and only then is the archive emitted. The
	// contract is at-least-once emission, NOT invocation-exactly-once — so the
	// invariant is ordering, not call count: no archive event is ever emitted
	// before its owed publication.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive retry: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.archived) < 1 {
		t.Fatalf("archive event never emitted: calls=%v", e.pub.calls)
	}
	// The final emission is the archive; every archive in the log is preceded by
	// at least one publication (publication is always emitted first).
	if got := e.pub.calls[len(e.pub.calls)-1]; got != "archived" {
		t.Fatalf("last emission = %q, want archived; calls=%v", got, e.pub.calls)
	}
	seenPublished := false
	for _, c := range e.pub.calls {
		switch c {
		case "published":
			seenPublished = true
		case "archived":
			if !seenPublished {
				t.Fatalf("archive emitted before any publication: calls=%v", e.pub.calls)
			}
		}
	}
}

func TestArchivedPerformanceExcludedFromPublicReads(t *testing.T) {
	e := newEnv(t)
	eventID, perfID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive: %d", rec.Code)
	}
	list := decode[PublicEventList](t, e.do("GET", "/public/events?locale=en", nil))
	if len(list.Events) != 0 {
		t.Fatalf("archived performance remains listed: %+v", list.Events)
	}
	if rec := e.do("GET", "/public/events/"+eventID.String()+"?locale=en", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("all-archived detail: want 404, got %d", rec.Code)
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

func TestPublicListExcludesDraftsAndPublishRequiresPrice(t *testing.T) {
	e := newEnv(t)
	e.createFixture(false) // draft: not listed

	// Publishing without a ticket type is refused (409): the publication
	// event and public visibility must never disagree.
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Halle B", GaCapacity: 50}))
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Brouillon", "en": "Draft"},
	}))
	startsAt := time.Now().UTC()
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris",
	}))
	if rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil); rec.Code != http.StatusConflict {
		t.Fatalf("unpriced publish: want 409, got %d", rec.Code)
	}
	if len(e.pub.published) != 0 {
		t.Fatalf("refused publish must not emit, got %d emissions", len(e.pub.published))
	}

	list := decode[PublicEventList](t, e.do("GET", "/public/events?locale=en", nil))
	if len(list.Events) != 0 {
		t.Fatalf("draft performances must not be listed, got %d events", len(list.Events))
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

// --- US-009: typed dated slot (kinds, attributes, closure) ---

// dayEnv creates a venue + event and returns their ids for slot-kind tests.
func (e *env) dayEnv() (venueID, eventID uuid.UUID) {
	e.t.Helper()
	v := decode[Venue](e.t, e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "La Ronde", GaCapacity: 800}))
	ev := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Journée parc", "en": "Park day"},
	}))
	return v.Id, ev.Id
}

func TestCreateOperatingDaySlot(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	kind := SlotKind("operating_day")
	opens, closes := "10:00", "02:00" // spans midnight
	opDate := openapi_types.Date{Time: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}
	max := int32(3)
	rec := e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: venueID, Kind: &kind,
		OperatingDate: &opDate, OpensAt: &opens, ClosesAt: &closes, Timezone: "America/Toronto",
		ReEntry: &ReEntryPolicy{Mode: "count_limited", MaxEntries: &max, RequiresExit: true},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create operating_day: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Kind != "operating_day" || p.StartsAt != nil || p.OperatingDate == nil ||
		p.OpensAt == nil || *p.OpensAt != "10:00" || *p.ClosesAt != "02:00" {
		t.Fatalf("operating_day attributes not persisted: %+v", p)
	}
	if p.ReEntry.Mode != "count_limited" || p.ReEntry.MaxEntries == nil || *p.ReEntry.MaxEntries != 3 || !p.ReEntry.RequiresExit {
		t.Fatalf("re_entry not persisted: %+v", p.ReEntry)
	}
	if p.Closure.Status != "open" {
		t.Fatalf("new slot must be open, got %q", p.Closure.Status)
	}
}

func TestCreateSlotKindValidations(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	base := func() PerformanceCreate {
		return PerformanceCreate{OrganizerId: orgID, EventId: eventID, VenueId: venueID, Timezone: "America/Toronto"}
	}
	opens, closes := "09:00", "17:00"
	opDate := openapi_types.Date{Time: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}
	instant := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	festival := SlotKind("festival_day")
	perfKind := SlotKind("performance")
	max := int32(2)

	cases := []struct {
		name string
		body PerformanceCreate
	}{
		{"performance without starts_at", func() PerformanceCreate { b := base(); b.Kind = &perfKind; return b }()},
		{"performance with operating window", func() PerformanceCreate {
			b := base()
			b.Kind = &perfKind
			b.StartsAt = &instant
			b.OpensAt = &opens
			return b
		}()},
		{"day kind with starts_at", func() PerformanceCreate {
			b := base()
			b.Kind = &festival
			b.StartsAt = &instant
			b.OperatingDate = &opDate
			b.OpensAt = &opens
			b.ClosesAt = &closes
			return b
		}()},
		{"day kind missing closes_at", func() PerformanceCreate {
			b := base()
			b.Kind = &festival
			b.OperatingDate = &opDate
			b.OpensAt = &opens
			return b
		}()},
		{"count_limited without max", func() PerformanceCreate {
			b := base()
			b.StartsAt = &instant
			b.ReEntry = &ReEntryPolicy{Mode: "count_limited"}
			return b
		}()},
		{"max on non-count_limited", func() PerformanceCreate {
			b := base()
			b.StartsAt = &instant
			b.ReEntry = &ReEntryPolicy{Mode: "single", MaxEntries: &max}
			return b
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := e.do("POST", "/performances", c.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublishEmitsSlotKind(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	kind := SlotKind("festival_day")
	opens, closes := "12:00", "23:00"
	opDate := openapi_types.Date{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	perf := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: venueID, Kind: &kind,
		OperatingDate: &opDate, OpensAt: &opens, ClosesAt: &closes, Timezone: "Europe/Paris",
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: perf.Id,
		Name: LocalizedString{"fr": "Pass jour", "en": "Day pass"}, Price: Money{Amount: 9000, Currency: "EUR"},
	})
	if rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.published) != 1 || e.pub.published[0].Kind != "festival_day" {
		t.Fatalf("publication event must carry the slot kind, got %+v", e.pub.published)
	}
}

func TestClosureToggleLifecycle(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true) // published performance
	reason := "storm"

	// close
	rec := e.do("POST", "/performances/"+perfID.String()+"/close", SlotCloseRequest{Reason: &reason})
	if rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Closure.Status != "closed" || p.Closure.Reason == nil || *p.Closure.Reason != "storm" || p.Closure.ClosedAt == nil {
		t.Fatalf("closure not applied: %+v", p.Closure)
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("close must emit once, got %d", len(e.pub.closed))
	}

	// idempotent re-close: no new emission
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("re-close: %d", rec.Code)
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("idempotent re-close must not re-emit, got %d", len(e.pub.closed))
	}

	// reopen
	rec = e.do("POST", "/performances/"+perfID.String()+"/reopen", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reopen: %d %s", rec.Code, rec.Body.String())
	}
	p = decode[Performance](t, rec)
	if p.Closure.Status != "open" || p.Closure.ClosedAt != nil {
		t.Fatalf("reopen not applied: %+v", p.Closure)
	}
	if len(e.pub.reopened) != 1 {
		t.Fatalf("reopen must emit once, got %d", len(e.pub.reopened))
	}
}

// Archive stays legal from a closed slot (spike §Case 3): closure is orthogonal
// to the lifecycle, so a closed day can be archived without first reopening.
func TestArchiveLegalFromClosed(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("close: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive from closed must be legal, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestClosureRejectsUnpublished(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false) // draft
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusConflict {
		t.Fatalf("closing a draft must be 409, got %d %s", rec.Code, rec.Body.String())
	}
}

// Archiving must not strand an owed closure event: while the closed event is
// unemitted, archive is refused (409) so the toggle can still re-emit it. Once
// emitted, archive-from-closed proceeds (spike §Case 3).
func TestArchiveRefusedWhileClosureOwed(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failClosureNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("close emission failure: %d", rec.Code)
	}
	// closure event owed (emitted=0 < version=1): archive must refuse.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusConflict {
		t.Fatalf("archive while closure owed must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	// re-emit the owed closure, then archive succeeds.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("retry close: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive after closure emitted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCloseRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failClosureNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("emission failure must surface as 500, got %d", rec.Code)
	}
	if len(e.pub.closed) != 0 {
		t.Fatalf("failed emission must not record, got %d", len(e.pub.closed))
	}
	// retry re-emits the still-owed transition (same deterministic id at the stream)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("retry close: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("retry must emit the owed closure, got %d", len(e.pub.closed))
	}
}
