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
	return &Consumer{
		log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		maxProcessAttempts: 4,
		maxDeliver:         6,
		backoff:            []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond, 6 * time.Millisecond},
		failurePublisher:   publish,
	}
}

func TestPermanentFailurePublishesBeforeTerminating(t *testing.T) {
	body := validCompletedJSON(t)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	decoded["schema"] = float64(99)
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
