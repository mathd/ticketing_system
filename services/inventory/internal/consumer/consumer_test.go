package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
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
	data    []byte
	actions []string
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}
func (m *fakeMsg) Data() []byte                    { return m.data }
func (m *fakeMsg) Headers() nats.Header            { return nil }
func (m *fakeMsg) Subject() string                 { return subject }
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

func publicationJSON(t *testing.T, schema int) []byte {
	t.Helper()
	e := publication{ID: uuid.New(), Schema: schema}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

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

// This asserts classification only. It passed before TKT-61 and the event was still dropped:
// the error was never the bug, the disposition was. TestUnknownSchemaVersionSkewIsParked is the
// one that fails if Term() comes back — keep the two separate.
func TestProvisionInputRejectsUnsupportedSchema(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 4}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}
	_, err := c.provisionInput(context.Background(), e)
	if !errors.Is(err, errUnsupportedPublicationSchema) {
		t.Fatalf("err = %v, want it to wrap errUnsupportedPublicationSchema", err)
	}
}

// The version-skew regression (TKT-61, ADR-017 §5b): an inventory binary meets a retained variant
// it does not know — by rollback, by durable recreation, or merely by restarting across a window
// where catalog emitted it. The event is well-formed and a newer binary can provision it, so it
// must be parked for that binary. Term() would advance the durable consumer past it for good.
func TestUnknownSchemaVersionSkewIsParked(t *testing.T) {
	for _, schema := range []int{4, 99} {
		e := publication{ID: uuid.New(), Schema: schema}
		e.Data.PerformanceID = uuid.New()
		e.Data.OrganizerID = uuid.New()
		e.Data.Capacity = 250
		c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}

		_, err := c.provisionInput(context.Background(), e)
		if !errors.Is(err, errUnsupportedPublicationSchema) {
			t.Fatalf("schema %d: err = %v, want it to wrap errUnsupportedPublicationSchema", schema, err)
		}
		if got := dispositionForProvisionError(schema, err); got != dispositionPark {
			t.Fatalf("schema %d: disposition = %q, want %q — Term() drops the event with no retry", schema, got, dispositionPark)
		}
	}
}

// The poison/skew line has to hold at the bottom end too, or it isn't a line. Schema numbers start
// at 1 and climb, so <= 0 is never a future variant — it is an envelope missing `schema`
// (ADR-009 §5) and no binary will ever provision it. Parking it would wait forever for a version
// that cannot exist, and hand any buggy producer a way to latch inventory unready.
func TestEnvelopeWithoutUsableSchemaIsTerminated(t *testing.T) {
	for _, schema := range []int{0, -1} {
		e := publication{ID: uuid.New(), Schema: schema}
		e.Data.PerformanceID = uuid.New()
		e.Data.OrganizerID = uuid.New()
		e.Data.Capacity = 250

		_, err := (&Consumer{}).provisionInput(context.Background(), e)
		if err == nil {
			t.Fatalf("schema %d: expected rejection", schema)
		}
		if errors.Is(err, errUnsupportedPublicationSchema) {
			t.Fatalf("schema %d: must not be classified as version skew — it is a malformed envelope", schema)
		}
		if got := dispositionForProvisionError(schema, err); got != dispositionTerminate {
			t.Fatalf("schema %d: disposition = %q, want %q", schema, got, dispositionTerminate)
		}
	}
}

// The other side of the rule: a payload that is bad at a schema this binary *does* know is poison —
// no binary can provision it — so it still terminates. This is what stops parking from wedging the
// consumer, and it keeps ADR-017 §4's negative validation meaningful.
func TestInvalidKnownSchemaIsTerminated(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity

	_, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err == nil {
		t.Fatal("expected schema-2 festival fields to be rejected")
	}
	if got := dispositionForProvisionError(e.Schema, err); got != dispositionTerminate {
		t.Fatalf("disposition = %q, want %q — a corrupt known variant must not retry forever", got, dispositionTerminate)
	}
}

// Schema 1 resolves against catalog, so its failures are transient by nature. Unchanged by TKT-61;
// asserted because the call site that decided it was reshaped.
func TestSchema1ResolutionFailureIsRetried(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("catalog unreachable")}}

	_, err := c.provisionInput(context.Background(), e)
	if err == nil {
		t.Fatal("expected resolver error")
	}
	if got := dispositionForProvisionError(e.Schema, err); got != dispositionRetry {
		t.Fatalf("disposition = %q, want %q", got, dispositionRetry)
	}
}

// The finding this closes: the pure-function tests above cannot fail if handle stops obeying the
// helper. This one drives the shipped path — unknown schema must be NAK'd, must never be Term'd,
// and must latch readiness false so the skew is visible.
func TestHandleParksUnknownSchemaAndLatchesUnready(t *testing.T) {
	c := testConsumer()
	msg := &fakeMsg{data: publicationJSON(t, 4)}

	c.handle(context.Background(), msg)

	if slices.Contains(msg.actions, "term") {
		t.Fatalf("actions = %v — an unknown variant must never be terminated; a newer binary can provision it", msg.actions)
	}
	if !slices.Contains(msg.actions, "nak-delay") {
		t.Fatalf("actions = %v, want a delayed nak parking the event", msg.actions)
	}
	if c.Ready() {
		t.Fatal("consumer must latch unready on version skew — reporting healthy is what made TKT-61 invisible")
	}
}

// The poison side, as shipped: a malformed envelope terminates and must NOT take readiness down
// with it, or any buggy producer can wedge inventory with one message.
func TestHandleTerminatesEnvelopeWithoutUsableSchemaAndStaysReady(t *testing.T) {
	c := testConsumer()
	msg := &fakeMsg{data: publicationJSON(t, 0)}

	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "term") {
		t.Fatalf("actions = %v, want term — schema 0 is poison, not a future variant", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("a malformed envelope must not latch inventory unready")
	}
}

// Malformed JSON keeps terminating, and likewise must not touch readiness.
func TestHandleTerminatesMalformedJSON(t *testing.T) {
	c := testConsumer()
	msg := &fakeMsg{data: []byte("{not json")}

	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "term") {
		t.Fatalf("actions = %v, want term", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("malformed JSON must not latch inventory unready")
	}
}
