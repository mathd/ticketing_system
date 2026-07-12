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

func (p *JetStream) PerformancePublished(ctx context.Context, perf store.Performance) error {
	occurred := time.Now().UTC()
	if perf.PublishedAt != nil {
		occurred = *perf.PublishedAt
	}
	body, err := json.Marshal(Envelope{
		ID:         uuid.NewString(),
		Type:       SubjectPerformancePublished,
		OccurredAt: occurred,
		Schema:     1,
		Data: PerformancePublishedData{
			PerformanceID: perf.ID,
			EventID:       perf.EventID,
			OrganizerID:   perf.OrganizerID,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := p.js.Publish(ctx, SubjectPerformancePublished, body); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectPerformancePublished, err)
	}
	return nil
}
