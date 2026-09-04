package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// These interfaces live beside the API that consumes them. catalogStore is the
// construction-time union; each Server field exposes only one cohesive group.
type authoringStore interface {
	CreateVenue(ctx context.Context, in store.VenueInput) (store.Venue, error)
	CreateEvent(ctx context.Context, in store.EventInput) (store.Event, error)
	CreatePerformance(ctx context.Context, in store.PerformanceInput) (store.Performance, error)
	CreateTicketType(ctx context.Context, in store.TicketTypeInput) (store.TicketType, error)
	CreateSeries(ctx context.Context, in store.SeriesInput) (store.Series, error)
	AttachPerformanceToSeries(ctx context.Context, organizerID, seriesID, performanceID uuid.UUID, position int32) (store.Series, error)
	CreateSeason(ctx context.Context, in store.SeasonInput) (store.Season, error)
	AttachSeriesToSeason(ctx context.Context, organizerID, seasonID, seriesID uuid.UUID) (store.Season, error)
	AttachEventToSeason(ctx context.Context, organizerID, seasonID, eventID uuid.UUID) (store.Season, error)
	CreateFestival(ctx context.Context, in store.FestivalInput) (store.Festival, error)
	AttachDayToFestival(ctx context.Context, organizerID, festivalID, performanceID uuid.UUID) (store.Festival, error)
}

type seatMapStore interface {
	CreateSeatMap(ctx context.Context, in store.SeatMapInput) (store.SeatMap, error)
	AddSeatMapSection(ctx context.Context, in store.SeatMapSectionInput) (store.SeatMapSection, error)
	AddSeatMapRow(ctx context.Context, in store.SeatMapRowInput) (store.SeatMapRow, error)
	AddSeatMapSeat(ctx context.Context, in store.SeatMapSeatInput) (store.SeatMapSeat, error)
	PublishSeatMap(ctx context.Context, organizerID, id uuid.UUID) (store.SeatMap, bool, error)
	MarkSeatMapEventEmitted(ctx context.Context, id uuid.UUID) error
	EditSeatMap(ctx context.Context, in store.EditSeatMapInput) (store.SeatMap, bool, error)
	GetSeatMapGeometry(ctx context.Context, seatMapID uuid.UUID) (store.SeatMapGeometry, error)
	ListVenueSeatMaps(ctx context.Context, venueID uuid.UUID) ([]store.SeatMap, error)
	ListSeatMapVersions(ctx context.Context, seatMapID uuid.UUID) ([]store.SeatMap, error)
	UpdateVenueGACapacity(ctx context.Context, in store.VenueGACapacityInput) (store.Venue, error)
}

type lifecycleStore interface {
	PublishPerformance(ctx context.Context, organizerID, id uuid.UUID) (store.Performance, bool, error)
	MarkPerformanceEventEmitted(ctx context.Context, id uuid.UUID) error
	ArchivePerformance(ctx context.Context, organizerID, id uuid.UUID) (store.Performance, bool, bool, error)
	MarkPerformanceArchiveEmitted(ctx context.Context, id uuid.UUID) error
	CloseSlot(ctx context.Context, organizerID, id uuid.UUID, reason *string) (store.Performance, bool, bool, error)
	ReopenSlot(ctx context.Context, organizerID, id uuid.UUID) (store.Performance, bool, bool, error)
	MarkClosureEmitted(ctx context.Context, id uuid.UUID, version int32) error
	PublishSeries(ctx context.Context, organizerID, id uuid.UUID) ([]store.SeriesTransition, error)
	ArchiveSeries(ctx context.Context, organizerID, id uuid.UUID) ([]store.SeriesTransition, error)
	PublishFestival(ctx context.Context, organizerID, id uuid.UUID) ([]store.SeriesTransition, error)
	ArchiveFestival(ctx context.Context, organizerID, id uuid.UUID) ([]store.SeriesTransition, error)
}

type channelStore interface {
	CreateChannel(ctx context.Context, in store.ChannelInput) (store.Channel, error)
	UpdateChannel(ctx context.Context, organizerID, id uuid.UUID, in store.ChannelUpdate) (store.Channel, error)
	GetChannel(ctx context.Context, id uuid.UUID) (store.Channel, error)
	ListChannels(ctx context.Context, organizerID uuid.UUID) ([]store.Channel, error)
	ListEnabledChannels(ctx context.Context, organizerID uuid.UUID) ([]store.PublicChannel, error)
}

type inventoryStore interface {
	GetTicketType(ctx context.Context, id uuid.UUID) (store.TicketType, error)
	GetPublishedPerformance(ctx context.Context, id uuid.UUID) (store.Performance, error)
	GetPoolOfferState(ctx context.Context, id uuid.UUID) (store.PoolOfferState, error)
	ListSeatMapPins(ctx context.Context, after uuid.UUID, limit int) ([]store.SeatMapPin, error)
	PinSeats(ctx context.Context, in store.BatchPinInput) error
	UnpinSeats(ctx context.Context, in store.BatchPinInput) error
}

type pricingStore interface {
	ResolveTicketTypePrice(ctx context.Context, ticketTypeID uuid.UUID, channel *string, at time.Time) (store.RuleSelection, error)
	ResolveTicketTypeFees(ctx context.Context, ticketTypeID uuid.UUID, channel *string, at time.Time) (store.FeeSelection, error)
}

type staffStore interface {
	AuthenticateStaff(ctx context.Context, identifier, password string) (store.StaffAccount, error)
}

type displayNameStore interface {
	PerformanceDisplayNames(ctx context.Context, ids []uuid.UUID) ([]store.PerformanceDisplayName, error)
}

type venueReadStore interface {
	ListVenues(ctx context.Context, organizerID uuid.UUID) ([]store.Venue, error)
}

type handlerStore interface {
	authoringStore
	seatMapStore
	lifecycleStore
	channelStore
	inventoryStore
	pricingStore
	staffStore
	displayNameStore
	venueReadStore
}

type catalogStore interface {
	handlerStore
	publicReadSource
}
