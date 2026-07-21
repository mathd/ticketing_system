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
	ErrClosurePending       = errors.New("previous closure event still owed; retry that transition first")
	ErrMembershipConflict   = errors.New("group membership conflict")
	ErrMembershipFrozen     = errors.New("series membership is frozen")
	ErrEmptySeries          = errors.New("series has no members")
	ErrSlotKindMismatch     = errors.New("only festival_day slots can join a festival")
	ErrAlreadyGrouped       = errors.New("slot already belongs to a festival")
	ErrGroupedSlotLifecycle = errors.New("grouped festival day must transition via its festival")
	ErrFestivalNotDraft     = errors.New("festival is not draft")
	ErrEmptyFestival        = errors.New("festival has no members")
	// ErrSeatMapConflict reports any uniqueness collision while authoring seat
	// geometry (US-019): a duplicate seat identity, or two sections/rows/seats
	// sharing a name/label/position within their scope. One sentinel maps every
	// such unique violation to 409 so none falls through to a 500.
	ErrSeatMapConflict = errors.New("seat map conflict: duplicate seat, row, section, name, or position")
	// ErrSeatMapNotPublished reports a seated performance referencing a seat map
	// that exists and belongs to the organizer/venue but is not in the published
	// state (TKT-103): a slot may only be seated against a published version, so
	// a draft/archived reference is rejected rather than silently accepted.
	ErrSeatMapNotPublished = errors.New("seat map is not published")
	// ErrSeatMapEditOrphansPinned reports an EditSeatMap whose new geometry would
	// drop (orphan) a seat identity that a sale/hold currently pins (TKT-104,
	// COS-2/3). The edit is hard-rejected — never silently applied — so a pinned
	// identity always resolves in the current published version. See ADR-029.
	ErrSeatMapEditOrphansPinned = errors.New("edit would orphan a pinned seat identity")
	// ErrSeatIdentityNotFound reports a PinSeat against an identity absent from
	// the family's current published version (TKT-104). Symmetric with the edit
	// rejection: an edit cannot drop a pinned seat, and a pin cannot reference a
	// seat the current version does not contain — together they close the
	// edit-vs-sale race (ADR-018 two-sided lock).
	ErrSeatIdentityNotFound = errors.New("seat identity not in current published version")
)

type SeriesTransitionConflict struct {
	PerformanceID uuid.UUID
	Reason        string
	Cause         error
}

func (e *SeriesTransitionConflict) Error() string { return e.Reason }
func (e *SeriesTransitionConflict) Unwrap() error { return e.Cause }

type FestivalTransitionConflict struct {
	PerformanceID uuid.UUID
	Reason        string
	Cause         error
}

func (e *FestivalTransitionConflict) Error() string { return e.Reason }
func (e *FestivalTransitionConflict) Unwrap() error { return e.Cause }

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

// PoolOfferState is the reconciliation answer for a pool id (TKT-90).
// Lifecycle and Closure are meaningful only for kind "performance".
type PoolOfferState struct {
	Kind      string // "performance" | "festival"
	Lifecycle string // performance status: draft | published | archived
	Closure   Closure
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

type SeriesMember struct {
	PerformanceID uuid.UUID
	Position      int32
}

type Series struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	EventID     uuid.UUID
	Name        LocalizedText
	Members     []SeriesMember
	CreatedAt   time.Time
}

type Season struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	Name        LocalizedText
	SeriesIDs   []uuid.UUID
	EventIDs    []uuid.UUID
	CreatedAt   time.Time
}

type Festival struct {
	ID             uuid.UUID
	OrganizerID    uuid.UUID
	Name           LocalizedText
	SharedCapacity int32
	Status         string
	MemberIDs      []uuid.UUID
	CreatedAt      time.Time
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
	// CapacityGroupID identifies the festival capacity group for grouped
	// festival days; nil keeps ordinary slots keyed by their own id.
	CapacityGroupID *uuid.UUID
	// SeatMapID references the exact published seat-map version this slot is
	// seated against (TKT-103); nil is a GA slot. A version is a seat_maps row
	// (TKT-102), so the id IS the version. Seated and CapacityGroupID are
	// mutually exclusive — a festival day is GA-shared-capacity by definition.
	SeatMapID   *uuid.UUID
	Status      string // draft | published | archived
	PublishedAt *time.Time
	ArchivedAt  *time.Time
	CreatedAt   time.Time
	// Capacity is the publication-time snapshot used to provision the
	// inventory-owned dated-slot pool. It is not persisted on performances.
	Capacity int32
	// SharedCapacity is the publication-time snapshot used to provision a
	// festival's shared inventory pool. It is not persisted on performances.
	SharedCapacity *int32
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

// SeatMap is a venue-owned, versioned description of reserved seating (US-019).
// TKT-102 authors it in the draft state only; Version + Status are the stable
// shape TKT-103 (publish) and TKT-104 (edit-safety) extend. A venue keeps its
// GA capacity and may carry seat maps simultaneously.
type SeatMap struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	VenueID     uuid.UUID
	Name        string
	Version     int32
	Status      string // draft | published | archived
	// PublishedAt is set when the map is published (TKT-103); nil while draft.
	PublishedAt *time.Time
	CreatedAt   time.Time
}

// SeatMapSection / SeatMapRow / SeatMapSeat are the geometry tree. The nested
// slices are populated only by GetSeatMapGeometry; the Add* writes return the
// flat created resource (empty children).
type SeatMapSection struct {
	ID       uuid.UUID
	Name     string
	Position int32
	Rows     []SeatMapRow
}

type SeatMapRow struct {
	ID       uuid.UUID
	Label    string
	Position int32
	Seats    []SeatMapSeat
}

type SeatMapSeat struct {
	ID uuid.UUID
	// SeatIdentity is the stable contract TKT-104 pins against, composed
	// server-side as "section/row/seat" from the parent labels, never mutated.
	SeatIdentity string
	Label        string
	Position     int32
}

// SeatMapGeometry is the full nested read unit: a map with its ordered
// sections -> rows -> seats (each level ordered by position).
type SeatMapGeometry struct {
	Map      SeatMap
	Sections []SeatMapSection
}

type VenueInput struct {
	OrganizerID uuid.UUID
	Name        string
	GACapacity  int32
}

// VenueGACapacityInput carries a GA-capacity update (TKT-105). Organizer-scoped
// like every owned-entity write (ADR-002).
type VenueGACapacityInput struct {
	OrganizerID uuid.UUID
	VenueID     uuid.UUID
	GACapacity  int32
}

type EventInput struct {
	OrganizerID uuid.UUID
	Name        LocalizedText
	Description LocalizedText
}

type SeatMapInput struct {
	OrganizerID uuid.UUID
	VenueID     uuid.UUID
	Name        string
}

type SeatMapSectionInput struct {
	OrganizerID uuid.UUID
	SeatMapID   uuid.UUID
	Name        string
	Position    int32
}

type SeatMapRowInput struct {
	OrganizerID uuid.UUID
	SeatMapID   uuid.UUID
	SectionID   uuid.UUID
	Label       string
	Position    int32
}

// SeatMapSeatInput carries the seat's own label; SeatIdentity is composed by
// the store from the parent section/row labels, not supplied by the caller.
type SeatMapSeatInput struct {
	OrganizerID uuid.UUID
	SeatMapID   uuid.UUID
	RowID       uuid.UUID
	Label       string
	Position    int32
}

// EditSeatMapInput is the full replacement geometry for a published seat map
// (TKT-104). SeatMapID is any version of the target family (the store resolves
// the family's current published version and locks it); Sections is the complete
// new section->row->seat tree. An edit that would orphan a pinned seat identity
// is hard-rejected (ErrSeatMapEditOrphansPinned); otherwise a new published
// version is created and the old one stays immutable.
type EditSeatMapInput struct {
	OrganizerID uuid.UUID
	SeatMapID   uuid.UUID
	Sections    []EditSectionInput
}

type EditSectionInput struct {
	Name     string
	Position int32
	Rows     []EditRowInput
}

type EditRowInput struct {
	Label    string
	Position int32
	Seats    []EditSeatInput
}

type EditSeatInput struct {
	Label    string
	Position int32
}

// PinSeatInput records (or clears) that a seat identity is referenced by a
// sale/hold (TKT-104). SeatMapID is any version of the family; the pin is
// version-independent (keyed on the family). PinnedBy is a free-form reference
// ("sale:<order_id>", "hold:<hold_id>") that TKT-80 fills in — it is part of the
// idempotency key so distinct references to the same seat are distinct pins.
type PinSeatInput struct {
	OrganizerID  uuid.UUID
	SeatMapID    uuid.UUID
	SeatIdentity string
	PinnedBy     string
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
	// SeatMapID, when set, makes this a seated slot referencing a published
	// seat-map version (TKT-103). The store validates the map exists, is
	// published, and shares the performance's organizer and venue; nil is GA.
	SeatMapID *uuid.UUID
}

type TicketTypeInput struct {
	OrganizerID   uuid.UUID
	PerformanceID uuid.UUID
	Name          LocalizedText
	PriceAmount   int64
	Currency      string
}

type SeriesInput struct {
	OrganizerID uuid.UUID
	EventID     uuid.UUID
	Name        LocalizedText
}

type SeasonInput struct {
	OrganizerID uuid.UUID
	Name        LocalizedText
}

type FestivalInput struct {
	OrganizerID    uuid.UUID
	Name           LocalizedText
	SharedCapacity int32
}

type SeriesTransition struct {
	Performance      Performance
	PublishNeedsEmit bool
	ArchiveNeedsEmit bool
}

type SeriesAggregate struct {
	Series         Series
	PerformanceIDs []uuid.UUID
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
	Series       []SeriesAggregate
	Performances []PerformanceAggregate
}

type SeasonAggregate struct {
	Season Season
	Events []EventAggregate
}

type FestivalAggregate struct {
	Festival     Festival
	Performances []PerformanceAggregate
}

// Store is the persistence port. Referential and tenancy checks live behind
// it (they need the data); shape/locale validation lives in the API layer.
type Store interface {
	CreateVenue(ctx context.Context, in VenueInput) (Venue, error)
	// ListVenues returns an organizer's venues, name-ordered (US-018). Tenant
	// scoping is a query predicate, not a post-filter (ADR-002).
	ListVenues(ctx context.Context, organizerID uuid.UUID) ([]Venue, error)
	CreateEvent(ctx context.Context, in EventInput) (Event, error)
	// Seat-map authoring (US-019), draft-only. Each Add* scopes its parent by
	// (id, organizer_id) and requires the map status='draft' in one INSERT ...
	// SELECT, so cross-map/cross-organizer parentage and writes to a non-draft
	// map are unrepresentable through the store. A no-match yields ErrNotFound;
	// any uniqueness collision yields ErrSeatMapConflict.
	CreateSeatMap(ctx context.Context, in SeatMapInput) (SeatMap, error)
	AddSeatMapSection(ctx context.Context, in SeatMapSectionInput) (SeatMapSection, error)
	AddSeatMapRow(ctx context.Context, in SeatMapRowInput) (SeatMapRow, error)
	AddSeatMapSeat(ctx context.Context, in SeatMapSeatInput) (SeatMapSeat, error)
	// PublishSeatMap flips a seat map draft->published (TKT-103, idempotent,
	// monotonic/lock-free per ADR-018). needsEmit is true while the
	// seat_map.published domain event has not been ack'd (event_emitted_at is
	// null) — the caller emits, then marks. A published version is immutable:
	// the Add* write gate (status='draft') refuses further authoring.
	PublishSeatMap(ctx context.Context, id uuid.UUID) (m SeatMap, needsEmit bool, err error)
	MarkSeatMapEventEmitted(ctx context.Context, id uuid.UUID) error
	// EditSeatMap safely edits a published seat map (TKT-104). Under a family-
	// scoped advisory lock (NOT a current-row FOR UPDATE — see ADR-029/§lock), in
	// one transaction it re-resolves the family's current published version,
	// validates that every currently-pinned seat identity survives exactly once in
	// the submitted geometry, then creates a new published version (version+1, same
	// map_family_id) with the new geometry and commits. An edit that would orphan a
	// pinned identity is rejected with ErrSeatMapEditOrphansPinned; the predecessor
	// is never mutated. needsEmit is true while the new version's seat_map.published
	// event is still owed — the caller emits, then marks (same discipline as
	// PublishSeatMap). The lock is state-deriving per ADR-018 (the outcome depends
	// on the current pinned set), and PinSeat takes the SAME family lock so an edit
	// and a concurrent pin serialize.
	EditSeatMap(ctx context.Context, in EditSeatMapInput) (m SeatMap, needsEmit bool, err error)
	// PinSeat records that a seat identity is referenced by a sale/hold — the
	// write path TKT-80 consumes (COS-5). Under the SAME family advisory lock
	// EditSeatMap takes, it re-resolves the current published version, validates the
	// identity exists in that version (else ErrSeatIdentityNotFound), and inserts
	// the pin idempotently on (map_family_id, seat_identity, pinned_by). Taking the
	// same family lock as the edit is what closes the edit-vs-sale race (ADR-029).
	PinSeat(ctx context.Context, in PinSeatInput) error
	// UnpinSeat clears a pin (sale cancelled / hold released), so a later edit may
	// drop that seat. Idempotent: removing an absent pin is a no-op.
	UnpinSeat(ctx context.Context, in PinSeatInput) error
	// GetSeatMapGeometry returns a map's full nested geometry, each level
	// ordered by position; ErrNotFound if the map does not exist.
	GetSeatMapGeometry(ctx context.Context, seatMapID uuid.UUID) (SeatMapGeometry, error)
	// ListVenueSeatMaps returns a venue's seat-map summaries (no geometry),
	// version-then-name ordered. Tenant/venue scoping is a query predicate
	// backed by seat_maps_by_venue (ADR-019).
	ListVenueSeatMaps(ctx context.Context, venueID uuid.UUID) ([]SeatMap, error)
	// ListSeatMapVersions returns every version of the family that seatMapID
	// belongs to (any version resolves the family), newest first, each carrying
	// its PublishedAt. It is the read behind the TKT-105 version-history UI
	// (COS-3); "current" is the caller's job to derive (highest published
	// version). ErrNotFound if seatMapID is unknown.
	ListSeatMapVersions(ctx context.Context, seatMapID uuid.UUID) ([]SeatMap, error)
	// UpdateVenueGACapacity sets a venue's GA capacity (TKT-105 COS-5) — until
	// now it was write-once at CreateVenue. Organizer-scoped as a query predicate
	// (ADR-002); ErrNotFound when the (id, organizer_id) pair matches no row.
	UpdateVenueGACapacity(ctx context.Context, in VenueGACapacityInput) (Venue, error)
	CreatePerformance(ctx context.Context, in PerformanceInput) (Performance, error)
	CreateTicketType(ctx context.Context, in TicketTypeInput) (TicketType, error)
	CreateSeries(ctx context.Context, in SeriesInput) (Series, error)
	AttachPerformanceToSeries(ctx context.Context, seriesID, performanceID uuid.UUID, position int32) (Series, error)
	CreateSeason(ctx context.Context, in SeasonInput) (Season, error)
	AttachSeriesToSeason(ctx context.Context, seasonID, seriesID uuid.UUID) (Season, error)
	AttachEventToSeason(ctx context.Context, seasonID, eventID uuid.UUID) (Season, error)
	CreateFestival(ctx context.Context, in FestivalInput) (Festival, error)
	AttachDayToFestival(ctx context.Context, festivalID, performanceID uuid.UUID) (Festival, error)
	GetTicketType(ctx context.Context, id uuid.UUID) (TicketType, error)
	GetPublishedPerformance(ctx context.Context, id uuid.UUID) (Performance, error)
	// GetPoolOfferState answers for an inventory pool id whatever it is — a
	// performance in ANY lifecycle or a festival capacity group — so the
	// reconciliation pass (TKT-90) only ever acts on positive assertions.
	GetPoolOfferState(ctx context.Context, id uuid.UUID) (PoolOfferState, error)
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
	PublishSeries(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error)
	ArchiveSeries(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error)
	PublishFestival(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error)
	ArchiveFestival(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error)
	// ListPublishedEvents returns events having at least one published
	// performance, each appearing once with all its published slots.
	ListPublishedEvents(ctx context.Context) ([]EventAggregate, error)
	GetPublishedEvent(ctx context.Context, id uuid.UUID) (EventAggregate, error)
	GetPublishedSeason(ctx context.Context, id uuid.UUID) (SeasonAggregate, error)
	GetPublishedFestival(ctx context.Context, id uuid.UUID) (FestivalAggregate, error)
}
