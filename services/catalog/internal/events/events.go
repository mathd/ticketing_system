// Package events publishes catalog domain events on the PLATFORM stream
// (ADR-007), using the envelope precedent set in ADR-009 §5.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/catalog/internal/store"
)

const SubjectPerformancePublished = "platform.catalog.performance.published"

// Envelope is the platform domain-event envelope (ADR-009 §5): minimal
// identifying payload, versioned shape, type == subject.
type Envelope struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	Schema     int       `json:"schema"`
	Data       any       `json:"data"`
}

type PerformancePublishedData struct {
	PerformanceID uuid.UUID `json:"performance_id"`
	EventID       uuid.UUID `json:"event_id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	Capacity      int32     `json:"capacity"`
}

// Publisher is the emission port; the API layer emits through it so tests
// use a fake and the smoke stack the real stream.
type Publisher interface {
	PerformancePublished(ctx context.Context, p store.Performance) error
}

// JetStream publishes with acks (js.Publish), never fire-and-forget core
// NATS: the stream durably has the event once this returns nil (ADR-009).
type JetStream struct {
	js jetstream.JetStream
}

func NewJetStream(nc *nats.Conn) (*JetStream, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &JetStream{js: js}, nil
}

// EventID derives the envelope id deterministically from the publication
// (performance id + published_at): an emission retried after a failed ack —
// or raced by a concurrent publish request — carries the SAME id, so
// consumers de-duplicate on it and JetStream's Nats-Msg-Id window drops
// exact re-publishes at the stream.
func EventID(perf store.Performance) string {
	key := SubjectPerformancePublished + ":" + perf.ID.String()
	if perf.PublishedAt != nil {
		key += ":" + perf.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func (p *JetStream) PerformancePublished(ctx context.Context, perf store.Performance) error {
	occurred := time.Now().UTC()
	if perf.PublishedAt != nil {
		occurred = *perf.PublishedAt
	}
	id := EventID(perf)
	body, err := json.Marshal(Envelope{
		ID:         id,
		Type:       SubjectPerformancePublished,
		OccurredAt: occurred,
		Schema:     2,
		Data: PerformancePublishedData{
			PerformanceID: perf.ID,
			EventID:       perf.EventID,
			OrganizerID:   perf.OrganizerID,
			Capacity:      perf.Capacity,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	msg := &nats.Msg{Subject: SubjectPerformancePublished, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectPerformancePublished, err)
	}
	return nil
}
