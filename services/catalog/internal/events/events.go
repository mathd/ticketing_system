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
	"ticketing/shared/domainevent"
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
	// OrphanPrevention is the schema-5 fork's payload half (TKT-183): the rule
	// setting of the exact bound seat-map version. `omitempty` is load-bearing — it
	// is what keeps the schema-4 bytes byte-identical to what shipped, since a
	// rule-off seated slot serializes false and false is omitted. A consequence
	// worth stating: a schema-5 event can never carry `false`, because schema 5 is
	// only chosen when the flag is true.
	OrphanPrevention bool `json:"orphan_prevention_enabled,omitempty"`
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
func SeatMapPublishedEventID(m store.SeatMap) string { return seatMapPublishedEventUUID(m).String() }

// seatMapPublishedEventUUID is the same derivation before the string
// conversion. The envelope's id is a uuid.UUID (ADR-033), and going
// uuid -> string -> uuid to fill it would mean parsing on a publish path for
// no reason; the exported helper keeps its string signature for its callers.
func seatMapPublishedEventUUID(m store.SeatMap) uuid.UUID {
	key := SubjectSeatMapPublished + ":" + m.ID.String()
	if m.PublishedAt != nil {
		key += ":" + m.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
}

func seatMapPublishedEnvelope(m store.SeatMap) ([]byte, error) {
	occurred := time.Now().UTC()
	if m.PublishedAt != nil {
		occurred = m.PublishedAt.UTC()
	}
	body, err := json.Marshal(domainevent.Envelope[SeatMapPublishedData]{
		ID:         seatMapPublishedEventUUID(m),
		Type:       SubjectSeatMapPublished,
		OccurredAt: occurred,
		Schema:     1,
		Data: SeatMapPublishedData{
			SeatMapID:   m.ID,
			OrganizerID: m.OrganizerID,
			VenueID:     m.VenueID,
			Version:     m.Version,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal seat map envelope: %w", err)
	}
	return body, nil
}

func (p *JetStream) SeatMapPublished(ctx context.Context, m store.SeatMap) error {
	id := SeatMapPublishedEventID(m)
	body, err := seatMapPublishedEnvelope(m)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: SubjectSeatMapPublished, Data: body}
	msg.Header = nats.Header{"Nats-Msg-Id": []string{id}}
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", SubjectSeatMapPublished, err)
	}
	return nil
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
func BackfillEventID(perf store.Performance) string { return backfillEventUUID(perf).String() }

func backfillEventUUID(perf store.Performance) uuid.UUID {
	key := SubjectPerformancePublished + ":backfill:" + backfillEpoch + ":" + perf.ID.String()
	if perf.PublishedAt != nil {
		key += ":" + perf.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
}

// orphanPreventionEpoch namespaces the TKT-183 correction wave's identity. Fixed, like
// backfillEpoch, so re-running converges instead of multiplying events (ADR-041).
//
// backfillEpoch is deliberately NOT bumped for this: that would change reemit-policies'
// identity for every ungrouped slot — a different wave, a different candidate set — and
// the id it produced might already sit in a consumer's consumed_events from an earlier
// re_entry backfill, where a replay is a silent no-op. A new wave gets a new namespace.
const orphanPreventionEpoch = "orphan-prevention-schema5-1"

// OrphanPreventionCorrectionEventID derives the correction wave's envelope id for an
// already-published slot (TKT-183). It MUST differ from both EventID (inventory consumed
// that one as schema 4) and BackfillEventID (access consumed that one, and
// reemit-policies may reuse it) — a correction under a consumed id is dropped by
// ON CONFLICT DO NOTHING and repairs nothing, silently.
//
// published_at is in the key so an unpublish/republish is a distinct publication and
// stays correctable.
func OrphanPreventionCorrectionEventID(perf store.Performance) string {
	key := SubjectPerformancePublished + ":orphan-prevention:" + orphanPreventionEpoch + ":" + perf.ID.String()
	if perf.PublishedAt != nil {
		key += ":" + perf.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

// PerformancePublishedOrphanCorrection re-emits a published slot's publication under the
// correction identity (TKT-183). The payload is whatever the live builder produces for
// the slot today — which, for a slot bound to a rule-enabled version, is schema 5.
func (p *JetStream) PerformancePublishedOrphanCorrection(ctx context.Context, perf store.Performance) error {
	return p.publishPerformancePublished(ctx, perf, OrphanPreventionCorrectionEventID(perf))
}

func ArchivedEventID(perf store.Performance) string { return archivedEventUUID(perf).String() }

func archivedEventUUID(perf store.Performance) uuid.UUID {
	key := SubjectPerformanceArchived + ":" + perf.ID.String()
	if perf.ArchivedAt != nil {
		key += ":" + perf.ArchivedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
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
func EventID(perf store.Performance) string { return eventUUID(perf).String() }

func eventUUID(perf store.Performance) uuid.UUID {
	key := SubjectPerformancePublished + ":" + perf.ID.String()
	if perf.PublishedAt != nil {
		key += ":" + perf.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
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
	occurred := time.Now().UTC()
	if perf.PublishedAt != nil {
		occurred = *perf.PublishedAt
	}
	body, err := performancePublishedEnvelopeWithID(perf, occurred, id)
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
	// The id arrives as a string because that is what the exported EventID
	// family returns and what callers pass. The envelope holds a uuid.UUID
	// (ADR-033), so it converts here -- once, returning an error rather than
	// panicking, since a publish path is no place for a MustParse.
	envelopeID, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("envelope id %q is not a uuid: %w", id, err)
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
	case !seated && perf.OrphanPreventionEnabled:
		// The flag comes from the bound seat-map version, so a GA slot carrying it is
		// a corrupt row. Fail closed rather than emit schema 5 with no map for the
		// consumer to project, or schema 2 silently dropping a rule someone enabled.
		return nil, fmt.Errorf("orphan prevention requires a seat map")
	case seated && perf.OrphanPreventionEnabled:
		// TKT-183. Schema and flag are ONE fact: 5 is emitted exactly when the flag is
		// true, and the flag is serialized exactly when the schema is 5. Neither may
		// appear without the other — a schema-5 event with the flag absent would
		// provision a rule-enabled pool that says nothing is enabled, and a schema-4
		// event for an enabled version is the TKT-179 gap this ticket exists to close.
		schema = 5
	case seated:
		schema = 4
	case capacityGroupID != nil:
		schema = 3
	}
	body, err := json.Marshal(domainevent.Envelope[PerformancePublishedData]{
		ID:         envelopeID,
		Type:       SubjectPerformancePublished,
		OccurredAt: occurred,
		Schema:     schema,
		Data: PerformancePublishedData{
			PerformanceID:    perf.ID,
			EventID:          perf.EventID,
			OrganizerID:      perf.OrganizerID,
			Kind:             perf.Kind,
			Capacity:         perf.Capacity,
			CapacityGroupID:  capacityGroupID,
			SharedCapacity:   sharedCapacity,
			SeatMapID:        perf.SeatMapID,
			OrphanPrevention: perf.OrphanPreventionEnabled,
			ReEntry:          reEntryData(perf),
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
	return closureEventUUID(subject, perf).String()
}

func closureEventUUID(subject string, perf store.Performance) uuid.UUID {
	key := fmt.Sprintf("%s:%s:%d", subject, perf.ID, perf.Closure.Version)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
}

func (p *JetStream) SlotClosed(ctx context.Context, perf store.Performance) error {
	return p.publishClosure(ctx, SubjectSlotClosed, perf, perf.Closure.ChangedAt)
}

func (p *JetStream) SlotReopened(ctx context.Context, perf store.Performance) error {
	// occurred_at is the persisted transition instant, not time.Now(), so a
	// retried emission (same deterministic id) carries a byte-stable payload.
	return p.publishClosure(ctx, SubjectSlotReopened, perf, perf.Closure.ChangedAt)
}

// closureEnvelope marshals the closed/reopened envelope. The other three
// subjects already had a pure byte seam; this one was inline in publishClosure,
// so it is extracted to give every published subject the same testable shape.
func closureEnvelope(subject string, perf store.Performance, occurred time.Time) ([]byte, error) {
	body, err := json.Marshal(domainevent.Envelope[SlotClosureData]{
		ID: closureEventUUID(subject, perf), Type: subject, OccurredAt: occurred, Schema: 1,
		Data: SlotClosureData{
			PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID,
			Kind: perf.Kind, Version: perf.Closure.Version, Reason: perf.Closure.Reason,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, nil
}

func (p *JetStream) publishClosure(ctx context.Context, subject string, perf store.Performance, at *time.Time) error {
	occurred := time.Now().UTC()
	if at != nil {
		occurred = at.UTC()
	}
	id := ClosureEventID(subject, perf)
	body, err := closureEnvelope(subject, perf, occurred)
	if err != nil {
		return err
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
	id := archivedEventUUID(perf)
	capacityGroupID, _, err := festivalCapacity(perf)
	if err != nil {
		return nil, err
	}
	schema := 2
	if capacityGroupID != nil {
		schema = 3
	}
	body, err := json.Marshal(domainevent.Envelope[PerformanceArchivedData]{
		ID: id, Type: SubjectPerformanceArchived, OccurredAt: occurred, Schema: schema,
		Data: PerformanceArchivedData{PerformanceID: perf.ID, EventID: perf.EventID, OrganizerID: perf.OrganizerID, CapacityGroupID: capacityGroupID},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, nil
}
