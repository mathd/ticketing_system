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
	// claim path (ADR-005).
	Kind            string     `json:"kind"`
	Capacity        int32      `json:"capacity"`
	CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
	SharedCapacity  *int32     `json:"shared_capacity,omitempty"`
	// ReEntry rides additively at the current schemas (ADR-017 §2, the `kind`
	// precedent): no deployed consumer forks on it, so no bump. Access projects
	// it for gate-side policy enforcement (ADR-005: catalog owns the policy,
	// access enforces it); a consumer that predates the field treats absence as
	// mode "single", which is also what every pre-field event means.
	ReEntry *ReEntryData `json:"re_entry,omitempty"`
}

// ReEntryData is ADR-005's re_entry_policy on the wire.
type ReEntryData struct {
	Mode         string `json:"mode"`
	MaxEntries   *int32 `json:"max_entries,omitempty"`
	RequiresExit bool   `json:"requires_exit"`
}

// reEntryData mirrors the stored policy; an empty stored mode is the
// pre-typed-slot default and is emitted as explicit single rather than absent —
// absence is reserved for events older than the field itself.
func reEntryData(perf store.Performance) *ReEntryData {
	mode := perf.ReEntry.Mode
	if mode == "" {
		mode = "single"
	}
	return &ReEntryData{Mode: mode, MaxEntries: perf.ReEntry.MaxEntries, RequiresExit: perf.ReEntry.RequiresExit}
}

type PerformanceArchivedData struct {
	PerformanceID   uuid.UUID  `json:"performance_id"`
	EventID         uuid.UUID  `json:"event_id"`
	OrganizerID     uuid.UUID  `json:"organizer_id"`
	CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
}

// festivalCapacity keeps grouped capacity fields atomic and fails closed when
// a persisted group reference cannot produce a valid festival pool.
func festivalCapacity(perf store.Performance) (*uuid.UUID, *int32, error) {
	if perf.CapacityGroupID == nil {
		return nil, nil, nil
	}
	if *perf.CapacityGroupID == uuid.Nil || perf.Kind != store.KindFestivalDay || perf.SharedCapacity == nil || *perf.SharedCapacity <= 0 {
		return nil, nil, fmt.Errorf("grouped performance has invalid festival capacity")
	}
	return perf.CapacityGroupID, perf.SharedCapacity, nil
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
	body, err := performancePublishedEnvelope(perf, occurred)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: SubjectPerformancePublished, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectPerformancePublished, err)
	}
	return nil
}

func performancePublishedEnvelope(perf store.Performance, occurred time.Time) ([]byte, error) {
	id := EventID(perf)
	capacityGroupID, sharedCapacity, err := festivalCapacity(perf)
	if err != nil {
		return nil, err
	}
	schema := 2
	if capacityGroupID != nil {
		schema = 3
	}
	body, err := json.Marshal(Envelope{
		ID:         id,
		Type:       SubjectPerformancePublished,
		OccurredAt: occurred,
		Schema:     schema,
		Data: PerformancePublishedData{
			PerformanceID:   perf.ID,
			EventID:         perf.EventID,
			OrganizerID:     perf.OrganizerID,
			Kind:            perf.Kind,
			Capacity:        perf.Capacity,
			CapacityGroupID: capacityGroupID,
			SharedCapacity:  sharedCapacity,
			ReEntry:         reEntryData(perf),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, nil
}

// ClosureEventID derives the closed/reopened envelope id from the slot id and
// the monotonic closure version: re-emitting one transition carries the same
// id (de-dup at the stream), a new toggle a new id.
func ClosureEventID(subject string, perf store.Performance) string {
	key := fmt.Sprintf("%s:%s:%d", subject, perf.ID, perf.Closure.Version)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func (p *JetStream) SlotClosed(ctx context.Context, perf store.Performance) error {
	return p.publishClosure(ctx, SubjectSlotClosed, perf, perf.Closure.ChangedAt)
}

func (p *JetStream) SlotReopened(ctx context.Context, perf store.Performance) error {
	// occurred_at is the persisted transition instant, not time.Now(), so a
	// retried emission (same deterministic id) carries a byte-stable payload.
	return p.publishClosure(ctx, SubjectSlotReopened, perf, perf.Closure.ChangedAt)
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
	body, err := performanceArchivedEnvelope(perf, occurred)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: SubjectPerformanceArchived, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectPerformanceArchived, err)
	}
	return nil
}

func performanceArchivedEnvelope(perf store.Performance, occurred time.Time) ([]byte, error) {
	id := ArchivedEventID(perf)
	capacityGroupID, _, err := festivalCapacity(perf)
	if err != nil {
		return nil, err
	}
	schema := 2
	if capacityGroupID != nil {
		schema = 3
	}
	body, err := json.Marshal(Envelope{
		ID: id, Type: SubjectPerformanceArchived, OccurredAt: occurred, Schema: schema,
		Data: PerformanceArchivedData{PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID, CapacityGroupID: capacityGroupID},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, nil
}
