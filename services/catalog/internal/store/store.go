// Package store owns the catalog domain model and its persistence.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound: a referenced entity does not exist (or is not visible on
	// the public read path).
	ErrNotFound = errors.New("not found")
	// ErrOrganizerMismatch: entities wired together must belong to the same
	// organizer (ADR-002 tenancy invariant).
	ErrOrganizerMismatch = errors.New("organizer mismatch")
	// ErrNotSellable: publishing requires at least one ticket type —
	// otherwise the publication event and public visibility would disagree
	// forever ("no sellable offer, no listing" would hide a published slot
	// whose event consumers already saw).
	ErrNotSellable = errors.New("performance has no ticket type")
	// ErrIllegalTransition reports a lifecycle transition that the explicit
	// draft -> published -> archived state machine does not allow.
	ErrIllegalTransition = errors.New("illegal performance lifecycle transition")
	// ErrClosurePending reports a closure toggle attempted while the previous
	// closed/reopened event is still owed (closure_emitted_version <
	// closure_version). The caller must retry the pending transition — which
	// re-emits with the same deterministic id — before toggling again, so the
	// single outbox marker never drops an event.
	ErrClosurePending = errors.New("previous closure event still owed; retry that transition first")
)

// Slot kinds (ADR-005 amendment / US-009). A performance is one kind of dated
// slot; festival days and park operating days share the same machinery.
const (
	KindPerformance  = "performance"
	KindFestivalDay  = "festival_day"
	KindOperatingDay = "operating_day"
)

// ReEntryPolicy is a slot attribute owned by Catalog and enforced by Access at
// the gate (spike §Case 1). single is the performance case (one redemption);
// multi/count_limited generalize to an append-only entry/exit stream.
type ReEntryPolicy struct {
	Mode         string // single | multi | count_limited
	MaxEntries   *int32 // set iff Mode == count_limited
	RequiresExit bool
}

// Closure is the orthogonal weather-closure attribute (spike §Case 3): a
// published slot is open|closed independent of draft|published|archived.
// Version is the monotonic transition counter behind the domain-event id.
type Closure struct {
	Status   string // open | closed
	ClosedAt *time.Time
	Reason   *string
	Version  int32
	// ChangedAt is the instant of the latest closure transition, persisted so
	// the closed/reopened event's occurred_at is stable across emission retries.
	ChangedAt *time.Time
}

// LocalizedText is locale-keyed text; adding a locale is data, not schema (TKT-36).
type LocalizedText map[string]string

type Venue struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Name        string
	GACapacity  int32
	CreatedAt   time.Time
}

type Event struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Name        LocalizedText
	Description LocalizedText
	CreatedAt   time.Time
}

// Performance is a dated slot (ADR-005). kind selects the temporal shape:
// 'performance' carries StartsAt (an instant); 'festival_day'/'operating_day'
// carry the operating window (OperatingDate + OpensAt/ClosesAt, local-date
// semantics). Re-entry and closure are slot attributes; capacity authority
// stays in Inventory (ADR-010), so there is no count here.
type Performance struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	EventID     uuid.UUID
	VenueID     uuid.UUID
	Kind        string
	StartsAt    *time.Time // set for kind 'performance'; nil for day kinds
	// Operating window, set for day kinds. OperatingDate is a local date;
	// OpensAt/ClosesAt are "HH:MM" local times (ClosesAt < OpensAt spans midnight).
	OperatingDate *time.Time
	OpensAt       *string
	ClosesAt      *string
	Timezone      string
	ReEntry       ReEntryPolicy
	Closure       Closure
	// CapacityGroupID is a nullable forward-compat seam for shared festival
	// capacity (TKT-14); unused until then.
	CapacityGroupID *uuid.UUID
	Status          string // draft | published | archived
	PublishedAt     *time.Time
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	// Capacity is the publication-time snapshot used to provision the
	// inventory-owned dated-slot pool. It is not persisted on performances.
	Capacity int32
}

type TicketType struct {
	ID            uuid.UUID
	OrganizerID   uuid.UUID
	PerformanceID uuid.UUID
	Name          LocalizedText
	PriceAmount   int64 // integer minor units (ADR-001); floats are banned
	Currency      string
	CreatedAt     time.Time
}

type VenueInput struct {
	OrganizerID uuid.UUID
	Name        string
	GACapacity  int32
}

type EventInput struct {
	OrganizerID uuid.UUID
	Name        LocalizedText
	Description LocalizedText
}

type PerformanceInput struct {
	OrganizerID   uuid.UUID
	EventID       uuid.UUID
	VenueID       uuid.UUID
	Kind          string     // defaults to KindPerformance when empty
	StartsAt      *time.Time // required for kind 'performance'
	OperatingDate *time.Time // required for day kinds
	OpensAt       *string    // "HH:MM", required for day kinds
	ClosesAt      *string    // "HH:MM", required for day kinds
	Timezone      string
	ReEntry       ReEntryPolicy // defaults to {Mode: single} when Mode empty
}

type TicketTypeInput struct {
	OrganizerID   uuid.UUID
	PerformanceID uuid.UUID
	Name          LocalizedText
	PriceAmount   int64
	Currency      string
}

// PerformanceAggregate carries everything the storefront needs about one slot.
type PerformanceAggregate struct {
	Performance Performance
	Venue       Venue
	TicketTypes []TicketType
}

// EventAggregate is the public read unit: an event with its published
// performances (one aggregated call per page view, ADR-004 rule 3).
type EventAggregate struct {
	Event        Event
	Performances []PerformanceAggregate
}

// Store is the persistence port. Referential and tenancy checks live behind
// it (they need the data); shape/locale validation lives in the API layer.
type Store interface {
	CreateVenue(ctx context.Context, in VenueInput) (Venue, error)
	CreateEvent(ctx context.Context, in EventInput) (Event, error)
	CreatePerformance(ctx context.Context, in PerformanceInput) (Performance, error)
	CreateTicketType(ctx context.Context, in TicketTypeInput) (TicketType, error)
	GetTicketType(ctx context.Context, id uuid.UUID) (TicketType, error)
	GetPublishedPerformance(ctx context.Context, id uuid.UUID) (Performance, error)
	// PublishPerformance flips draft->published (idempotent). needsEmit is
	// true while the domain event for this publication has not been ack'd
	// (event_emitted_at is null) — the caller emits, then marks.
	PublishPerformance(ctx context.Context, id uuid.UUID) (perf Performance, needsEmit bool, err error)
	MarkPerformanceEventEmitted(ctx context.Context, id uuid.UUID) error
	// ArchivePerformance flips published->archived (idempotent). The two
	// marker booleans report whether the publication and archive events are
	// still owed, respectively.
	ArchivePerformance(ctx context.Context, id uuid.UUID) (perf Performance, publishNeedsEmit, archiveNeedsEmit bool, err error)
	MarkPerformanceArchiveEmitted(ctx context.Context, id uuid.UUID) error
	// CloseSlot / ReopenSlot toggle the orthogonal closure attribute while the
	// slot is published (spike §Case 3). Each toggle bumps closure_version;
	// closureNeedsEmit is true while that version's closed/reopened event is
	// owed. publishNeedsEmit reports whether the publication event is still owed
	// — the caller emits it BEFORE the closure event so a closure can never
	// overtake the publication of the same slot. A toggle is refused with
	// ErrClosurePending while a prior closure event is still owed, so the single
	// marker never loses one. Idempotent: closing an already-closed slot (or
	// reopening an open one) does not bump the version.
	CloseSlot(ctx context.Context, id uuid.UUID, reason *string) (perf Performance, publishNeedsEmit, closureNeedsEmit bool, err error)
	ReopenSlot(ctx context.Context, id uuid.UUID) (perf Performance, publishNeedsEmit, closureNeedsEmit bool, err error)
	MarkClosureEmitted(ctx context.Context, id uuid.UUID, version int32) error
	// ListPublishedEvents returns events having at least one published
	// performance, each appearing once with all its published slots.
	ListPublishedEvents(ctx context.Context) ([]EventAggregate, error)
	GetPublishedEvent(ctx context.Context, id uuid.UUID) (EventAggregate, error)
}
