package consumer

import (
	"context"
	"errors"
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
	closed  bool
	version int32
}

// fakeCatalogStore records mutations; err is returned by every mutation.
type fakeCatalogStore struct {
	archived []uuid.UUID
	closures []closureCall
	err      error
}

func (s *fakeCatalogStore) Provision(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int32) error {
	return s.err
}
func (s *fakeCatalogStore) ApplyArchive(_ context.Context, _ uuid.UUID, pool uuid.UUID) error {
	if s.err != nil {
		return s.err
	}
	s.archived = append(s.archived, pool)
	return nil
}
func (s *fakeCatalogStore) ApplyClosure(_ context.Context, _ uuid.UUID, pool uuid.UUID, closed bool, version int32) error {
	if s.err != nil {
		return s.err
	}
	s.closures = append(s.closures, closureCall{pool, closed, version})
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
		name       string
		body       string
		storeErr   error
		want       string // final disposition action
		wantsReady bool
		wantPool   string // non-empty: ApplyArchive must have been called with this pool
	}{
		{"solo archive applies to the slot pool", solo, nil, "ack", true, perfID},
		{"grouped archive applies to the festival pool", grouped, nil, "ack", true, grpID},
		{"future schema is parked and latches unready", `{` + evtID + `,"schema":4,"data":{"slot_ref":"a"}}`, nil, "nak-delay", false, ""},
		{"schema zero is poison", `{` + evtID + `,"schema":0,"data":{}}`, nil, "term", true, ""},
		{"schema below the first archived variant is poison", `{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`, nil, "term", true, ""},
		{"missing identifiers are poison", `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `"}}`, nil, "term", true, ""},
		{"schema 2 carrying a group is poison", `{` + evtID + `,"schema":2,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","capacity_group_id":"` + grpID + `"}}`, nil, "term", true, ""},
		{"schema 3 without a group is poison", `{` + evtID + `,"schema":3,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `"}}`, nil, "term", true, ""},
		{"unreadable known data is poison", `{` + evtID + `,"schema":2,"data":{"performance_id":42}}`, nil, "term", true, ""},
		{"missing pool parks for redelivery", solo, store.ErrNotFound, "nak-delay", true, ""},
		{"store failure retries", solo, errors.New("db down"), "nak", true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeCatalogStore{err: tt.storeErr}
			c := offeringConsumer(st, nil)
			msg := &fakeMsg{subject: subjectArchived, data: []byte(tt.body)}

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
			if tt.want == "term" || tt.want == "nak-delay" {
				if len(st.archived) != 0 {
					t.Fatalf("archived pools = %v — a parked or poisoned event must not mutate", st.archived)
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
			fakeResolver{organizerID: org, capacity: 10}, nil, "ack", true, &closureCall{uuid.MustParse(perfID), true, 1}},
		{"reopened applies at the slot pool", subjectReopened,
			`{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":2}}`,
			fakeResolver{organizerID: org, capacity: 10}, nil, "ack", true, &closureCall{uuid.MustParse(perfID), false, 2}},
		{"grouped day converges on the festival pool", subjectClosed, closedV1,
			fakeResolver{organizerID: org, capacityGroupID: &grp, sharedCapacity: ptr(int32(100))}, nil, "ack", true, &closureCall{grp, true, 1}},
		{"future schema is parked and latches unready", subjectClosed,
			`{` + evtID + `,"schema":2,"data":{"slot_ref":"a","state":"shut"}}`, nil, nil, "nak-delay", false, nil},
		{"schema zero is poison", subjectClosed, `{` + evtID + `,"schema":0,"data":{}}`, nil, nil, "term", true, nil},
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
			msg := &fakeMsg{subject: tt.subject, data: []byte(tt.body)}

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
		})
	}
}

// The moot path must never reach the store: the pool may not even exist, and an
// ack that also mutated would make "moot" a lie.
func TestMootClosureDoesNotTouchTheStore(t *testing.T) {
	st := &fakeCatalogStore{}
	c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	msg := &fakeMsg{subject: subjectReopened, data: []byte(`{` + evtID + `,"schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":3}}`)}

	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "ack") || len(st.closures) != 0 {
		t.Fatalf("actions = %v closures = %v, want a pure ack", msg.actions, st.closures)
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
