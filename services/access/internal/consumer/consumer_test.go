package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type fakeMsg struct {
	data     []byte
	delivery uint64
	actions  []string
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.delivery}, nil
}
func (m *fakeMsg) Data() []byte                    { return m.data }
func (m *fakeMsg) Headers() nats.Header            { return nil }
func (m *fakeMsg) Subject() string                 { return SubjectOrderCompleted }
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

func validCompletedJSON(t *testing.T) []byte {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(`{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":1,"data":{"order_id":"10000000-0000-0000-0000-000000000002","guest_order_ref":"10000000-0000-0000-0000-000000000003","organizer_id":"10000000-0000-0000-0000-000000000004","buyer_id":"10000000-0000-0000-0000-000000000005","slot_id":"10000000-0000-0000-0000-000000000006","ticket_type_id":"10000000-0000-0000-0000-000000000007","quantity":1}}`), &event); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testConsumer(publish func(context.Context, FailureEvent) error) *Consumer {
	c := &Consumer{
		log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		maxProcessAttempts: 4,
		maxDeliver:         6,
		backoff:            []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond, 6 * time.Millisecond},
		failurePublisher:   publish,
	}
	// Production sets readiness in Run; tests construct the struct directly, so
	// prime it here — every disposition test asserts Ready() explicitly.
	c.ready.Store(true)
	return c
}

// Skew fixtures are hand-written JSON, never marshaled from the completed
// struct: a fixture built from the type under test encodes the compatibility
// it claims to prove and cannot fail (ADR-017 §5b′).
func TestUnknownSchemaVersionSkewIsParkedAndLatchesUnready(t *testing.T) {
	cases := map[string]string{
		"renamed keys, changed types": `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":2,"data":{"order_ref":"ref-1","qty":"2"}}`,
		"empty data":                  `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":2,"data":{}}`,
		"data not an object":          `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":9,"data":[1,2,3]}`,
		"same shape as schema 1":      string(bumpSchema(t, 2)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			published := false
			c := testConsumer(func(context.Context, FailureEvent) error { published = true; return nil })
			msg := &fakeMsg{data: []byte(body), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
				t.Fatalf("actions = %v, want parked (nak-delay)", msg.actions)
			}
			if published {
				t.Fatal("published a failure record for a future variant — it is not a failure")
			}
			if c.Ready() {
				t.Fatal("readiness not latched false on version skew")
			}
		})
	}
}

func bumpSchema(t *testing.T, schema int) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(validCompletedJSON(t), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["schema"] = schema
	body, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBrokenEnvelopeTerminatesAndStaysReady(t *testing.T) {
	cases := map[string]string{
		"schema zero":               `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":0,"data":{}}`,
		"schema negative":           `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":-1,"data":{}}`,
		"missing id":                `{"type":"platform.commerce.order.completed","schema":1,"data":{}}`,
		"missing id, future schema": `{"type":"platform.commerce.order.completed","schema":5,"data":{}}`,
		"wrong type, future schema": `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.cancelled","schema":5,"data":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var failure FailureEvent
			c := testConsumer(func(_ context.Context, event FailureEvent) error { failure = event; return nil })
			msg := &fakeMsg{data: []byte(body), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "term" {
				t.Fatalf("actions = %v, want publish then term", msg.actions)
			}
			if failure.Data.Reason != ReasonInvalidContract {
				t.Fatalf("reason = %q, want %q", failure.Data.Reason, ReasonInvalidContract)
			}
			if !c.Ready() {
				t.Fatal("poison touched readiness — a broken producer must not take access down")
			}
		})
	}
}

func TestInvalidKnownSchemaStillTerminates(t *testing.T) {
	cases := map[string]struct {
		body   string
		reason string
	}{
		"quantity wrong type": {
			body:   `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":1,"data":{"order_id":"10000000-0000-0000-0000-000000000002","guest_order_ref":"10000000-0000-0000-0000-000000000003","organizer_id":"10000000-0000-0000-0000-000000000004","buyer_id":"10000000-0000-0000-0000-000000000005","slot_id":"10000000-0000-0000-0000-000000000006","ticket_type_id":"10000000-0000-0000-0000-000000000007","quantity":"one"}}`,
			reason: ReasonInvalidJSON,
		},
		"quantity zero": {
			body:   `{"id":"10000000-0000-0000-0000-000000000001","type":"platform.commerce.order.completed","schema":1,"data":{"order_id":"10000000-0000-0000-0000-000000000002","guest_order_ref":"10000000-0000-0000-0000-000000000003","organizer_id":"10000000-0000-0000-0000-000000000004","buyer_id":"10000000-0000-0000-0000-000000000005","slot_id":"10000000-0000-0000-0000-000000000006","ticket_type_id":"10000000-0000-0000-0000-000000000007","quantity":0}}`,
			reason: ReasonInvalidContract,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var failure FailureEvent
			c := testConsumer(func(_ context.Context, event FailureEvent) error { failure = event; return nil })
			msg := &fakeMsg{data: []byte(tc.body), delivery: 1}
			c.handle(context.Background(), msg)
			if len(msg.actions) != 1 || msg.actions[0] != "term" {
				t.Fatalf("actions = %v, want publish then term", msg.actions)
			}
			if failure.Data.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", failure.Data.Reason, tc.reason)
			}
			if !c.Ready() {
				t.Fatal("poison at a known schema touched readiness")
			}
		})
	}
}

// The vehicle to the reject path is genuine poison at a schema this binary
// knows (quantity 0 at schema 1) — an unknown schema is not a "permanent
// failure", it is a variant we cannot read yet, and it parks (see
// TestUnknownSchemaVersionSkewIsParkedAndLatchesUnready). The subject here is
// the security property: the attacker-controlled body is never copied into
// the failure record.
func TestPermanentFailurePublishesBeforeTerminating(t *testing.T) {
	body := validCompletedJSON(t)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	decoded["data"].(map[string]any)["quantity"] = float64(0)
	decoded["attacker_payload"] = "must-not-be-copied"
	body, _ = json.Marshal(decoded)
	var failure FailureEvent
	c := testConsumer(func(_ context.Context, event FailureEvent) error { failure = event; return nil })
	msg := &fakeMsg{data: body, delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "term" {
		t.Fatalf("actions = %v, want publish then term", msg.actions)
	}
	encoded, _ := json.Marshal(failure)
	if failure.Data.Reason != ReasonInvalidContract || failure.Data.SourceEventID == "" || string(encoded) == "" {
		t.Fatalf("failure = %+v", failure)
	}
	if json.Valid(encoded) && string(encoded) != "" && contains(string(encoded), "must-not-be-copied") {
		t.Fatalf("failure copied attacker-controlled body: %s", encoded)
	}
	if !c.Ready() {
		t.Fatal("poison at a known schema touched readiness")
	}
}

func TestFailurePublishErrorRequestsRedelivery(t *testing.T) {
	c := testConsumer(func(context.Context, FailureEvent) error { return errors.New("publish unavailable") })
	msg := &fakeMsg{data: []byte(`not-json`), delivery: 1}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
		t.Fatalf("actions = %v, want delayed NAK", msg.actions)
	}
}

func TestFailurePublishErrorRemainsRetryablePastProcessingBudget(t *testing.T) {
	c := testConsumer(func(context.Context, FailureEvent) error { return errors.New("publish unavailable") })
	msg := &fakeMsg{data: []byte(`not-json`), delivery: 7}
	c.handle(context.Background(), msg)
	if len(msg.actions) != 1 || msg.actions[0] != "nak-delay" {
		t.Fatalf("actions = %v, want delayed NAK after former delivery limit", msg.actions)
	}
}

func TestTransientFailureUsesBackoffThenTerminatesWithFailureRecord(t *testing.T) {
	var failure FailureEvent
	c := testConsumer(func(_ context.Context, event FailureEvent) error { failure = event; return nil })
	c.process = func(context.Context, completed) (FailureStage, error) {
		return StageDelivery, errors.New("commerce unavailable")
	}

	beforeLimit := &fakeMsg{data: validCompletedJSON(t), delivery: 2}
	c.handle(context.Background(), beforeLimit)
	if len(beforeLimit.actions) != 0 {
		t.Fatalf("transient retry explicitly acked/nacked instead of using consumer backoff: %v", beforeLimit.actions)
	}

	exhausted := &fakeMsg{data: validCompletedJSON(t), delivery: 4}
	c.handle(context.Background(), exhausted)
	if len(exhausted.actions) != 1 || exhausted.actions[0] != "term" {
		t.Fatalf("exhausted actions = %v", exhausted.actions)
	}
	if failure.Data.Reason != ReasonDeliveryExhausted || failure.Data.Attempts != 4 {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestConsumerConfigBoundsProcessingAndKeepsFailurePublicationRetryable(t *testing.T) {
	c := testConsumer(func(context.Context, FailureEvent) error { return nil })
	config := c.consumerConfig("access-ticket-issuer")
	if config.MaxDeliver != -1 || len(config.BackOff) != 6 || config.AckPolicy != jetstream.AckExplicitPolicy {
		t.Fatalf("consumer config = %+v", config)
	}
}

func contains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
