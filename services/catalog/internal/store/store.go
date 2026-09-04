// Package store owns the catalog domain model and its persistence.
package store

import (
	"errors"
	"time"
	"unicode/utf8"

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
	// ErrSeatMapFamilyNotFound reports an unpin whose (seat_map_id, organizer_id)
	// resolved no family at all (TKT-306).
	//
	// It is NOT a failure and callers keep treating an unpin as successful when they
	// see it — the operation is idempotent and there is genuinely nothing to unpin.
	// What it adds is the DISTINCTION the previous bare `return nil` erased: "the pins
	// were already gone" and "you named a map that does not exist, or one belonging to
	// another organizer" are different facts, and only the second means the caller is
	// confused about what it is releasing.
	//
	// The case that motivated it: an internal caller passing the WRONG organizer for a
	// real map got `nil`, reported a successful release, and left the pins in place —
	// discoverable only later, by TKT-112's reconcile sweep, as pins naming a claim
	// nobody remembers. Answering nil there is not idempotency, it is a wrong answer
	// that looks like the right one.
	ErrSeatMapFamilyNotFound = errors.New("seat map family not found for this organizer")
	// ErrSeatIdentityNotFound reports a PinSeat against an identity absent from
	// the family's current published version (TKT-104). Symmetric with the edit
	// rejection: an edit cannot drop a pinned seat, and a pin cannot reference a
	// seat the current version does not contain — together they close the
	// edit-vs-sale race (ADR-018 two-sided lock).
	ErrSeatIdentityNotFound = errors.New("seat identity not in current published version")
	// ErrSeatIdentityTooLong rejects a composed identity that the Commerce and
	// Inventory request contracts cannot carry.
	ErrSeatIdentityTooLong = errors.New("seat identity exceeds 200 characters")
	// ErrIdempotencyConflict reports a create whose idempotency key already
	// belongs to a DIFFERENT request for this organizer (TKT-200). Replaying the
	// first result would hand the caller a resource it did not ask for, so the
	// reuse is refused. Same decision, and the same reasoning, as commerce's
	// checkout and refund paths.
	ErrIdempotencyConflict = errors.New("idempotency key reused with different terms")
)

// MaxSeatIdentityCharacters is shared by Catalog's producer contract and the
// Commerce and Inventory consumers. OpenAPI maxLength and Postgres char_length
// both count Unicode characters, so this check counts runes rather than bytes.
const MaxSeatIdentityCharacters = 200

// ComposeSeatIdentity builds the stable identity and enforces the bound every
// consumer accepts. Call it before any write that persists a composed identity.
func ComposeSeatIdentity(section, row, seat string) (string, error) {
	identity := section + "/" + row + "/" + seat
	if utf8.RuneCountInString(identity) > MaxSeatIdentityCharacters {
		return "", ErrSeatIdentityTooLong
	}
	return identity, nil
}

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

// PerformanceDisplayName is the minimum a wallet row needs to say which show a
// purchase was for (TKT-222). Not a Performance: this read deliberately carries
// no venue, capacity, pricing or publication state — a caller that needs those
// has GetPublishedPerformance, and a bulk read is not the place to widen what an
// internal caller can pull in one request.
type PerformanceDisplayName struct {
	PerformanceID uuid.UUID
	EventName     LocalizedText
	// Nullable, like Performance.StartsAt: a FESTIVAL DAY has an operating_date
	// and opening hours instead of an instant (ADR-014). A plain time.Time here
	// makes the resolver fail on exactly the purchases a festival wallet contains.
	StartsAt *time.Time
}

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
	SeatMapID *uuid.UUID
	// OrphanPreventionEnabled is the rule setting of the EXACT bound seat-map
	// version, not of the map family (ADR-029: a published version is immutable and
	// a slot is bound to one of them). Hydrated by a join in the shared read, so an
	// emitter cannot accidentally reach for the family's current version — the
	// difference is invisible until someone edits the map, and then it is silent.
	OrphanPreventionEnabled bool
	Status                  string // draft | published | archived
	PublishedAt             *time.Time
	ArchivedAt              *time.Time
	CreatedAt               time.Time
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
	// OrphanPreventionEnabled turns on the single-seat orphan rule for slots seated
	// against THIS version (ADR-041 / TKT-179). Per version, not per family: a
	// published version is immutable and an edit mints a new one (ADR-029), and a
	// seated pool binds to one specific version — which is what stops a republish
	// changing the rule a live pool enforces. Defaults false; nothing reads it yet.
	OrphanPreventionEnabled bool
	CreatedAt               time.Time
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
	// IdempotencyKey deduplicates the create (TKT-200). The API refuses an empty
	// one before reaching here, so an empty value means a non-contract caller
	// (a test fixture, a future internal writer) and stores NULL — outside the
	// partial unique index rather than colliding with every other keyless row.
	IdempotencyKey string
}

type SeatMapInput struct {
	OrganizerID uuid.UUID
	VenueID     uuid.UUID
	Name        string
	// OrphanPreventionEnabled defaults false, so a caller that does not know about
	// the setting creates exactly the map it created before (ADR-041).
	OrphanPreventionEnabled bool
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
	// OrphanPreventionEnabled is nil to INHERIT the version being edited, or set to
	// apply a new value to the newly minted version only. Inheritance is the default
	// because an edit is a geometry change: a caller who says nothing about the rule
	// must not silently turn it off, and a published version can never be altered
	// either way (ADR-029).
	OrphanPreventionEnabled *bool
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

// BatchPinInput pins (or clears) a whole seat set under one family advisory lock, all
// or nothing (TKT-80). A seat-set hold in inventory maps to one BatchPin: either every
// seat pins or the batch fails (ErrSeatIdentityNotFound) and none do — matching the
// inventory hold's atomicity so a live claim never has a partially-pinned set.
type BatchPinInput struct {
	OrganizerID    uuid.UUID
	SeatMapID      uuid.UUID
	SeatIdentities []string
	PinnedBy       string
}

// SeatMapPin is one row of the pin table as the reconciliation read exposes it
// (TKT-112). SeatMapID is a REPRESENTATIVE version of the pin's family, not the
// version the pin was created against — pins are version-independent (ADR-029),
// and UnpinSeats resolves the family from whatever version id it is handed, so any
// member of the family lets the caller reach the pin through the family-locked
// path. A pin whose family has no seat_maps row left is not listable and therefore
// not reclaimable by this read: there would be no version id to name it with.
type SeatMapPin struct {
	ID           uuid.UUID
	OrganizerID  uuid.UUID
	SeatMapID    uuid.UUID
	SeatIdentity string
	PinnedBy     string
}

// MaxSeatMapPinPage bounds one reconciliation page. The operator scan is a full
// drain of seat_map_pins, so the page — not the table — is what bounds memory and
// the HTTP payload.
const MaxSeatMapPinPage = 500

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
	// IdempotencyKey deduplicates the create (TKT-200). See EventInput.
	IdempotencyKey string
}

type TicketTypeInput struct {
	OrganizerID   uuid.UUID
	PerformanceID uuid.UUID
	Name          LocalizedText
	PriceAmount   int64
	Currency      string
	// IdempotencyKey deduplicates the create (TKT-200). See EventInput.
	IdempotencyKey string
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
