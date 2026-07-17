package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeMsg records what the handler did to the message, mirroring the shape
// services/access/internal/consumer already uses. It is what lets the disposition be asserted as
// shipped rather than in effigy: the pure helper agreeing with the closure is not the same fact as
// the closure obeying it.
type fakeMsg struct {
	subject string
	data    []byte
	actions []string
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}
func (m *fakeMsg) Data() []byte         { return m.data }
func (m *fakeMsg) Headers() nats.Header { return nil }
func (m *fakeMsg) Subject() string {
	if m.subject == "" {
		return subjectPublished
	}
	return m.subject
}
func (m *fakeMsg) Reply() string                   { return "" }
func (m *fakeMsg) Ack() error                      { m.actions = append(m.actions, "ack"); return nil }
func (m *fakeMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakeMsg) Nak() error                      { m.actions = append(m.actions, "nak"); return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error {
	m.actions = append(m.actions, "nak-delay")
	return nil
}
func (m *fakeMsg) InProgress() error           { return nil }
func (m *fakeMsg) Term() error                 { m.actions = append(m.actions, "term"); return nil }
func (m *fakeMsg) TermWithReason(string) error { return m.Term() }

func testConsumer() *Consumer {
	c := &Consumer{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.ready.Store(true)
	return c
}

type fakeResolver struct {
	organizerID     uuid.UUID
	capacity        int32
	capacityGroupID *uuid.UUID
	sharedCapacity  *int32
	err             error
}

func (r fakeResolver) PublishedPerformance(context.Context, uuid.UUID) (PublishedPerformance, error) {
	return PublishedPerformance{
		OrganizerID:     r.organizerID,
		Capacity:        r.capacity,
		CapacityGroupID: r.capacityGroupID,
		SharedCapacity:  r.sharedCapacity,
	}, r.err
}

func TestProvisionInputResolvesSchema1Capacity(t *testing.T) {
	organizerID := uuid.New()
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	c := &Consumer{resolver: fakeResolver{organizerID: organizerID, capacity: 500}}
	got, err := c.provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.organizerID != organizerID || got.poolID != e.Data.PerformanceID || got.capacity != 500 {
		t.Fatalf("resolved (%s, %s, %d), want (%s, %s, 500)", got.organizerID, got.poolID, got.capacity, organizerID, e.Data.PerformanceID)
	}
}

func TestProvisionInputUsesOneSharedFestivalPool(t *testing.T) {
	organizerID, groupID := uuid.New(), uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 3}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity

	got, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != groupID || got.capacity != sharedCapacity {
		t.Fatalf("pool (%s, %d), want shared festival pool (%s, %d)", got.poolID, got.capacity, groupID, sharedCapacity)
	}
}

func TestProvisionInputResolvesLegacyFestivalGroup(t *testing.T) {
	organizerID, groupID := uuid.New(), uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	c := &Consumer{resolver: fakeResolver{
		organizerID: organizerID, capacityGroupID: &groupID, sharedCapacity: &sharedCapacity,
	}}
	got, err := c.provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != groupID || got.capacity != sharedCapacity {
		t.Fatalf("legacy pool (%s, %d), want shared festival pool (%s, %d)", got.poolID, got.capacity, groupID, sharedCapacity)
	}
}

func TestProvisionInputRejectsPartialFestivalCapacity(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	zeroCapacity := int32(0)
	tests := []struct {
		name     string
		groupID  *uuid.UUID
		capacity *int32
	}{
		{name: "both missing"},
		{name: "shared capacity missing", groupID: &groupID},
		{name: "group missing", capacity: &sharedCapacity},
		{name: "zero group", groupID: new(uuid.UUID), capacity: &sharedCapacity},
		{name: "zero shared capacity", groupID: &groupID, capacity: &zeroCapacity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := publication{ID: uuid.New(), Schema: 3}
			e.Data.PerformanceID = uuid.New()
			e.Data.OrganizerID = uuid.New()
			e.Data.CapacityGroupID = tt.groupID
			e.Data.SharedCapacity = tt.capacity
			if _, err := (&Consumer{}).provisionInput(context.Background(), e); err == nil {
				t.Fatal("expected invalid schema-3 festival capacity to be rejected")
			}
		})
	}
}

func TestProvisionInputRejectsFestivalCapacityOnSchema2(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity
	if _, err := (&Consumer{}).provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected schema-2 festival fields to be rejected")
	}
}

func TestProvisionInputUsesPerSlotPoolWithoutFestivalGroup(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250

	got, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != e.Data.PerformanceID || got.capacity != e.Data.Capacity {
		t.Fatalf("pool (%s, %d), want slot pool (%s, %d)", got.poolID, got.capacity, e.Data.PerformanceID, e.Data.Capacity)
	}
}

func TestProvisionInputRejectsSchema1CatalogMismatch(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{organizerID: uuid.New(), capacity: 500}}
	if _, err := c.provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected catalog mismatch")
	}
}

// The version-skew regression (TKT-61, ADR-017 §5b). An inventory binary meets a retained variant
// it does not know — by rollback, by durable recreation, or merely by restarting across a window
// where catalog emitted it.
//
// These payloads are deliberately NOT built from the current Go struct. A schema bump exists
// precisely because `data` changed (ADR-017 §3), so a real future variant reshapes it: renamed
// keys, new required fields, changed types. An earlier version of this fix decoded the payload
// before dispatching on schema and terminated every one of these — passing a test that built
// schema 4 from today's struct and so guaranteed the compatibility it meant to prove.
func TestUnknownSchemaVersionSkewIsParked(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	for _, tt := range []struct{ name, body string }{
		{"reshaped data", `{` + id + `,"schema":4,"data":{"slot_ref":"a","org_ref":"b"}}`},
		{"changed field type", `{` + id + `,"schema":4,"data":{"performance_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":"500"}}`},
		{"empty data", `{` + id + `,"schema":5,"data":{}}`},
		{"data is not an object", `{` + id + `,"schema":9,"data":[1,2,3]}`},
		{"shaped like today", `{` + id + `,"schema":4,"data":{"performance_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":500}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConsumer()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v — a future variant must never be terminated; a newer binary can provision it", msg.actions)
			}
			if !slices.Contains(msg.actions, "nak-delay") {
				t.Fatalf("actions = %v, want a delayed nak parking the event", msg.actions)
			}
			if c.Ready() {
				t.Fatal("consumer must latch unready on version skew — reporting healthy is what made TKT-61 invisible")
			}
		})
	}
}

// The poison side. Schema <= 0 is a broken envelope (ADR-009 §5 requires `schema`), not a variant
// from the future — no binary will ever provision it, so it terminates. Readiness must survive:
// otherwise one malformed message is a free, permanent inventory outage.
func TestEnvelopeWithoutUsableSchemaIsTerminatedAndStaysReady(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	for _, tt := range []struct{ name, body string }{
		{"schema omitted", `{` + id + `,"data":{}}`},
		{"schema zero", `{` + id + `,"schema":0,"data":{}}`},
		{"schema negative", `{` + id + `,"schema":-1,"data":{}}`},
		{"malformed json", `{not json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConsumer()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v, want term — this is poison, not a future variant", msg.actions)
			}
			if !c.Ready() {
				t.Fatal("a broken envelope must not latch inventory unready — that hands any buggy producer an outage")
			}
		})
	}
}

// A payload bad at a schema we DO know is poison: no binary can provision it, so it terminates.
// This is what stops parking from becoming an infinite retry loop for corrupt data, and it keeps
// ADR-017 §4's negative validation meaningful.
func TestInvalidKnownSchemaIsTerminatedAndStaysReady(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	uid := `"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	for _, tt := range []struct{ name, body string }{
		{"schema 2 carrying festival fields", `{` + id + `,"schema":2,"data":{"performance_id":` + uid + `,"organizer_id":` + uid + `,"capacity":250,"capacity_group_id":` + uid + `,"shared_capacity":1000}}`},
		{"schema 3 missing festival capacity", `{` + id + `,"schema":3,"data":{"performance_id":` + uid + `,"organizer_id":` + uid + `,"capacity":250}}`},
		{"known schema, unreadable data", `{` + id + `,"schema":2,"data":{"capacity":"not-a-number"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConsumer()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v, want term — a corrupt known variant must not retry forever", msg.actions)
			}
			if !c.Ready() {
				t.Fatal("a corrupt known variant must not latch inventory unready")
			}
		})
	}
}

// Schema 1 resolves against catalog, so its failures are transient: retry, never terminate, and
// never touch readiness. Unchanged by TKT-61; asserted because the call site was restructured.
func TestSchema1ResolutionFailureIsRetriedAndStaysReady(t *testing.T) {
	uid := `"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	c := testConsumer()
	c.resolver = fakeResolver{err: errors.New("catalog unreachable")}
	msg := &fakeMsg{data: []byte(`{"id":` + uid + `,"schema":1,"data":{"performance_id":` + uid + `,"organizer_id":` + uid + `}}`)}

	c.handle(context.Background(), msg)

	if slices.Contains(msg.actions, "term") {
		t.Fatalf("actions = %v — a transient catalog failure must not be terminal", msg.actions)
	}
	if !slices.Contains(msg.actions, "nak-delay") {
		t.Fatalf("actions = %v, want a delayed nak", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("catalog being down is not version skew — readiness must not latch")
	}
}

// Tripwire, half one: the const must not run AHEAD of the arms. maxKnownPublicationSchema and
// provisionInput's case arms are two statements of the same fact, and drift between them is
// silent. Bump the const without adding an arm and inventory claims to understand a variant it
// cannot read — it stops parking the event and terminates it instead, which is TKT-61 all over
// again. The pair of tests pins both directions.
func TestEveryKnownSchemaHasAnArm(t *testing.T) {
	for s := 1; s <= maxKnownPublicationSchema; s++ {
		e := publication{ID: uuid.New(), Schema: s}
		e.Data.PerformanceID = uuid.New()
		e.Data.OrganizerID = uuid.New()
		c := &Consumer{resolver: fakeResolver{organizerID: e.Data.OrganizerID, capacity: 1}}
		_, err := c.provisionInput(context.Background(), e)
		if err != nil && strings.Contains(err.Error(), "unsupported publication schema") {
			t.Fatalf("schema %d is <= maxKnownPublicationSchema but provisionInput has no arm for it", s)
		}
	}
}

// Tripwire, half two: the const must not lag BEHIND the arms. Add a case arm and forget the const
// and the opposite happens — a variant this binary fully implements is parked as "from the future"
// forever, and inventory sits unready waiting for a binary that is already running.
func TestMaxKnownSchemaIsNotBehindTheArms(t *testing.T) {
	next := maxKnownPublicationSchema + 1
	e := publication{ID: uuid.New(), Schema: next}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	_, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "unsupported publication schema") {
		t.Fatalf("schema %d = maxKnownPublicationSchema+1 must be unsupported, got err = %v — the const is behind the arms", next, err)
	}
}
