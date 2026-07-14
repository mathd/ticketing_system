package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationsFS embed.FS

// Migrate applies the embedded goose migrations (ADR-008): every service
// migrates its own database at startup, before listening, and fails fast.
func Migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

type Postgres struct {
	db *sql.DB
}

func NewPostgres(db *sql.DB) *Postgres {
	return &Postgres{db: db}
}

// isFKViolation reports whether err is a Postgres foreign-key violation
// (SQLSTATE 23503) without importing driver-specific error types.
func isFKViolation(err error) bool {
	type coder interface{ SQLState() string }
	var c coder
	return errors.As(err, &c) && c.SQLState() == "23503"
}

func (p *Postgres) CreateVenue(ctx context.Context, in VenueInput) (Venue, error) {
	v := Venue{OrganizerID: in.OrganizerID, Name: in.Name, GACapacity: in.GACapacity}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO venues (organizer_id, name, ga_capacity)
		 VALUES ($1, $2, $3) RETURNING id, created_at`,
		in.OrganizerID, in.Name, in.GACapacity).Scan(&v.ID, &v.CreatedAt)
	if isFKViolation(err) {
		return Venue{}, fmt.Errorf("organizer: %w", ErrNotFound)
	}
	if err != nil {
		return Venue{}, fmt.Errorf("insert venue: %w", err)
	}
	return v, nil
}

func (p *Postgres) CreateEvent(ctx context.Context, in EventInput) (Event, error) {
	name, err := json.Marshal(in.Name)
	if err != nil {
		return Event{}, fmt.Errorf("marshal name: %w", err)
	}
	var desc any
	if len(in.Description) > 0 {
		b, err := json.Marshal(in.Description)
		if err != nil {
			return Event{}, fmt.Errorf("marshal description: %w", err)
		}
		desc = b
	}
	e := Event{OrganizerID: in.OrganizerID, Name: in.Name, Description: in.Description}
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO events (organizer_id, name, description)
		 VALUES ($1, $2, $3) RETURNING id, created_at`,
		in.OrganizerID, name, desc).Scan(&e.ID, &e.CreatedAt)
	if isFKViolation(err) {
		return Event{}, fmt.Errorf("organizer: %w", ErrNotFound)
	}
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	return e, nil
}

func (p *Postgres) CreatePerformance(ctx context.Context, in PerformanceInput) (Performance, error) {
	// Existence + same-organizer checks first, for precise errors
	// (ADR-002 tenancy: performance, event and venue share one organizer).
	var evOrg, vOrg uuid.UUID
	err := p.db.QueryRowContext(ctx,
		`SELECT organizer_id FROM events WHERE id = $1`, in.EventID).Scan(&evOrg)
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, fmt.Errorf("event: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, fmt.Errorf("lookup event: %w", err)
	}
	err = p.db.QueryRowContext(ctx,
		`SELECT organizer_id FROM venues WHERE id = $1`, in.VenueID).Scan(&vOrg)
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, fmt.Errorf("venue: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, fmt.Errorf("lookup venue: %w", err)
	}
	if evOrg != in.OrganizerID || vOrg != in.OrganizerID {
		return Performance{}, ErrOrganizerMismatch
	}

	perf := Performance{OrganizerID: in.OrganizerID, EventID: in.EventID, VenueID: in.VenueID,
		StartsAt: in.StartsAt, Timezone: in.Timezone, Status: "draft"}
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO performances (organizer_id, event_id, venue_id, starts_at, timezone)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id, starts_at, created_at`,
		in.OrganizerID, in.EventID, in.VenueID, in.StartsAt, in.Timezone).
		Scan(&perf.ID, &perf.StartsAt, &perf.CreatedAt)
	if err != nil {
		return Performance{}, fmt.Errorf("insert performance: %w", err)
	}
	return perf, nil
}

func (p *Postgres) CreateTicketType(ctx context.Context, in TicketTypeInput) (TicketType, error) {
	var perfOrg uuid.UUID
	err := p.db.QueryRowContext(ctx,
		`SELECT organizer_id FROM performances WHERE id = $1`, in.PerformanceID).Scan(&perfOrg)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketType{}, fmt.Errorf("performance: %w", ErrNotFound)
	}
	if err != nil {
		return TicketType{}, fmt.Errorf("lookup performance: %w", err)
	}
	if perfOrg != in.OrganizerID {
		return TicketType{}, ErrOrganizerMismatch
	}
	name, err := json.Marshal(in.Name)
	if err != nil {
		return TicketType{}, fmt.Errorf("marshal name: %w", err)
	}
	tt := TicketType{OrganizerID: in.OrganizerID, PerformanceID: in.PerformanceID,
		Name: in.Name, PriceAmount: in.PriceAmount, Currency: in.Currency}
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO ticket_types (organizer_id, performance_id, name, price_amount, currency)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		in.OrganizerID, in.PerformanceID, name, in.PriceAmount, in.Currency).
		Scan(&tt.ID, &tt.CreatedAt)
	if err != nil {
		return TicketType{}, fmt.Errorf("insert ticket type: %w", err)
	}
	return tt, nil
}

func (p *Postgres) GetTicketType(ctx context.Context, id uuid.UUID) (TicketType, error) {
	var tt TicketType
	var raw []byte
	err := p.db.QueryRowContext(ctx, `SELECT id,organizer_id,performance_id,name,price_amount,currency,created_at
		FROM ticket_types WHERE id=$1`, id).Scan(&tt.ID, &tt.OrganizerID, &tt.PerformanceID, &raw, &tt.PriceAmount, &tt.Currency, &tt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return tt, ErrNotFound
	}
	if err != nil {
		return tt, err
	}
	if err := json.Unmarshal(raw, &tt.Name); err != nil {
		return tt, fmt.Errorf("ticket type name: %w", err)
	}
	return tt, nil
}

func (p *Postgres) PublishPerformance(ctx context.Context, id uuid.UUID) (Performance, bool, error) {
	// The transition is gated on a sellable offer existing (ErrNotSellable
	// otherwise): the publication event and public visibility must never
	// disagree. Single atomic statement flips draft->published exactly
	// once; the returned row also says whether the domain event is owed.
	res, err := p.db.ExecContext(ctx,
		`UPDATE performances SET status = 'published', published_at = now()
		 WHERE id = $1 AND status = 'draft'
		   AND EXISTS (SELECT 1 FROM ticket_types t WHERE t.performance_id = $1)`, id)
	if err != nil {
		return Performance{}, false, fmt.Errorf("publish: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Nothing flipped: not found, already published, or unpriced draft.
		perf, _, _, err := p.getPerformance(ctx, id)
		if err != nil {
			return Performance{}, false, err
		}
		if perf.Status == "draft" {
			return Performance{}, false, ErrNotSellable
		}
		if perf.Status == "archived" {
			return Performance{}, false, ErrIllegalTransition
		}
	}
	perf, emittedAt, _, err := p.getPerformance(ctx, id)
	if err != nil {
		return Performance{}, false, err
	}
	return perf, emittedAt == nil, nil
}

func (p *Postgres) getPerformance(ctx context.Context, id uuid.UUID) (Performance, *sql.NullTime, *sql.NullTime, error) {
	var perf Performance
	var emitted, archiveEmitted sql.NullTime
	err := p.db.QueryRowContext(ctx,
		`SELECT p.id, p.organizer_id, p.event_id, p.venue_id, p.starts_at, p.timezone,
		        p.status, p.published_at, p.archived_at, p.event_emitted_at,
		        p.archive_emitted_at, p.created_at, v.ga_capacity
		 FROM performances p JOIN venues v ON v.id = p.venue_id WHERE p.id = $1`, id).
		Scan(&perf.ID, &perf.OrganizerID, &perf.EventID, &perf.VenueID, &perf.StartsAt,
			&perf.Timezone, &perf.Status, &perf.PublishedAt, &perf.ArchivedAt, &emitted,
			&archiveEmitted, &perf.CreatedAt, &perf.Capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, nil, nil, fmt.Errorf("performance: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, nil, nil, fmt.Errorf("get performance: %w", err)
	}
	var emittedPtr, archiveEmittedPtr *sql.NullTime
	if emitted.Valid {
		emittedPtr = &emitted
	}
	if archiveEmitted.Valid {
		archiveEmittedPtr = &archiveEmitted
	}
	return perf, emittedPtr, archiveEmittedPtr, nil
}

func (p *Postgres) GetPublishedPerformance(ctx context.Context, id uuid.UUID) (Performance, error) {
	perf, _, _, err := p.getPerformance(ctx, id)
	if err != nil {
		return Performance{}, err
	}
	if perf.Status != "published" {
		return Performance{}, ErrNotFound
	}
	return perf, nil
}

func (p *Postgres) ArchivePerformance(ctx context.Context, id uuid.UUID) (Performance, bool, bool, error) {
	res, err := p.db.ExecContext(ctx,
		`UPDATE performances SET status = 'archived', archived_at = now()
		 WHERE id = $1 AND status = 'published'`, id)
	if err != nil {
		return Performance{}, false, false, fmt.Errorf("archive: %w", err)
	}
	perf, publishedEmitted, archiveEmitted, err := p.getPerformance(ctx, id)
	if err != nil {
		return Performance{}, false, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 && perf.Status == "draft" {
		return Performance{}, false, false, ErrIllegalTransition
	}
	return perf, publishedEmitted == nil, archiveEmitted == nil, nil
}

func (p *Postgres) MarkPerformanceEventEmitted(ctx context.Context, id uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE performances SET event_emitted_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark emitted: %w", err)
	}
	return nil
}

func (p *Postgres) MarkPerformanceArchiveEmitted(ctx context.Context, id uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE performances SET archive_emitted_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark archive emitted: %w", err)
	}
	return nil
}

// publicPerformances returns the publicly listable slots (published AND
// priced — no sellable offer, no listing) grouped into event aggregates,
// events ordered by their earliest slot, slots by start time.
func (p *Postgres) publicPerformances(ctx context.Context, eventID *uuid.UUID) ([]EventAggregate, error) {
	query := `
		SELECT e.id, e.organizer_id, e.name, e.description, e.created_at,
		       p.id, p.starts_at, p.timezone, p.status, p.published_at, p.created_at,
		       v.id, v.name, v.ga_capacity, v.created_at,
		       t.id, t.name, t.price_amount, t.currency, t.created_at
		FROM performances p
		JOIN events e ON e.id = p.event_id
		JOIN venues v ON v.id = p.venue_id
		JOIN ticket_types t ON t.performance_id = p.id
		WHERE p.status = 'published' AND ($1::uuid IS NULL OR e.id = $1)
		ORDER BY p.starts_at, p.id, t.price_amount, t.id`
	rows, err := p.db.QueryContext(ctx, query, eventID)
	if err != nil {
		return nil, fmt.Errorf("public read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type perfRef struct {
		agg *EventAggregate
		idx int
	}
	var (
		order    []uuid.UUID
		byEvent  = map[uuid.UUID]*EventAggregate{}
		perfSeen = map[uuid.UUID]perfRef{}
	)
	for rows.Next() {
		var (
			ev     Event
			perf   Performance
			venue  Venue
			tt     TicketType
			evName []byte
			evDesc []byte
			ttName []byte
		)
		if err := rows.Scan(
			&ev.ID, &ev.OrganizerID, &evName, &evDesc, &ev.CreatedAt,
			&perf.ID, &perf.StartsAt, &perf.Timezone, &perf.Status, &perf.PublishedAt, &perf.CreatedAt,
			&venue.ID, &venue.Name, &venue.GACapacity, &venue.CreatedAt,
			&tt.ID, &ttName, &tt.PriceAmount, &tt.Currency, &tt.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan public read: %w", err)
		}
		if err := json.Unmarshal(evName, &ev.Name); err != nil {
			return nil, fmt.Errorf("event name jsonb: %w", err)
		}
		if evDesc != nil {
			if err := json.Unmarshal(evDesc, &ev.Description); err != nil {
				return nil, fmt.Errorf("event description jsonb: %w", err)
			}
		}
		if err := json.Unmarshal(ttName, &tt.Name); err != nil {
			return nil, fmt.Errorf("ticket type name jsonb: %w", err)
		}
		// Public reads are cross-organizer; each row still carries its
		// owner so aggregates stay tenancy-complete (ADR-002).
		perf.OrganizerID = ev.OrganizerID
		perf.EventID = ev.ID
		perf.VenueID = venue.ID
		venue.OrganizerID = ev.OrganizerID
		tt.OrganizerID = ev.OrganizerID
		tt.PerformanceID = perf.ID

		agg, ok := byEvent[ev.ID]
		if !ok {
			agg = &EventAggregate{Event: ev}
			byEvent[ev.ID] = agg
			order = append(order, ev.ID)
		}
		ref, ok := perfSeen[perf.ID]
		if !ok {
			agg.Performances = append(agg.Performances, PerformanceAggregate{Performance: perf, Venue: venue})
			ref = perfRef{agg: agg, idx: len(agg.Performances) - 1}
			perfSeen[perf.ID] = ref
		}
		ref.agg.Performances[ref.idx].TicketTypes = append(ref.agg.Performances[ref.idx].TicketTypes, tt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("public read rows: %w", err)
	}
	out := make([]EventAggregate, 0, len(order))
	for _, id := range order {
		out = append(out, *byEvent[id])
	}
	return out, nil
}

func (p *Postgres) ListPublishedEvents(ctx context.Context) ([]EventAggregate, error) {
	return p.publicPerformances(ctx, nil)
}

func (p *Postgres) GetPublishedEvent(ctx context.Context, id uuid.UUID) (EventAggregate, error) {
	aggs, err := p.publicPerformances(ctx, &id)
	if err != nil {
		return EventAggregate{}, err
	}
	if len(aggs) == 0 {
		return EventAggregate{}, ErrNotFound
	}
	return aggs[0], nil
}
