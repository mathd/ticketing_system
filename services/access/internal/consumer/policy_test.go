package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/access/internal/store"
)

type fakePolicyMsg struct {
	data    []byte
	subject string
	actions []string
}

func (m *fakePolicyMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}
func (m *fakePolicyMsg) Data() []byte                    { return m.data }
func (m *fakePolicyMsg) Headers() nats.Header            { return nil }
func (m *fakePolicyMsg) Subject() string                 { return m.subject }
func (m *fakePolicyMsg) Reply() string                   { return "" }
func (m *fakePolicyMsg) Ack() error                      { m.actions = append(m.actions, "ack"); return nil }
func (m *fakePolicyMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakePolicyMsg) Nak() error                      { m.actions = append(m.actions, "nak"); return nil }
func (m *fakePolicyMsg) NakWithDelay(time.Duration) error {
	m.actions = append(m.actions, "nak-delay")
	return nil
}
func (m *fakePolicyMsg) InProgress() error           { return nil }
func (m *fakePolicyMsg) Term() error                 { m.actions = append(m.actions, "term"); return nil }
func (m *fakePolicyMsg) TermWithReason(string) error { return m.Term() }

type fakePolicyStore struct {
	upserts []store.SlotPolicy
	events  []uuid.UUID
	err     error
}

func (f *fakePolicyStore) UpsertSlotPolicy(_ context.Context, eventID uuid.UUID, sp store.SlotPolicy) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, eventID)
	f.upserts = append(f.upserts, sp)
	return nil
}

func newPolicyConsumerForTest(st PolicyStore) *PolicyConsumer {
	c := NewPolicyConsumer(nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ready.Store(true)
	return c
}

func policyMsg(body string) *fakePolicyMsg {
	return &fakePolicyMsg{data: []byte(body), subject: SubjectPerformancePublished}
}

// Every fixture below is a hand-written JSON literal, never marshalled from a
// producer type: a fixture built from the type under test cannot fail
// (ADR-017 §5b′, the named trap).

func TestPolicyConsumerProjectsPublishedPolicy(t *testing.T) {
	st := &fakePolicyStore{}
	c := newPolicyConsumerForTest(st)
	msg := policyMsg(`{"id":"20000000-0000-0000-0000-000000000001","type":"platform.catalog.performance.published","schema":2,"occurred_at":"2026-07-01T10:00:00Z","data":{"performance_id":"20000000-0000-0000-0000-000000000002","event_id":"20000000-0000-0000-0000-000000000003","organizer_id":"20000000-0000-0000-0000-000000000004","kind":"operating_day","capacity":5000,"re_entry":{"mode":"count_limited","max_entries":3,"requires_exit":true}}}`)
	c.handle(context.Background(), msg)

	if len(msg.actions) != 1 || msg.actions[0] != "ack" {
		t.Fatalf("actions = %v, want ack", msg.actions)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(st.upserts))
	}
	got := st.upserts[0]
	if got.SlotID != uuid.MustParse("20000000-0000-0000-0000-000000000002") ||
		got.OrganizerID != uuid.MustParse("20000000-0000-0000-0000-000000000004") {
		t.Fatalf("upsert identity = %+v", got)
	}
	if got.Policy.Mode != "count_limited" || got.Policy.MaxEntries == nil || *got.Policy.MaxEntries != 3 || !got.Policy.RequiresExit {
		t.Fatalf("upsert policy = %+v, want count_limited/3/requires_exit", got.Policy)
	}
	if st.events[0] != uuid.MustParse("20000000-0000-0000-0000-000000000001") {
		t.Fatalf("dedup envelope id = %s, want the envelope's id", st.events[0])
	}
}

func TestPolicyConsumerDefaultsAbsentPolicyToSingle(t *testing.T) {
	// A pre-TKT-87 schema-2 emission: no re_entry field at all. Projected as
	// explicit single — absence of knowledge is today's semantics (COS 7).
	st := &fakePolicyStore{}
	c := newPolicyConsumerForTest(st)
	msg := policyMsg(`{"id":"20000000-0000-0000-0000-000000000011","type":"platform.catalog.performance.published","schema":2,"data":{"performance_id":"20000000-0000-0000-0000-000000000012","event_id":"20000000-0000-0000-0000-000000000013","organizer_id":"20000000-0000-0000-0000-000000000014","kind":"performance","capacity":250}}`)
	c.handle(context.Background(), msg)

	if len(msg.actions) != 1 || msg.actions[0] != "ack" || len(st.upserts) != 1 {
		t.Fatalf("actions=%v upserts=%d, want ack + 1 upsert", msg.actions, len(st.upserts))
	}
	if st.upserts[0].Policy.Mode != "single" {
		t.Fatalf("mode = %s, want single", st.upserts[0].Policy.Mode)
	}
}

func TestPolicyConsumerFutureSchemaParksAndLatchesUnready(t *testing.T) {
	// Schema 4 does not exist yet. When it does, its data may be reshaped
	// arbitrarily — this fixture is deliberately incompatible so decoding it
	// with today's struct would fail loudly if dispatch ordering ever broke.
	st := &fakePolicyStore{}
	c := newPolicyConsumerForTest(st)
	msg := policyMsg(`{"id":"20000000-0000-0000-0000-000000000021","type":"platform.catalog.performance.published","schema":4,"data":{"admission":{"policy_ref":"opaque-v4-shape"}}}`)
	c.handle(context.Background(), msg)

	if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
		t.Fatalf("actions = %v, want nak-delay (park, not poison)", msg.actions)
	}
	if len(st.upserts) != 0 {
		t.Fatalf("future variant was applied: %+v", st.upserts)
	}
	if c.Ready() {
		t.Fatal("future schema did not latch the projector unready")
	}
}

func TestPolicyConsumerPoisonTerminatesWithoutTouchingReadiness(t *testing.T) {
	st := &fakePolicyStore{}
	tests := []struct {
		name string
		body string
	}{
		{"unreadable envelope", `{"id":`},
		{"schema zero", `{"id":"20000000-0000-0000-0000-000000000031","type":"platform.catalog.performance.published","schema":0,"data":{}}`},
		{"negative schema", `{"id":"20000000-0000-0000-0000-000000000032","type":"platform.catalog.performance.published","schema":-1,"data":{}}`},
		{"nil id", `{"id":"00000000-0000-0000-0000-000000000000","type":"platform.catalog.performance.published","schema":2,"data":{}}`},
		{"wrong type", `{"id":"20000000-0000-0000-0000-000000000033","type":"platform.catalog.performance.closed","schema":2,"data":{}}`},
		{"nil performance id", `{"id":"20000000-0000-0000-0000-000000000034","type":"platform.catalog.performance.published","schema":2,"data":{"performance_id":"00000000-0000-0000-0000-000000000000","organizer_id":"20000000-0000-0000-0000-000000000035"}}`},
		{"invalid policy invariant", `{"id":"20000000-0000-0000-0000-000000000036","type":"platform.catalog.performance.published","schema":2,"data":{"performance_id":"20000000-0000-0000-0000-000000000037","organizer_id":"20000000-0000-0000-0000-000000000038","re_entry":{"mode":"count_limited","requires_exit":false}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPolicyConsumerForTest(st)
			msg := policyMsg(tt.body)
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "term" {
				t.Fatalf("actions = %v, want term — poison, not the future", msg.actions)
			}
			if !c.Ready() {
				t.Fatal("poison touched readiness; a broken producer must not take access down")
			}
		})
	}
	if len(st.upserts) != 0 {
		t.Fatalf("poison was applied: %+v", st.upserts)
	}
}

func TestPolicyConsumerTransientStoreFailureRetries(t *testing.T) {
	st := &fakePolicyStore{err: errors.New("db down")}
	c := newPolicyConsumerForTest(st)
	msg := policyMsg(`{"id":"20000000-0000-0000-0000-000000000041","type":"platform.catalog.performance.published","schema":2,"data":{"performance_id":"20000000-0000-0000-0000-000000000042","event_id":"20000000-0000-0000-0000-000000000043","organizer_id":"20000000-0000-0000-0000-000000000044","kind":"performance","capacity":10}}`)
	c.handle(context.Background(), msg)

	if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
		t.Fatalf("actions = %v, want nak-delay — the projection must converge, never drop", msg.actions)
	}
	if !c.Ready() {
		t.Fatal("a transient store failure latched readiness")
	}
}
