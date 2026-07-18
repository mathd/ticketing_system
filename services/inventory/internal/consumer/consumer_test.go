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

	"ticketing/services/inventory/internal/store"
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
	c, _ := testConsumerWithStore()
	return c
}

func testConsumerWithStore() (*Consumer, *fakeCatalogStore) {
	st := &fakeCatalogStore{}
	c := &Consumer{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.ready.Store(true)
	return c, st
}

type fakeResolver struct {
	organizerID     uuid.UUID
	capacity        int32
	capacityGroupID *uuid.UUID
	sharedCapacity  *int32
	err             error
}

func (r fakeResolver) PoolOfferState(context.Context, uuid.UUID) (PoolOfferState, error) {
	return PoolOfferState{}, r.err
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
func TestUnknownSchemaVersionSkewIsQuarantinedAndAcked(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	for _, tt := range []struct {
		name, body string
		schema     int
	}{
		{"reshaped data", `{` + id + `,"schema":4,"data":{"slot_ref":"a","org_ref":"b"}}`, 4},
		{"changed field type", `{` + id + `,"schema":4,"data":{"performance_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":"500"}}`, 4},
		{"empty data", `{` + id + `,"schema":5,"data":{}}`, 5},
		{"data is not an object", `{` + id + `,"schema":9,"data":[1,2,3]}`, 9},
		{"shaped like today", `{` + id + `,"schema":4,"data":{"performance_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","organizer_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","capacity":500}}`, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, st := testConsumerWithStore()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v — a future variant must never be terminated; a newer binary can provision it", msg.actions)
			}
			if !slices.Contains(msg.actions, "ack") {
				t.Fatalf("actions = %v, want ack — the quarantined copy frees the ack window (TKT-68)", msg.actions)
			}
			if len(st.quarantined) != 1 {
				t.Fatalf("quarantined = %d events, want exactly 1", len(st.quarantined))
			}
			q := st.quarantined[0]
			if q.subject != subjectPublished || q.eventID != uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") || q.schema != tt.schema {
				t.Fatalf("quarantined (%s, %s, %d), want (%s, 6ba7b810…, %d)", q.subject, q.eventID, q.schema, subjectPublished, tt.schema)
			}
			if string(q.envelope) != tt.body {
				t.Fatalf("quarantined envelope %q, want the exact raw bytes %q — reprocessing republishes them verbatim", q.envelope, tt.body)
			}
			if c.Ready() {
				t.Fatal("consumer must latch unready on version skew — reporting healthy is what made TKT-61 invisible")
			}
		})
	}
}

// The ack must be the consequence of a committed quarantine write, never unconditional. These
// failure cases are what pin the ordering: if handle acked before asking the store, every one of
// them would still show an ack.
func TestQuarantineFailureKeepsTheEventOutstanding(t *testing.T) {
	id := `"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	body := `{` + id + `,"schema":4,"data":{"slot_ref":"a"}}`
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"store failure", errors.New("db down")},
		{"quarantine full", store.ErrCatalogQuarantineFull},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, st := testConsumerWithStore()
			st.quarantineErr = tt.err
			msg := &fakeMsg{data: []byte(body)}

			c.handle(context.Background(), msg)

			if slices.Contains(msg.actions, "ack") || slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v — without a committed quarantine copy the event must stay outstanding", msg.actions)
			}
			if !slices.Contains(msg.actions, "nak-delay") {
				t.Fatalf("actions = %v, want a delayed nak", msg.actions)
			}
			if c.Ready() {
				t.Fatal("readiness must latch false while a future variant cannot be quarantined")
			}
		})
	}
}

// Two different payloads under one (subject, event_id) is a broken producer invariant
// (ADR-009 §5 — the id is the stable part), not skew: the row will never be overwritten, so a
// delayed NAK would re-park it forever — recreating the exact ack-window occupation this ticket
// removes. Poison rules apply: terminate, readiness untouched. The first copy stays quarantined.
func TestQuarantineCollisionIsPoison(t *testing.T) {
	c, st := testConsumerWithStore()
	st.quarantineErr = store.ErrCatalogQuarantineCollision
	msg := &fakeMsg{data: []byte(`{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","schema":4,"data":{"x":1}}`)}

	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "term") || slices.Contains(msg.actions, "ack") {
		t.Fatalf("actions = %v, want term — an id collision is a producer bug no binary will ever apply", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("a producer invariant break must not latch inventory unready — poison rule")
	}
}

// The COS regression: a variant this binary DOES understand keeps flowing while an unknown one is
// held in quarantine — the stall TKT-68 exists to remove.
func TestKnownEventFlowsWhileUnknownIsHeld(t *testing.T) {
	uid := `"6ba7b810-9dad-11d1-80b4-00c04fd430c8"`
	c, st := testConsumerWithStore()

	future := &fakeMsg{data: []byte(`{"id":` + uid + `,"schema":4,"data":{"slot_ref":"a"}}`)}
	c.handle(context.Background(), future)
	if !slices.Contains(future.actions, "ack") || len(st.quarantined) != 1 {
		t.Fatalf("future: actions = %v quarantined = %d, want quarantine + ack", future.actions, len(st.quarantined))
	}

	known := &fakeMsg{data: []byte(`{"id":` + uid + `,"schema":2,"data":{"performance_id":` + uid + `,"organizer_id":` + uid + `,"capacity":500}}`)}
	c.handle(context.Background(), known)
	if !slices.Contains(known.actions, "ack") || len(st.provisioned) != 1 {
		t.Fatalf("known: actions = %v provisioned = %d — a supported variant must still provision and ack", known.actions, len(st.provisioned))
	}
	if c.Ready() {
		t.Fatal("readiness stays latched until quarantine is reprocessed and the binary restarts — recovery on the next good message would hide the held event")
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
			c, st := testConsumerWithStore()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v, want term — this is poison, not a future variant", msg.actions)
			}
			if !c.Ready() {
				t.Fatal("a broken envelope must not latch inventory unready — that hands any buggy producer an outage")
			}
			if len(st.quarantined) != 0 {
				t.Fatalf("quarantined = %v — poison must never reach quarantine", st.quarantined)
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
			c, st := testConsumerWithStore()
			msg := &fakeMsg{data: []byte(tt.body)}

			c.handle(context.Background(), msg)

			if !slices.Contains(msg.actions, "term") {
				t.Fatalf("actions = %v, want term — a corrupt known variant must not retry forever", msg.actions)
			}
			if !c.Ready() {
				t.Fatal("a corrupt known variant must not latch inventory unready")
			}
			if len(st.quarantined) != 0 {
				t.Fatalf("quarantined = %v — a corrupt known variant is poison, not a future variant", st.quarantined)
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
// The durable's backpressure bound must be explicit in code, not inherited from the nats-server
// default of 1000 (TKT-68 COS 2). This fails if the field is dropped and the default sneaks back.
func TestConsumerConfigSetsExplicitMaxAckPending(t *testing.T) {
	cfg := (&Consumer{}).consumerConfig()
	if cfg.Durable != "inventory-catalog-offering" {
		t.Fatalf("durable = %q, want inventory-catalog-offering", cfg.Durable)
	}
	want := []string{subjectPublished, subjectArchived, subjectClosed, subjectReopened}
	if !slices.Equal(cfg.FilterSubjects, want) {
		t.Fatalf("filter subjects = %v, want %v", cfg.FilterSubjects, want)
	}
	if cfg.AckPolicy != jetstream.AckExplicitPolicy || cfg.DeliverPolicy != jetstream.DeliverAllPolicy {
		t.Fatalf("policies = (%v, %v), want (explicit ack, deliver all)", cfg.AckPolicy, cfg.DeliverPolicy)
	}
	if cfg.MaxDeliver != -1 {
		t.Fatalf("MaxDeliver = %d, want -1", cfg.MaxDeliver)
	}
	if cfg.MaxAckPending != 64 {
		t.Fatalf("MaxAckPending = %d, want the explicit bound 64 (= maxAckPending const %d)", cfg.MaxAckPending, maxAckPending)
	}
}

// Acking quarantined originals means a restart can no longer rediscover unresolved skew from
// JetStream — startup has to ask Postgres instead. Pending rows keep readiness false (while known
// variants still flow); a failed query is an error, not a silent claim of health.
func TestStartupReadinessReflectsPendingQuarantine(t *testing.T) {
	for _, tt := range []struct {
		name      string
		pending   bool
		err       error
		wantReady bool
		wantErr   bool
	}{
		{"no pending quarantine", false, nil, true, false},
		{"pending quarantine", true, nil, false, false},
		{"query failure", false, errors.New("db down"), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, st := testConsumerWithStore()
			c.ready.Store(false)
			st.pending, st.pendingErr = tt.pending, tt.err

			err := c.refreshStartupReadiness(context.Background())

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if c.Ready() != tt.wantReady {
				t.Fatalf("ready = %v, want %v", c.Ready(), tt.wantReady)
			}
		})
	}
}

// SupportsCatalogSchema is the reprocessor's gate: it must derive from the same knownSchemas
// registry as live dispatch, or a deployed binary could re-inject variants it then terminates.
func TestSupportsCatalogSchemaMatchesTheRegistry(t *testing.T) {
	for subject, spec := range knownSchemas {
		if SupportsCatalogSchema(subject, spec.min-1) {
			t.Fatalf("%s: schema %d below min must be unsupported", subject, spec.min-1)
		}
		if !SupportsCatalogSchema(subject, spec.min) || !SupportsCatalogSchema(subject, spec.max) {
			t.Fatalf("%s: min %d and max %d must be supported", subject, spec.min, spec.max)
		}
		if SupportsCatalogSchema(subject, spec.max+1) {
			t.Fatalf("%s: schema %d above max must be unsupported", subject, spec.max+1)
		}
	}
	if SupportsCatalogSchema("platform.unknown.subject", 1) {
		t.Fatal("an unknown subject must be unsupported")
	}
}

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
