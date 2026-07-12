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
)

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

// Performance is the catalog's first dated slot (ADR-005): no
// concert-specific fields, so festival/park days fit the same shape later.
type Performance struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	EventID     uuid.UUID
	VenueID     uuid.UUID
	StartsAt    time.Time
	Timezone    string
	Status      string // draft | published
	PublishedAt *time.Time
	CreatedAt   time.Time
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
	OrganizerID uuid.UUID
	EventID     uuid.UUID
	VenueID     uuid.UUID
	StartsAt    time.Time
	Timezone    string
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
	// PublishPerformance flips draft->published (idempotent). needsEmit is
	// true while the domain event for this publication has not been ack'd
	// (event_emitted_at is null) — the caller emits, then marks.
	PublishPerformance(ctx context.Context, id uuid.UUID) (perf Performance, needsEmit bool, err error)
	MarkPerformanceEventEmitted(ctx context.Context, id uuid.UUID) error
	// ListPublishedEvents returns events having at least one published
	// performance, each appearing once with all its published slots.
	ListPublishedEvents(ctx context.Context) ([]EventAggregate, error)
	GetPublishedEvent(ctx context.Context, id uuid.UUID) (EventAggregate, error)
}
