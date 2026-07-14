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

const (
	SubjectPerformancePublished = "platform.catalog.performance.published"
	SubjectPerformanceArchived  = "platform.catalog.performance.archived"
	SubjectSlotClosed           = "platform.catalog.performance.closed"
	SubjectSlotReopened         = "platform.catalog.performance.reopened"
)

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
	// Kind lets inventory attribute the pool to a slot kind without forking the
	// claim path (ADR-005). Added additively; Schema stays 2 (backward
	// compatible — existing consumers ignore it, ADR-009).
	Kind     string `json:"kind"`
	Capacity int32  `json:"capacity"`
}

type PerformanceArchivedData struct {
	PerformanceID uuid.UUID `json:"performance_id"`
	EventID       uuid.UUID `json:"event_id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
}

// SlotClosureData carries a weather-closure transition (spike §Case 3). Version
// is the monotonic closure counter; the envelope id is derived from it so a
// re-emitted transition de-duplicates while a new toggle is a distinct event.
type SlotClosureData struct {
	PerformanceID uuid.UUID `json:"performance_id"`
	EventID       uuid.UUID `json:"event_id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	Kind          string    `json:"kind"`
	Version       int32     `json:"closure_version"`
	Reason        *string   `json:"reason,omitempty"`
}

// Publisher is the emission port; the API layer emits through it so tests
// use a fake and the smoke stack the real stream.
type Publisher interface {
	PerformancePublished(ctx context.Context, p store.Performance) error
	PerformanceArchived(ctx context.Context, p store.Performance) error
	SlotClosed(ctx context.Context, p store.Performance) error
	SlotReopened(ctx context.Context, p store.Performance) error
}

func ArchivedEventID(perf store.Performance) string {
	key := SubjectPerformanceArchived + ":" + perf.ID.String()
	if perf.ArchivedAt != nil {
		key += ":" + perf.ArchivedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
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
			Kind:          perf.Kind,
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

// ClosureEventID derives the closed/reopened envelope id from the slot id and
// the monotonic closure version: re-emitting one transition carries the same
// id (de-dup at the stream), a new toggle a new id.
func ClosureEventID(subject string, perf store.Performance) string {
	key := fmt.Sprintf("%s:%s:%d", subject, perf.ID, perf.Closure.Version)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func (p *JetStream) SlotClosed(ctx context.Context, perf store.Performance) error {
	return p.publishClosure(ctx, SubjectSlotClosed, perf, perf.Closure.ClosedAt)
}

func (p *JetStream) SlotReopened(ctx context.Context, perf store.Performance) error {
	return p.publishClosure(ctx, SubjectSlotReopened, perf, nil)
}

func (p *JetStream) publishClosure(ctx context.Context, subject string, perf store.Performance, at *time.Time) error {
	occurred := time.Now().UTC()
	if at != nil {
		occurred = at.UTC()
	}
	id := ClosureEventID(subject, perf)
	body, err := json.Marshal(Envelope{
		ID: id, Type: subject, OccurredAt: occurred, Schema: 1,
		Data: SlotClosureData{
			PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID,
			Kind: perf.Kind, Version: perf.Closure.Version, Reason: perf.Closure.Reason,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	msg := &nats.Msg{Subject: subject, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (p *JetStream) PerformanceArchived(ctx context.Context, perf store.Performance) error {
	occurred := time.Now().UTC()
	if perf.ArchivedAt != nil {
		occurred = *perf.ArchivedAt
	}
	id := ArchivedEventID(perf)
	body, err := json.Marshal(Envelope{
		ID: id, Type: SubjectPerformanceArchived, OccurredAt: occurred, Schema: 2,
		Data: PerformanceArchivedData{PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID},
	})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	msg := &nats.Msg{Subject: SubjectPerformanceArchived, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectPerformanceArchived, err)
	}
	return nil
}
