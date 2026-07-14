//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	orderCompletedSubject = "platform.commerce.order.completed"
	accessFailureSubject  = "platform.access.ticket-issuance.failed"
)

type accessFailureEvent struct {
	Data struct {
		SourceEventID      string `json:"source_event_id"`
		MessageFingerprint string `json:"message_fingerprint"`
		Reason             string `json:"reason"`
		Stage              string `json:"stage"`
		Attempts           uint64 `json:"attempts"`
	} `json:"data"`
}

func TestAccessPoisonEventPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := stream.Consumer(ctx, "access-ticket-issuer")
	if err != nil {
		t.Fatal(err)
	}
	info, err := issuer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxDeliver != 6 || len(info.Config.BackOff) != 6 || info.Config.BackOff[0] != 100*time.Millisecond {
		t.Fatalf("access consumer is not finitely configured for smoke: %+v", info.Config)
	}

	consumerName := "access-failures-smoke-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	failures, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: consumerName, FilterSubject: accessFailureSubject, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.DeleteConsumer(context.Background(), consumerName) })

	invalidID := uuid.New()
	invalidBody, _ := json.Marshal(map[string]any{
		"id": invalidID, "type": orderCompletedSubject, "schema": 99, "attacker_marker": "must-not-be-copied",
		"data": map[string]any{"quantity": 1},
	})
	if _, err := js.Publish(ctx, orderCompletedSubject, invalidBody, jetstream.WithMsgID(invalidID.String())); err != nil {
		t.Fatal(err)
	}
	invalidFailure := nextAccessFailure(t, failures, 5*time.Second)
	if invalidFailure.Data.SourceEventID != invalidID.String() || invalidFailure.Data.Reason != "invalid_contract" || invalidFailure.Data.Stage != "contract" || invalidFailure.Data.Attempts != 1 {
		t.Fatalf("permanent failure = %+v", invalidFailure)
	}
	encoded, _ := json.Marshal(invalidFailure)
	if strings.Contains(string(encoded), "must-not-be-copied") || invalidFailure.Data.MessageFingerprint == "" {
		t.Fatalf("failed-event record is not sanitized: %s", encoded)
	}

	transientID := uuid.New()
	transientBody, _ := json.Marshal(map[string]any{
		"id": transientID, "type": orderCompletedSubject, "schema": 1,
		"data": map[string]any{
			"order_id": uuid.New(), "guest_order_ref": uuid.New(), "organizer_id": uuid.New(), "buyer_id": uuid.New(),
			"slot_id": uuid.New(), "ticket_type_id": uuid.New(), "quantity": 1,
		},
	})
	if _, err := js.Publish(ctx, orderCompletedSubject, transientBody, jetstream.WithMsgID(transientID.String())); err != nil {
		t.Fatal(err)
	}
	exhausted := nextAccessFailure(t, failures, 10*time.Second)
	if exhausted.Data.SourceEventID != transientID.String() || exhausted.Data.Reason != "delivery_retries_exhausted" || exhausted.Data.Stage != "delivery" || exhausted.Data.Attempts != 4 {
		t.Fatalf("exhausted failure = %+v", exhausted)
	}

	retry(t, 15*time.Second, func() error {
		query := url.QueryEscape(`access_event_failures_total{service_name="access"}`)
		code, body := get(t, promURL+"/api/v1/query?query="+query, nil)
		if code != http.StatusOK || !strings.Contains(string(body), `"result":[{`) {
			return fmt.Errorf("access failure metric not exported yet: %d %s", code, body)
		}
		return nil
	})
}

func nextAccessFailure(t *testing.T, consumer jetstream.Consumer, wait time.Duration) accessFailureEvent {
	t.Helper()
	message, err := consumer.Next(jetstream.FetchMaxWait(wait))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = message.Ack() }()
	var failure accessFailureEvent
	if err := json.Unmarshal(message.Data(), &failure); err != nil {
		t.Fatalf("decode failed-event record: %v: %s", err, message.Data())
	}
	return failure
}
