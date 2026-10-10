package consumer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// Disposition tests for the offering subjects (TKT-75), same discipline as the
// publication tests: every fixture is a handwritten JSON literal, never built from the
// Go struct under test — a fixture built from the type cannot fail (ADR-017).

const (
	perfID = "11111111-1111-4111-8111-111111111111"
	orgID  = "22222222-2222-4222-8222-222222222222"
	grpID  = "33333333-3333-4333-8333-333333333333"
	evtID  = `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
)

type closureCall struct {
	pool    uuid.UUID
	perf    uuid.UUID
	closed  bool
	version int32
}

type quarantineCall struct {
	subject  string
	eventID  uuid.UUID
	schema   int
	envelope []byte
}

// mootCall is one RecordMootSlot write (TKT-317 D1). The source is part of the call, because the
// consumer decides it: only a publication record authorises the fallback (D7).
type mootCall struct {
	eventID, organizer, slot uuid.UUID
	source                   store.MootSource
}

func (c mootCall) String() string {
	return fmt.Sprintf("{event %s organizer %s slot %s source %s}", c.eventID, c.organizer, c.slot, c.source)
}

// fallbackCall is one ConsumeMootOffering question, with every argument the consumer passed.
type fallbackCall struct {
	eventID, organizer, slot, pool uuid.UUID
}

func (c fallbackCall) String() string {
	return fmt.Sprintf("{event %s organizer %s slot %s pool %s}", c.eventID, c.organizer, c.slot, c.pool)
}

// fakeCatalogStore records mutations; err is returned by every mutation.
// quarantineErr is separate: quarantining a future variant must be testable
// independently of the known-variant apply paths. The moot and parked fields follow the
// same rule: each has its own error and its own recorder (TKT-317).
type fakeCatalogStore struct {
	archived         []uuid.UUID
	archiveEventIDs  []uuid.UUID
	closures         []closureCall
	closureEventIDs  []uuid.UUID
	provisioned      []uuid.UUID
	seatProvisioned  []uuid.UUID
	seatMapIDs       []uuid.UUID
	orphanPrevention []bool
	adjacency        [][]store.SeatAdjacencyRow
	quarantined      []quarantineCall
	pools            []store.PoolOffering
	err              error
	listErr          error
	quarantineErr    error
	pending          bool
	pendingErr       error
	moot             []mootCall
	mootErr          error
	fallbacks        []fallbackCall
	fallbackMatch    bool
	fallbackErr      error
	parked           []store.ParkedCatalogEvent
	parkErr          error
}

func (s *fakeCatalogStore) RecordMootSlot(_ context.Context, eventID, organizerID, slotID uuid.UUID, source store.MootSource) error {
	if s.mootErr != nil {
		return s.mootErr
	}
	s.moot = append(s.moot, mootCall{eventID, organizerID, slotID, source})
	return nil
}

// ConsumeMootOffering answers with fallbackMatch: the test decides whether a publication record exists.
func (s *fakeCatalogStore) ConsumeMootOffering(_ context.Context, eventID, organizerID, slotID, poolID uuid.UUID) (bool, error) {
	s.fallbacks = append(s.fallbacks, fallbackCall{eventID, organizerID, slotID, poolID})
	return s.fallbackMatch, s.fallbackErr
}

// ParkCatalogEvent mirrors the store: a duplicate with the same bytes is a no-op, and different
// bytes are a collision that leaves the first copy in place.
func (s *fakeCatalogStore) ParkCatalogEvent(_ context.Context, e store.ParkedCatalogEvent) (bool, error) {
	if s.parkErr != nil {
		return false, s.parkErr
	}
	for _, p := range s.parked {
		if p.Subject == e.Subject && p.EventID == e.EventID {
			if !bytes.Equal(p.Envelope, e.Envelope) {
				return false, store.ErrCatalogParkedCollision
			}
			return false, nil
		}
	}
	s.parked = append(s.parked, e)
	return true, nil
}

func (s *fakeCatalogStore) ListPublishedPoolOfferings(context.Context) ([]store.PoolOffering, error) {
	return s.pools, s.listErr
}

func (s *fakeCatalogStore) Provision(_ context.Context, eventID, _, _ uuid.UUID, _ int32) error {
	if s.err != nil {
		return s.err
	}
	s.provisioned = append(s.provisioned, eventID)
	return nil
}

func (s *fakeCatalogStore) ProvisionSeated(_ context.Context, eventID, _, _, seatMapID uuid.UUID, _ int32, orphanPrevention bool, adjacency []store.SeatAdjacencyRow) error {
	if s.err != nil {
		return s.err
	}
	s.seatProvisioned = append(s.seatProvisioned, eventID)
	s.seatMapIDs = append(s.seatMapIDs, seatMapID)
	s.orphanPrevention = append(s.orphanPrevention, orphanPrevention)
	s.adjacency = append(s.adjacency, adjacency)
	return nil
}

func (s *fakeCatalogStore) QuarantineCatalogEvent(_ context.Context, subject string, eventID uuid.UUID, schema int, envelope []byte) error {
	if s.quarantineErr != nil {
		return s.quarantineErr
	}
	s.quarantined = append(s.quarantined, quarantineCall{subject, eventID, schema, envelope})
	return nil
}

func (s *fakeCatalogStore) HasPendingCatalogQuarantine(context.Context) (bool, error) {
	return s.pending, s.pendingErr
}
func (s *fakeCatalogStore) ApplyArchive(_ context.Context, eventID, pool uuid.UUID) error {
	if s.err != nil {
		return s.err
	}
	s.archived = append(s.archived, pool)
	s.archiveEventIDs = append(s.archiveEventIDs, eventID)
	return nil
}
func (s *fakeCatalogStore) ApplyClosure(_ context.Context, eventID uuid.UUID, pool, perf uuid.UUID, closed bool, version int32) error {
	if s.err != nil {
		return s.err
	}
	s.closures = append(s.closures, closureCall{pool, perf, closed, version})
	s.closureEventIDs = append(s.closureEventIDs, eventID)
	return nil
}

func offeringConsumer(st catalogStore, r PerformanceResolver) *Consumer {
	c := &Consumer{st: st, resolver: r, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.ready.Store(true)
	return c
}

func TestArchivedEventDispositions(t *testing.T) {
	solo := `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `","event_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`
	grouped := `{` + evtID + `,"schema":3,"data":{"performance_id":"` + perfID + `","event_id":"` + perfID + `","organizer_id":"` + orgID + `","capacity_group_id":"` + grpID + `"}}`

	for _, tt := range []struct {
		name            string
		body            string
		storeErr        error
		want            string // final disposition action
		wantsReady      bool
		wantPool        string // non-empty: ApplyArchive must have been called with this pool
		wantQuarantined bool
	}{
		{"solo archive applies to the slot pool", solo, nil, "ack", true, perfID, false},
		{"grouped archive applies to the festival pool", grouped, nil, "ack", true, grpID, false},
		{"future schema is quarantined, acked, and latches unready", `{` + evtID + `,"schema":4,"data":{"slot_ref":"a"}}`, nil, "ack", false, "", true},
		{"schema zero is poison", `{` + evtID + `,"schema":0,"data":{}}`, nil, "term", true, "", false},
		{"future schema without an id is poison, not skew", `{"schema":9,"data":{"slot_ref":"a"}}`, nil, "term", true, "", false},
		{"schema below the first archived variant is poison", `{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`, nil, "term", true, "", false},
		{"missing identifiers are poison", `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `"}}`, nil, "term", true, "", false},
		{"schema 2 carrying a group is poison", `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","capacity_group_id":"` + grpID + `"}}`, nil, "term", true, "", false},
		{"schema 3 without a group is poison", `{` + evtID + `,"schema":3,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`, nil, "term", true, "", false},
		{"unreadable known data is poison", `{` + evtID + `,"schema":2,"data":{"performance_id":42}}`, nil, "term", true, "", false},
		{"missing pool parks for redelivery", solo, store.ErrNotFound, "nak-delay", true, "", false},
		{"store failure retries", solo, errors.New("db down"), "nak", true, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeCatalogStore{err: tt.storeErr}
			c := offeringConsumer(st, nil)
			msg := &fakeMsg{subject: subjectArchived, data: []byte(withSubjectType(subjectArchived, tt.body))}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, tt.want) {
				t.Fatalf("actions = %v, want %s", msg.actions, tt.want)
			}
			if c.Ready() != tt.wantsReady {
				t.Fatalf("ready = %v, want %v", c.Ready(), tt.wantsReady)
			}
			if tt.wantPool != "" && (len(st.archived) != 1 || st.archived[0] != uuid.MustParse(tt.wantPool)) {
				t.Fatalf("archived pools = %v, want exactly [%s]", st.archived, tt.wantPool)
			}
			if got := len(st.quarantined); got != map[bool]int{true: 1}[tt.wantQuarantined] {
				t.Fatalf("quarantined %d events, wantQuarantined=%v — only a valid future variant may reach quarantine", got, tt.wantQuarantined)
			}
			if tt.want == "term" || tt.want == "nak-delay" || tt.wantQuarantined {
				if len(st.archived) != 0 {
					t.Fatalf("archived pools = %v — a quarantined, parked or poisoned event must not mutate", st.archived)
				}
			}
		})
	}
}

func TestClosureEventDispositions(t *testing.T) {
	org := uuid.MustParse(orgID)
	grp := uuid.MustParse(grpID)
	closedV1 := `{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","event_id":"` + perfID + `","organizer_id":"` + orgID + `","kind":"performance","closure_version":1}}`

	for _, tt := range []struct {
		name       string
		subject    string
		body       string
		resolver   PerformanceResolver
		storeErr   error
		want       string
		wantsReady bool
		wantCall   *closureCall
	}{
		{"closed applies at the slot pool", subjectClosed, closedV1,
			fakeResolver{organizerID: org, capacity: 10}, nil, "ack", true, &closureCall{uuid.MustParse(perfID), uuid.MustParse(perfID), true, 1}},
		{"reopened applies at the slot pool", subjectReopened,
			`{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":2}}`,
			fakeResolver{organizerID: org, capacity: 10}, nil, "ack", true, &closureCall{uuid.MustParse(perfID), uuid.MustParse(perfID), false, 2}},
		{"grouped day converges on the festival pool", subjectClosed, closedV1,
			fakeResolver{organizerID: org, capacityGroupID: &grp, sharedCapacity: ptr(int32(100))}, nil, "ack", true, &closureCall{grp, uuid.MustParse(perfID), true, 1}},
		{"future schema is quarantined, acked, and latches unready", subjectClosed,
			`{` + evtID + `,"schema":2,"data":{"slot_ref":"a","state":"shut"}}`, nil, nil, "ack", false, nil},
		{"schema zero is poison", subjectClosed, `{` + evtID + `,"schema":0,"data":{}}`, nil, nil, "term", true, nil},
		{"future schema without an id is poison, not skew", subjectClosed,
			`{"schema":7,"data":{"state":"shut"}}`, nil, nil, "term", true, nil},
		{"missing closure version is poison", subjectClosed,
			`{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`, nil, nil, "term", true, nil},
		{"no-longer-published slot is moot, not parked", subjectClosed, closedV1,
			fakeResolver{err: ErrPerformanceNotFound}, nil, "ack", true, nil},
		{"transient resolver failure is retried", subjectClosed, closedV1,
			fakeResolver{err: errors.New("catalog unreachable")}, nil, "nak-delay", true, nil},
		{"organizer conflict with catalog is poison", subjectClosed, closedV1,
			fakeResolver{organizerID: uuid.MustParse(grpID), capacity: 10}, nil, "term", true, nil},
		{"missing pool parks for redelivery", subjectClosed, closedV1,
			fakeResolver{organizerID: org, capacity: 10}, store.ErrNotFound, "nak-delay", true, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeCatalogStore{err: tt.storeErr}
			c := offeringConsumer(st, tt.resolver)
			msg := &fakeMsg{subject: tt.subject, data: []byte(withSubjectType(tt.subject, tt.body))}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, tt.want) {
				t.Fatalf("actions = %v, want %s", msg.actions, tt.want)
			}
			if c.Ready() != tt.wantsReady {
				t.Fatalf("ready = %v, want %v", c.Ready(), tt.wantsReady)
			}
			if tt.wantCall != nil {
				if len(st.closures) != 1 || st.closures[0] != *tt.wantCall {
					t.Fatalf("closures = %v, want exactly [%+v]", st.closures, *tt.wantCall)
				}
			} else if len(st.closures) != 0 {
				t.Fatalf("closures = %v — this disposition must not mutate", st.closures)
			}
			if wantQ := !tt.wantsReady && tt.want == "ack"; (len(st.quarantined) == 1) != wantQ {
				t.Fatalf("quarantined = %d events — exactly the future variant, and only it, is quarantined", len(st.quarantined))
			}
		})
	}
}

// TKT-317 D1: a moot closure records its outcome before its ack. The test observes the store
// at the moment of the ack, so an acked event always has its moot record.
func TestTKT317MootClosureRecordsItsOutcomeBeforeItsAck(t *testing.T) {
	evt := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	for _, subject := range []string{subjectReopened, subjectClosed} {
		t.Run(subject, func(t *testing.T) {
			st := &fakeCatalogStore{}
			c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
			msg := &fakeMsg{subject: subject, data: []byte(withSubjectType(subject, `{`+evtID+`,"schema":1,"data":{"performance_id":"`+perfID+`","organizer_id":"`+orgID+`","closure_version":3}}`))}
			msg.onAck = func() {
				if len(st.moot) != 1 {
					t.Errorf("acked with %d moot records; the record must commit before the ack", len(st.moot))
				}
			}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, "ack") {
				t.Fatalf("actions = %v, want ack", msg.actions)
			}
			if want := (mootCall{evt, uuid.MustParse(orgID), uuid.MustParse(perfID), store.MootSourceClosure}); len(st.moot) != 1 || st.moot[0] != want {
				t.Fatalf("moot records = %v, want exactly [%+v]", st.moot, want)
			}
			if len(st.closures) != 0 {
				t.Fatalf("closures = %v; a moot closure must not mutate the pool", st.closures)
			}
			if !c.Ready() {
				t.Fatal("a moot closure latched readiness")
			}
		})
	}
}

// A failed moot write keeps the event for redelivery. An ack without the record is the defect
// the disposition exists to remove.
func TestTKT317FailedMootWriteRetainsTheClosure(t *testing.T) {
	st := &fakeCatalogStore{mootErr: errors.New("db down")}
	c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	msg := &fakeMsg{subject: subjectClosed, data: []byte(withSubjectType(subjectClosed, `{`+evtID+`,"schema":1,"data":{"performance_id":"`+perfID+`","organizer_id":"`+orgID+`","closure_version":1}}`))}

	c.handle(context.Background(), msg)

	if slices.Contains(msg.actions, "ack") || !slices.Contains(msg.actions, "nak-delay") {
		t.Fatalf("actions = %v, want a delayed retry and no ack", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("a failed moot write latched readiness")
	}
}

// TKT-317 D1: an archive that finds no pool is consumed only when the fallback answers yes. The
// fallback is asked with the archive's own event, organizer, slot and pool. For a grouped archive
// the slot is the member's performance and the pool is its capacity group.
func TestTKT317ArchiveWithoutPoolFallsBackOnlyOnAPublicationRecord(t *testing.T) {
	evt := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	org, perf, grp := uuid.MustParse(orgID), uuid.MustParse(perfID), uuid.MustParse(grpID)
	solo := `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`
	grouped := `{` + evtID + `,"schema":3,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","capacity_group_id":"` + grpID + `"}}`

	for _, tc := range []struct {
		name  string
		body  string
		match bool
		err   error
		pool  uuid.UUID
		want  string
	}{
		{"solo with no publication record waits", solo, false, nil, perf, "nak-delay"},
		{"solo with a publication record is consumed", solo, true, nil, perf, "ack"},
		{"grouped member asks with its capacity group", grouped, true, nil, grp, "ack"},
		{"a fallback store failure retries", solo, false, errors.New("db down"), perf, "nak-delay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCatalogStore{err: store.ErrNotFound, fallbackMatch: tc.match, fallbackErr: tc.err}
			c := offeringConsumer(st, nil)
			msg := &fakeMsg{subject: subjectArchived, data: []byte(withSubjectType(subjectArchived, tc.body))}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, tc.want) {
				t.Fatalf("actions = %v, want %s", msg.actions, tc.want)
			}
			if want := (fallbackCall{evt, org, perf, tc.pool}); len(st.fallbacks) != 1 || st.fallbacks[0] != want {
				t.Fatalf("fallback asked %v, want exactly [%+v]", st.fallbacks, want)
			}
			if len(st.archived) != 0 {
				t.Fatalf("archived = %v; the normal apply found no pool", st.archived)
			}
		})
	}
}

// TKT-317 D1: a closure whose catalog answer resolves finds no pool. The consumer asks the fallback
// with the closure's own ids, and the store answers. A publication record for the slot is what lets
// it through (D7). Catalog does not produce this state today (ADR-077), and COS2 requires the
// branch, so this pins the branch and not a reachable production state.
func TestTKT317ClosureWithoutPoolFallsBackOnlyOnAPublicationRecord(t *testing.T) {
	evt := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	org, perf := uuid.MustParse(orgID), uuid.MustParse(perfID)
	body := `{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":1}}`

	for _, tc := range []struct {
		name  string
		match bool
		want  string
	}{
		{"no publication record waits", false, "nak-delay"},
		{"a publication record is consumed", true, "ack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCatalogStore{err: store.ErrNotFound, fallbackMatch: tc.match}
			c := offeringConsumer(st, fakeResolver{organizerID: org, capacity: 10})
			msg := &fakeMsg{subject: subjectClosed, data: []byte(withSubjectType(subjectClosed, body))}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, tc.want) {
				t.Fatalf("actions = %v, want %s", msg.actions, tc.want)
			}
			if want := (fallbackCall{evt, org, perf, perf}); len(st.fallbacks) != 1 || st.fallbacks[0] != want {
				t.Fatalf("fallback asked %v, want exactly [%+v]", st.fallbacks, want)
			}
			if len(st.closures) != 0 {
				t.Fatalf("closures = %v; the normal apply found no pool", st.closures)
			}
		})
	}
}

// TKT-317 D1: a moot publication records its outcome before its ack, with the publication's own
// event, organizer and slot. A failed write retains the message.
func TestTKT317MootPublicationIsRecordedBeforeItsAck(t *testing.T) {
	const pubUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	body := `{"id":"` + pubUUID + `","schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`

	st := &fakeCatalogStore{}
	c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	msg := &fakeMsg{data: []byte(withSubjectType(subjectPublished, body))}
	msg.onAck = func() {
		if len(st.moot) != 1 {
			t.Errorf("acked with %d moot records; the record must commit before the ack", len(st.moot))
		}
	}

	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "ack") {
		t.Fatalf("actions = %v, want ack", msg.actions)
	}
	if want := (mootCall{uuid.MustParse(pubUUID), uuid.MustParse(orgID), uuid.MustParse(perfID), store.MootSourcePublication}); len(st.moot) != 1 || st.moot[0] != want {
		t.Fatalf("moot records = %v, want exactly [%+v]", st.moot, want)
	}

	st = &fakeCatalogStore{mootErr: errors.New("db down")}
	c = offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	msg = &fakeMsg{data: []byte(withSubjectType(subjectPublished, body))}
	c.handle(context.Background(), msg)
	if slices.Contains(msg.actions, "ack") || !slices.Contains(msg.actions, "nak-delay") {
		t.Fatalf("failed moot write: actions = %v, want a delayed retry and no ack", msg.actions)
	}
}

// Tripwire: the per-subject schema registry and the publication const are two statements
// of one fact and must not drift.
func TestKnownSchemasRegistryMatchesThePublicationConst(t *testing.T) {
	if knownSchemas[subjectPublished].max != maxKnownPublicationSchema {
		t.Fatalf("knownSchemas[published].max = %d, maxKnownPublicationSchema = %d",
			knownSchemas[subjectPublished].max, maxKnownPublicationSchema)
	}
}

func ptr[T any](v T) *T { return &v }
