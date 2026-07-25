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
	// SubjectSeatMapPublished announces a seat-map version becoming published
	// (TKT-103). No consumer reads it yet — it is versioned against future
	// readers (TKT-104/TKT-35/TKT-80), and its emission follows the same
	// emit-after-commit + deterministic-id discipline as the performance events.
	SubjectSeatMapPublished = "platform.catalog.seat_map.published"
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
	// SeatMapID is set only on the seated fork (schema 4, TKT-103): the exact
	// published seat-map version this slot references. Inventory dispatches on
	// the schema and does NOT provision a quantity pool for it (the seat-level
	// claim is TKT-80); access still projects re_entry. A GA/festival event
	// (schema 2/3) omits it.
	SeatMapID *uuid.UUID `json:"seat_map_id,omitempty"`
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

// SeatMapPublishedData carries the identity of a published seat-map version
// (TKT-103). The id references the exact immutable version (a seat_maps row);
// Version is carried for readers that reason about version families without a
// second lookup. Capacity/geometry stay out — a consumer reads the map by id.
type SeatMapPublishedData struct {
	SeatMapID   uuid.UUID `json:"seat_map_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	VenueID     uuid.UUID `json:"venue_id"`
	Version     int32     `json:"version"`
}

// Publisher is the emission port; the API layer emits through it so tests
// use a fake and the smoke stack the real stream.
type Publisher interface {
	PerformancePublished(ctx context.Context, p store.Performance) error
	PerformancePublishedBackfill(ctx context.Context, p store.Performance) error
	PerformanceArchived(ctx context.Context, p store.Performance) error
	SlotClosed(ctx context.Context, p store.Performance) error
	SlotReopened(ctx context.Context, p store.Performance) error
	SeatMapPublished(ctx context.Context, m store.SeatMap) error
}

// SeatMapPublishedEventID derives the seat_map.published envelope id from the
// map id and its publication instant, so a retried emission carries the same id
// and de-duplicates at the stream (mirrors EventID for performances).
func SeatMapPublishedEventID(m store.SeatMap) string {
	key := SubjectSeatMapPublished + ":" + m.ID.String()
	if m.PublishedAt != nil {
		key += ":" + m.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func seatMapPublishedEnvelope(m store.SeatMap) ([]byte, error) {
	return marshalEnvelope(SubjectSeatMapPublished, SeatMapPublishedEventID(m), 1, occurredAt(m.PublishedAt),
		SeatMapPublishedData{
			SeatMapID:   m.ID,
			OrganizerID: m.OrganizerID,
			VenueID:     m.VenueID,
			Version:     m.Version,
		})
}

func (p *JetStream) SeatMapPublished(ctx context.Context, m store.SeatMap) error {
	body, err := seatMapPublishedEnvelope(m)
	if err != nil {
		return err
	}
	return p.publish(ctx, SubjectSeatMapPublished, SeatMapPublishedEventID(m), body)
}

// backfillEpoch namespaces the re-emission id (TKT-96). It is a FIXED string,
// never a timestamp or counter, so re-running the backfill produces the same id
// for a slot and dedups to a no-op on the second run (COS-2). A future
// correction wave that must re-emit again with a fresh identity bumps this
// constant — the deliberate, reviewable escape hatch.
const backfillEpoch = "reentry-backfill-1"

// BackfillEventID derives the re-emission's envelope id for an already-published
// slot (TKT-96). It MUST differ from EventID(perf): access's projector dedups on
// the envelope id via consumed_events ON CONFLICT DO NOTHING, so re-emitting
// under the live id would be swallowed and the re_entry backfill would silently
// do nothing (ADR-017 §1: the id is a dedup key, not a schema concern — the
// payload rides the unchanged (type,schema) 2/3). The ":backfill:" segment sits
// in a fixed position so the derivation can never collide with the live EventID
// domain; the id stays deterministic on (slot, published_at) so re-runs converge.
func BackfillEventID(perf store.Performance) string {
	key := SubjectPerformancePublished + ":backfill:" + backfillEpoch + ":" + perf.ID.String()
	if perf.PublishedAt != nil {
		key += ":" + perf.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
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

// marshalEnvelope builds the wire body for every catalog subject. The per-subject
// builders below feed it, so the envelope shape cannot drift between subjects —
// and they stay separate functions because the golden-byte tests assert against
// them directly.
func marshalEnvelope(subject, id string, schema int, occurred time.Time, data any) ([]byte, error) {
	body, err := json.Marshal(Envelope{ID: id, Type: subject, OccurredAt: occurred, Schema: schema, Data: data})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, nil
}

// publish is the single emission path. Nats-Msg-Id always carries the envelope
// id — they are the same dedup key seen from the stream and from a consumer,
// and binding them here is what stops the two drifting apart per subject.
func (p *JetStream) publish(ctx context.Context, subject, id string, body []byte) error {
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{"Nats-Msg-Id": []string{id}}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// occurredAt prefers the persisted transition instant over time.Now(), so a
// retried emission under the same deterministic id carries a byte-stable
// payload.
func occurredAt(at *time.Time) time.Time {
	if at != nil {
		return at.UTC()
	}
	return time.Now().UTC()
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
	return p.publishPerformancePublished(ctx, perf, EventID(perf))
}

// PerformancePublishedBackfill re-emits an already-published slot's publication
// under a DISTINCT deterministic id (TKT-96), so access re-projects its current
// re_entry policy for slots published before the field existed. The payload is
// byte-for-byte the live-publish payload (same schema fork, same re_entry
// population) — only the envelope id and Nats-Msg-Id differ, which is what makes
// the re-emission escape dedup while staying additive-without-bump (ADR-017 §1).
func (p *JetStream) PerformancePublishedBackfill(ctx context.Context, perf store.Performance) error {
	return p.publishPerformancePublished(ctx, perf, BackfillEventID(perf))
}

func (p *JetStream) publishPerformancePublished(ctx context.Context, perf store.Performance, id string) error {
	// occurred keeps the stored instant unconverted (not .UTC()) to match the
	// bytes this path has always emitted; occurredAt is not used here for that
	// reason.
	occurred := time.Now().UTC()
	if perf.PublishedAt != nil {
		occurred = *perf.PublishedAt
	}
	body, err := performancePublishedEnvelopeWithID(perf, occurred, id)
	if err != nil {
		return err
	}
	return p.publish(ctx, SubjectPerformancePublished, id, body)
}

// performancePublishedEnvelope marshals the live-publish envelope, deriving the
// id from EventID. The backfill path shares the body via
// performancePublishedEnvelopeWithID so the schema fork and re_entry population
// can never drift between the two.
func performancePublishedEnvelope(perf store.Performance, occurred time.Time) ([]byte, error) {
	return performancePublishedEnvelopeWithID(perf, occurred, EventID(perf))
}

func performancePublishedEnvelopeWithID(perf store.Performance, occurred time.Time, id string) ([]byte, error) {
	capacityGroupID, sharedCapacity, err := festivalCapacity(perf)
	if err != nil {
		return nil, err
	}
	// Schema is chosen from the payload's own shape (ADR-017 §4): 2 = plain GA,
	// 3 = grouped festival, 4 = seated (TKT-103). Seated and grouped are mutually
	// exclusive — a grouped festival day is shared-capacity GA — so a slot that
	// is somehow both is a corrupt row we fail closed on rather than emit an
	// ambiguous variant.
	seated := perf.SeatMapID != nil
	schema := 2
	switch {
	case seated && capacityGroupID != nil:
		return nil, fmt.Errorf("seated slot must not carry festival capacity")
	case seated:
		schema = 4
	case capacityGroupID != nil:
		schema = 3
	}
	return marshalEnvelope(SubjectPerformancePublished, id, schema, occurred,
		PerformancePublishedData{
			PerformanceID:   perf.ID,
			EventID:         perf.EventID,
			OrganizerID:     perf.OrganizerID,
			Kind:            perf.Kind,
			Capacity:        perf.Capacity,
			CapacityGroupID: capacityGroupID,
			SharedCapacity:  sharedCapacity,
			SeatMapID:       perf.SeatMapID,
			ReEntry:         reEntryData(perf),
		})
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

func slotClosureEnvelope(subject string, perf store.Performance, occurred time.Time) ([]byte, error) {
	return marshalEnvelope(subject, ClosureEventID(subject, perf), 1, occurred,
		SlotClosureData{
			PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID,
			Kind: perf.Kind, Version: perf.Closure.Version, Reason: perf.Closure.Reason,
		})
}

func (p *JetStream) publishClosure(ctx context.Context, subject string, perf store.Performance, at *time.Time) error {
	body, err := slotClosureEnvelope(subject, perf, occurredAt(at))
	if err != nil {
		return err
	}
	return p.publish(ctx, subject, ClosureEventID(subject, perf), body)
}

func (p *JetStream) PerformanceArchived(ctx context.Context, perf store.Performance) error {
	// occurred keeps the stored instant unconverted, matching the bytes this
	// path has always emitted.
	occurred := time.Now().UTC()
	if perf.ArchivedAt != nil {
		occurred = *perf.ArchivedAt
	}
	body, err := performanceArchivedEnvelope(perf, occurred)
	if err != nil {
		return err
	}
	return p.publish(ctx, SubjectPerformanceArchived, ArchivedEventID(perf), body)
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
	return marshalEnvelope(SubjectPerformanceArchived, id, schema, occurred,
		PerformanceArchivedData{PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID, CapacityGroupID: capacityGroupID})
}
