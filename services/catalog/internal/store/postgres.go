package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationsFS embed.FS

// Migrate applies the embedded goose migrations. Every service owns and
// migrates its own database and fails fast (ADR-008). Since ADR-022 the caller
// is the binary's `migrate` subcommand, run as a one-shot job before the
// service starts — never the server path.
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

func isUniqueViolation(err error) bool {
	type coder interface{ SQLState() string }
	var c coder
	return errors.As(err, &c) && c.SQLState() == "23505"
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

func (p *Postgres) ListVenues(ctx context.Context, organizerID uuid.UUID) ([]Venue, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, name, ga_capacity, created_at
		   FROM venues
		  WHERE organizer_id = $1
		  ORDER BY name`, organizerID)
	if err != nil {
		return nil, fmt.Errorf("list venues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	venues := make([]Venue, 0)
	for rows.Next() {
		var v Venue
		if err := rows.Scan(&v.ID, &v.OrganizerID, &v.Name, &v.GACapacity, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan venue: %w", err)
		}
		venues = append(venues, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate venues: %w", err)
	}
	return venues, nil
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

func (p *Postgres) CreateSeries(ctx context.Context, in SeriesInput) (Series, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Series{}, err
	}
	var s Series
	err = p.db.QueryRowContext(ctx, `INSERT INTO series(organizer_id,event_id,name)
		SELECT $1,e.id,$3 FROM events e WHERE e.id=$2 AND e.organizer_id=$1
		RETURNING id,organizer_id,event_id,name,created_at`, in.OrganizerID, in.EventID, raw).
		Scan(&s.ID, &s.OrganizerID, &s.EventID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	}
	if err != nil {
		return Series{}, fmt.Errorf("create series: %w", err)
	}
	if err := json.Unmarshal(raw, &s.Name); err != nil {
		return Series{}, err
	}
	s.Members = []SeriesMember{}
	return s, nil
}

func getSeriesFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Series, error) {
	var s Series
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,event_id,name,created_at FROM series WHERE id=$1`, id).
		Scan(&s.ID, &s.OrganizerID, &s.EventID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	}
	if err != nil {
		return Series{}, err
	}
	if err := json.Unmarshal(raw, &s.Name); err != nil {
		return Series{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT performance_id,position FROM series_performances WHERE series_id=$1 ORDER BY position`, id)
	if err != nil {
		return Series{}, err
	}
	defer func() { _ = rows.Close() }()
	s.Members = []SeriesMember{}
	for rows.Next() {
		var m SeriesMember
		if err := rows.Scan(&m.PerformanceID, &m.Position); err != nil {
			return Series{}, err
		}
		s.Members = append(s.Members, m)
	}
	return s, rows.Err()
}

func (p *Postgres) AttachPerformanceToSeries(ctx context.Context, seriesID, performanceID uuid.UUID, position int32) (Series, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Series{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var orgID, eventID uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,event_id FROM series WHERE id=$1 FOR UPDATE`, seriesID).Scan(&orgID, &eventID); errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	} else if err != nil {
		return Series{}, err
	}
	var targetOrg, targetEvent uuid.UUID
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,event_id,status FROM performances WHERE id=$1 FOR UPDATE`, performanceID).Scan(&targetOrg, &targetEvent, &status); errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	} else if err != nil {
		return Series{}, err
	}
	if targetOrg != orgID || targetEvent != eventID {
		return Series{}, ErrOrganizerMismatch
	}
	if status != "draft" {
		return Series{}, ErrMembershipFrozen
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.status FROM series_performances sp JOIN performances p ON p.id=sp.performance_id WHERE sp.series_id=$1 ORDER BY p.id FOR UPDATE OF p`, seriesID)
	if err != nil {
		return Series{}, err
	}
	launched := false
	for rows.Next() {
		var memberStatus string
		if err = rows.Scan(&memberStatus); err != nil {
			_ = rows.Close()
			return Series{}, err
		}
		launched = launched || memberStatus != "draft"
	}
	if err = rows.Close(); err != nil {
		return Series{}, err
	}
	if err = rows.Err(); err != nil {
		return Series{}, err
	}
	if launched {
		return Series{}, ErrMembershipFrozen
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO series_performances(series_id,performance_id,position) VALUES($1,$2,$3)`, seriesID, performanceID, position); err != nil {
		if isUniqueViolation(err) {
			return Series{}, ErrMembershipConflict
		}
		return Series{}, err
	}
	s, err := getSeriesFrom(ctx, tx, seriesID)
	if err != nil {
		return Series{}, err
	}
	if err = tx.Commit(); err != nil {
		return Series{}, err
	}
	return s, nil
}

func (p *Postgres) CreateSeason(ctx context.Context, in SeasonInput) (Season, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Season{}, err
	}
	var s Season
	err = p.db.QueryRowContext(ctx, `INSERT INTO seasons(organizer_id,name) SELECT o.id,$2 FROM organizers o WHERE o.id=$1 RETURNING id,organizer_id,name,created_at`, in.OrganizerID, raw).Scan(&s.ID, &s.OrganizerID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, err
	}
	if err = json.Unmarshal(raw, &s.Name); err != nil {
		return Season{}, err
	}
	s.SeriesIDs = []uuid.UUID{}
	s.EventIDs = []uuid.UUID{}
	return s, nil
}

func getSeasonFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Season, error) {
	var s Season
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,name,created_at FROM seasons WHERE id=$1`, id).
		Scan(&s.ID, &s.OrganizerID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, err
	}
	if err = json.Unmarshal(raw, &s.Name); err != nil {
		return Season{}, err
	}
	s.SeriesIDs = []uuid.UUID{}
	s.EventIDs = []uuid.UUID{}
	for query, dest := range map[string]*[]uuid.UUID{
		`SELECT series_id FROM season_series WHERE season_id=$1 ORDER BY series_id`: &s.SeriesIDs,
		`SELECT event_id FROM season_events WHERE season_id=$1 ORDER BY event_id`:   &s.EventIDs,
	} {
		rows, queryErr := q.QueryContext(ctx, query, id)
		if queryErr != nil {
			return Season{}, queryErr
		}
		for rows.Next() {
			var memberID uuid.UUID
			if scanErr := rows.Scan(&memberID); scanErr != nil {
				_ = rows.Close()
				return Season{}, scanErr
			}
			*dest = append(*dest, memberID)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return Season{}, closeErr
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return Season{}, rowsErr
		}
	}
	return s, nil
}

func (p *Postgres) CreateFestival(ctx context.Context, in FestivalInput) (Festival, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Festival{}, err
	}
	var f Festival
	err = p.db.QueryRowContext(ctx, `INSERT INTO festivals(organizer_id,name,shared_capacity)
		SELECT o.id,$2,$3 FROM organizers o WHERE o.id=$1
		RETURNING id,organizer_id,name,shared_capacity,status,created_at`, in.OrganizerID, raw, in.SharedCapacity).
		Scan(&f.ID, &f.OrganizerID, &raw, &f.SharedCapacity, &f.Status, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	}
	if err != nil {
		return Festival{}, err
	}
	if err = json.Unmarshal(raw, &f.Name); err != nil {
		return Festival{}, err
	}
	f.MemberIDs = []uuid.UUID{}
	return f, nil
}

func getFestivalFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Festival, error) {
	var f Festival
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,name,shared_capacity,status,created_at FROM festivals WHERE id=$1`, id).
		Scan(&f.ID, &f.OrganizerID, &raw, &f.SharedCapacity, &f.Status, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	}
	if err != nil {
		return Festival{}, err
	}
	if err = json.Unmarshal(raw, &f.Name); err != nil {
		return Festival{}, err
	}
	f.MemberIDs = []uuid.UUID{}
	rows, err := q.QueryContext(ctx, `SELECT id FROM performances WHERE capacity_group_id=$1 ORDER BY id`, id)
	if err != nil {
		return Festival{}, err
	}
	for rows.Next() {
		var memberID uuid.UUID
		if err = rows.Scan(&memberID); err != nil {
			_ = rows.Close()
			return Festival{}, err
		}
		f.MemberIDs = append(f.MemberIDs, memberID)
	}
	if err = rows.Close(); err != nil {
		return Festival{}, err
	}
	if err = rows.Err(); err != nil {
		return Festival{}, err
	}
	return f, nil
}

func (p *Postgres) AttachDayToFestival(ctx context.Context, festivalID, performanceID uuid.UUID) (Festival, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Festival{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var festivalOrg uuid.UUID
	var festivalStatus string
	var sharedCapacity int32
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,status,shared_capacity FROM festivals WHERE id=$1 FOR UPDATE`, festivalID).
		Scan(&festivalOrg, &festivalStatus, &sharedCapacity); errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	} else if err != nil {
		return Festival{}, err
	}
	var performanceOrg uuid.UUID
	var kind, performanceStatus string
	var capacityGroup uuid.NullUUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,kind,status,capacity_group_id FROM performances WHERE id=$1 FOR UPDATE`, performanceID).
		Scan(&performanceOrg, &kind, &performanceStatus, &capacityGroup); errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	} else if err != nil {
		return Festival{}, err
	}
	if festivalOrg != performanceOrg {
		return Festival{}, ErrOrganizerMismatch
	}
	if kind != KindFestivalDay {
		return Festival{}, ErrSlotKindMismatch
	}
	if festivalStatus != "draft" {
		return Festival{}, ErrFestivalNotDraft
	}
	if performanceStatus != "draft" {
		return Festival{}, ErrMembershipFrozen
	}
	if capacityGroup.Valid {
		return Festival{}, ErrAlreadyGrouped
	}
	if _, err = tx.ExecContext(ctx, `UPDATE performances SET capacity_group_id=$1 WHERE id=$2`, festivalID, performanceID); err != nil {
		return Festival{}, err
	}
	f, err := getFestivalFrom(ctx, tx, festivalID)
	if err != nil {
		return Festival{}, err
	}
	if err = tx.Commit(); err != nil {
		return Festival{}, err
	}
	return f, nil
}

func (p *Postgres) AttachSeriesToSeason(ctx context.Context, seasonID, seriesID uuid.UUID) (Season, error) {
	return p.attachSeasonMember(ctx, seasonID, seriesID, true)
}
func (p *Postgres) AttachEventToSeason(ctx context.Context, seasonID, eventID uuid.UUID) (Season, error) {
	return p.attachSeasonMember(ctx, seasonID, eventID, false)
}
func (p *Postgres) attachSeasonMember(ctx context.Context, seasonID, memberID uuid.UUID, isSeries bool) (Season, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Season{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var seasonOrg, memberOrg uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id FROM seasons WHERE id=$1 FOR UPDATE`, seasonID).Scan(&seasonOrg); errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	} else if err != nil {
		return Season{}, err
	}
	table, col := "events", "event_id"
	if isSeries {
		table, col = "series", "series_id"
	}
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id FROM `+table+` WHERE id=$1`, memberID).Scan(&memberOrg); errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	} else if err != nil {
		return Season{}, err
	}
	if memberOrg != seasonOrg {
		return Season{}, ErrOrganizerMismatch
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO season_`+table+`(season_id,`+col+`) VALUES($1,$2)`, seasonID, memberID); err != nil {
		if isUniqueViolation(err) {
			return Season{}, ErrMembershipConflict
		}
		return Season{}, err
	}
	s, err := getSeasonFrom(ctx, tx, seasonID)
	if err != nil {
		return Season{}, err
	}
	if err = tx.Commit(); err != nil {
		return Season{}, err
	}
	return s, nil
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

	kind := in.Kind
	if kind == "" {
		kind = KindPerformance
	}
	// Seated validation (TKT-103): the referenced map must exist, share the
	// performance's organizer AND venue, and be published — a slot may only be
	// seated against a published version. Seating is orthogonal to `kind`
	// (ADR-005), but a grouped festival day is shared-capacity GA by definition,
	// so a seat-map reference on one is contradictory and refused here. The check
	// mirrors the tenancy-by-scoped-query pattern the AddSeatMap* writes use.
	if in.SeatMapID != nil {
		if kind == KindFestivalDay {
			return Performance{}, fmt.Errorf("festival day cannot be seated: %w", ErrIllegalTransition)
		}
		var mapOrg, mapVenue uuid.UUID
		var mapStatus string
		err = p.db.QueryRowContext(ctx,
			`SELECT organizer_id, venue_id, status FROM seat_maps WHERE id = $1`, *in.SeatMapID).
			Scan(&mapOrg, &mapVenue, &mapStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return Performance{}, fmt.Errorf("seat map: %w", ErrNotFound)
		}
		if err != nil {
			return Performance{}, fmt.Errorf("lookup seat map: %w", err)
		}
		if mapOrg != in.OrganizerID || mapVenue != in.VenueID {
			return Performance{}, ErrOrganizerMismatch
		}
		if mapStatus != "published" {
			return Performance{}, ErrSeatMapNotPublished
		}
	}
	mode := in.ReEntry.Mode
	if mode == "" {
		mode = "single"
	}
	var id uuid.UUID
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO performances
		   (organizer_id, event_id, venue_id, kind, starts_at, operating_date,
		    opens_at, closes_at, timezone, re_entry_mode, max_entries, requires_exit, seat_map_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING id`,
		in.OrganizerID, in.EventID, in.VenueID, kind, in.StartsAt, in.OperatingDate,
		in.OpensAt, in.ClosesAt, in.Timezone, mode, in.ReEntry.MaxEntries, in.ReEntry.RequiresExit, in.SeatMapID).
		Scan(&id)
	if err != nil {
		return Performance{}, fmt.Errorf("insert performance: %w", err)
	}
	// Re-read through the canonical projection so every attribute (and the
	// venue capacity snapshot) is populated exactly as reads see it.
	perf, _, _, err := p.getPerformance(ctx, id)
	if err != nil {
		return Performance{}, err
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
		   AND capacity_group_id IS NULL
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
		if perf.CapacityGroupID != nil {
			return Performance{}, false, ErrGroupedSlotLifecycle
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

// rowQueryer is satisfied by both *sql.DB and *sql.Tx, so getPerformance can
// read either outside a transaction or inside the archive lock.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (p *Postgres) getPerformance(ctx context.Context, id uuid.UUID) (Performance, *sql.NullTime, *sql.NullTime, error) {
	return p.getPerformanceFrom(ctx, p.db, id)
}

// getPerformanceTx reads the row through the open transaction, so it observes
// the just-applied archive UPDATE and keeps the FOR UPDATE lock held.
func (p *Postgres) getPerformanceTx(ctx context.Context, tx *sql.Tx, id uuid.UUID) (Performance, *sql.NullTime, *sql.NullTime, error) {
	return p.getPerformanceFrom(ctx, tx, id)
}

// performanceColumns is the projection shared by every full-row Performance read
// (single-row get and the backfill's list). Both hydrate through scanPerformance,
// so the publication-time Capacity/SharedCapacity snapshot the backfill re-emits
// is byte-identical to what live publish emitted (TKT-96 COS-3 premise). The
// column ORDER is load-bearing — it must match scanPerformance's Scan targets.
const performanceColumns = `p.id, p.organizer_id, p.event_id, p.venue_id, p.kind, p.starts_at,
	        p.operating_date, p.opens_at, p.closes_at, p.timezone,
	        p.re_entry_mode, p.max_entries, p.requires_exit,
	        p.closure_status, p.closed_at, p.closure_reason, p.closure_version,
	        p.closure_changed_at, p.capacity_group_id, p.status, p.published_at, p.archived_at,
	        p.event_emitted_at, p.archive_emitted_at, p.created_at, v.ga_capacity,
	        f.shared_capacity, p.seat_map_id`

const performanceFrom = `FROM performances p JOIN venues v ON v.id = p.venue_id
	 LEFT JOIN festivals f ON f.id = p.capacity_group_id`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPerformance hydrates one performanceColumns row into a Performance and its
// two owed-event timestamps. Sole hydration path for full rows, so the get and
// list callers can never drift on how a row becomes a Performance.
func scanPerformance(s rowScanner) (Performance, *sql.NullTime, *sql.NullTime, error) {
	var perf Performance
	var (
		emitted, archiveEmitted        sql.NullTime
		opensAt, closesAt, closeReason sql.NullString
		maxEntries                     sql.NullInt32
		capacityGroup                  uuid.NullUUID
		sharedCapacity                 sql.NullInt32
		seatMap                        uuid.NullUUID
	)
	err := s.Scan(&perf.ID, &perf.OrganizerID, &perf.EventID, &perf.VenueID, &perf.Kind, &perf.StartsAt,
		&perf.OperatingDate, &opensAt, &closesAt, &perf.Timezone,
		&perf.ReEntry.Mode, &maxEntries, &perf.ReEntry.RequiresExit,
		&perf.Closure.Status, &perf.Closure.ClosedAt, &closeReason, &perf.Closure.Version,
		&perf.Closure.ChangedAt, &capacityGroup, &perf.Status, &perf.PublishedAt, &perf.ArchivedAt, &emitted,
		&archiveEmitted, &perf.CreatedAt, &perf.Capacity, &sharedCapacity, &seatMap)
	if err != nil {
		return Performance{}, nil, nil, err
	}
	if seatMap.Valid {
		perf.SeatMapID = &seatMap.UUID
	}
	if opensAt.Valid {
		perf.OpensAt = &opensAt.String
	}
	if closesAt.Valid {
		perf.ClosesAt = &closesAt.String
	}
	if maxEntries.Valid {
		perf.ReEntry.MaxEntries = &maxEntries.Int32
	}
	if closeReason.Valid {
		perf.Closure.Reason = &closeReason.String
	}
	if capacityGroup.Valid {
		perf.CapacityGroupID = &capacityGroup.UUID
	}
	if sharedCapacity.Valid {
		perf.SharedCapacity = &sharedCapacity.Int32
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

func (p *Postgres) getPerformanceFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Performance, *sql.NullTime, *sql.NullTime, error) {
	perf, emittedPtr, archiveEmittedPtr, err := scanPerformance(
		q.QueryRowContext(ctx, `SELECT `+performanceColumns+` `+performanceFrom+` WHERE p.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, nil, nil, fmt.Errorf("performance: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, nil, nil, fmt.Errorf("get performance: %w", err)
	}
	return perf, emittedPtr, archiveEmittedPtr, nil
}

// ListPublishedUngroupedPerformances returns fully-hydrated published, ungrouped
// performances for the re_entry re-emission backfill (TKT-96), keyset-paginated
// by id (cursor is exclusive; nil starts at the beginning). Grouped festival-day
// members (capacity_group_id NOT NULL) are excluded in the predicate, not
// post-filtered: re-emitting them would assert festival-shared capacity from a
// per-member backfill, the aggregate-ownership inversion ADR-018 rule 2 prevents.
//
// This is a one-shot operator backfill, not a hot read path (ADR-019): a
// sequential scan over published slots is acceptable and no index is added for
// it. The id keyset only bounds the batch; ordering, not index-backing, is its job.
func (p *Postgres) ListPublishedUngroupedPerformances(ctx context.Context, after *uuid.UUID, limit int) ([]Performance, error) {
	if limit <= 0 {
		limit = 100
	}
	args := []any{limit}
	cursor := ""
	if after != nil {
		cursor = "AND p.id > $2"
		args = append(args, *after)
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+performanceColumns+` `+performanceFrom+`
		 WHERE p.status = 'published' AND p.capacity_group_id IS NULL `+cursor+`
		 ORDER BY p.id LIMIT $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("list published ungrouped performances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Performance
	for rows.Next() {
		perf, _, _, scanErr := scanPerformance(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan performance: %w", scanErr)
		}
		out = append(out, perf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate performances: %w", err)
	}
	return out, nil
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

// GetPoolOfferState answers the reconciliation read (TKT-90): the id is tried as
// a performance in any lifecycle first, then as a festival. Only a miss on both
// is ErrNotFound — inventory treats that as a non-positive answer and writes
// nothing, so this method must never collapse "festival" into "not found".
func (p *Postgres) GetPoolOfferState(ctx context.Context, id uuid.UUID) (PoolOfferState, error) {
	perf, _, _, err := p.getPerformance(ctx, id)
	if err == nil {
		return PoolOfferState{Kind: "performance", Lifecycle: perf.Status, Closure: perf.Closure}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PoolOfferState{}, err
	}
	var one int
	err = p.db.QueryRowContext(ctx, `SELECT 1 FROM festivals WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return PoolOfferState{}, fmt.Errorf("pool offer state: %w", ErrNotFound)
	}
	if err != nil {
		return PoolOfferState{}, fmt.Errorf("pool offer state: %w", err)
	}
	return PoolOfferState{Kind: "festival"}, nil
}

// ArchivePerformance flips published->archived, deciding and transitioning
// inside one transaction with the row locked FOR UPDATE. The lock closes the
// check-then-act race: a concurrent publish cannot commit between reading the
// status and applying the transition, so archive can never emit an archive
// event for a row that is still published (nor derive a second, nil-timestamp
// archive id). draft is rejected; already-archived is idempotent.
func (p *Postgres) ArchivePerformance(ctx context.Context, id uuid.UUID) (Performance, bool, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Performance{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	var capacityGroup uuid.NullUUID
	var closureVersion, closureEmitted int32
	err = tx.QueryRowContext(ctx,
		`SELECT status, capacity_group_id, closure_version, closure_emitted_version
		 FROM performances WHERE id = $1 FOR UPDATE`, id).Scan(&status, &capacityGroup, &closureVersion, &closureEmitted)
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, false, false, fmt.Errorf("performance: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, false, false, fmt.Errorf("lock performance: %w", err)
	}
	if capacityGroup.Valid {
		return Performance{}, false, false, ErrGroupedSlotLifecycle
	}
	switch status {
	case "draft":
		// Only published offers archive (draft->archived is illegal).
		return Performance{}, false, false, ErrIllegalTransition
	case "published":
		// A closed slot can be archived (spike §Case 3) — but not while its
		// closed/reopened event is still owed: archiving strands the slot in a
		// terminal state where the closure toggle can no longer re-emit it, so
		// the event would be lost. Refuse until it is emitted (retry close/reopen).
		if closureEmitted < closureVersion {
			return Performance{}, false, false, ErrClosurePending
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE performances SET status = 'archived', archived_at = now() WHERE id = $1`, id); err != nil {
			return Performance{}, false, false, fmt.Errorf("archive: %w", err)
		}
	case "archived":
		// Idempotent: leave the row (and its archived_at) untouched.
	default:
		return Performance{}, false, false, ErrIllegalTransition
	}

	perf, publishedEmitted, archiveEmitted, err := p.getPerformanceTx(ctx, tx, id)
	if err != nil {
		return Performance{}, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return Performance{}, false, false, fmt.Errorf("commit archive: %w", err)
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

func (p *Postgres) CloseSlot(ctx context.Context, id uuid.UUID, reason *string) (Performance, bool, bool, error) {
	return p.toggleClosure(ctx, id, "closed", reason)
}

func (p *Postgres) ReopenSlot(ctx context.Context, id uuid.UUID) (Performance, bool, bool, error) {
	return p.toggleClosure(ctx, id, "open", nil)
}

// toggleClosure flips the orthogonal closure attribute under a FOR UPDATE lock
// (same race discipline as archive). Closure is only meaningful while
// published. Each real transition bumps closure_version and stamps
// closure_changed_at (so the event occurred_at is stable across retries); the
// returned closureNeedsEmit says whether that version's domain event is still
// owed. publishNeedsEmit reports whether the publication event is still owed:
// the caller emits publication first, so a closure never overtakes the
// publication of the same slot. Re-requesting the current state re-emits only
// if owed (safe: deterministic id de-duplicates). The opposite toggle is
// refused while the current version's event is still owed (ErrClosurePending),
// so the single closure_emitted_version marker can never silently drop one.
func (p *Postgres) toggleClosure(ctx context.Context, id uuid.UUID, target string, reason *string) (Performance, bool, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Performance{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, closureStatus string
	var version, emittedVersion int32
	err = tx.QueryRowContext(ctx,
		`SELECT status, closure_status, closure_version, closure_emitted_version
		 FROM performances WHERE id = $1 FOR UPDATE`, id).
		Scan(&status, &closureStatus, &version, &emittedVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Performance{}, false, false, fmt.Errorf("performance: %w", ErrNotFound)
	}
	if err != nil {
		return Performance{}, false, false, fmt.Errorf("lock performance: %w", err)
	}
	if status != "published" {
		// A closure attribute is only meaningful on a live (published) slot;
		// draft/archived slots have nothing to close (spike §Case 3).
		return Performance{}, false, false, ErrIllegalTransition
	}

	if closureStatus == target {
		// Already in the requested state: no transition. Re-emit only if this
		// version's event is still owed (a prior emission failed to ack).
		perf, publishedEmitted, _, err := p.getPerformanceTx(ctx, tx, id)
		if err != nil {
			return Performance{}, false, false, err
		}
		if err := tx.Commit(); err != nil {
			return Performance{}, false, false, fmt.Errorf("commit closure: %w", err)
		}
		return perf, publishedEmitted == nil, emittedVersion < version, nil
	}

	// A real transition is requested. Refuse it while the current version's
	// event is still owed — toggling now would lose that event under the
	// single marker. The caller retries the pending transition first.
	if emittedVersion < version {
		return Performance{}, false, false, ErrClosurePending
	}

	newVersion := version + 1
	if target == "closed" {
		_, err = tx.ExecContext(ctx,
			`UPDATE performances SET closure_status = 'closed', closed_at = now(),
			        closure_reason = $2, closure_version = $3, closure_changed_at = now() WHERE id = $1`,
			id, reason, newVersion)
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE performances SET closure_status = 'open', closed_at = NULL,
			        closure_reason = NULL, closure_version = $2, closure_changed_at = now() WHERE id = $1`,
			id, newVersion)
	}
	if err != nil {
		return Performance{}, false, false, fmt.Errorf("toggle closure: %w", err)
	}
	perf, publishedEmitted, _, err := p.getPerformanceTx(ctx, tx, id)
	if err != nil {
		return Performance{}, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return Performance{}, false, false, fmt.Errorf("commit closure: %w", err)
	}
	return perf, publishedEmitted == nil, true, nil
}

// MarkClosureEmitted advances the closure outbox marker to version (monotonic,
// idempotent): the closed/reopened event for that version has been ack'd.
func (p *Postgres) MarkClosureEmitted(ctx context.Context, id uuid.UUID, version int32) error {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE performances SET closure_emitted_version = $2
		 WHERE id = $1 AND closure_emitted_version < $2`, id, version); err != nil {
		return fmt.Errorf("mark closure emitted: %w", err)
	}
	return nil
}

func (p *Postgres) PublishSeries(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionSeries(ctx, id, "published")
}

func (p *Postgres) ArchiveSeries(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionSeries(ctx, id, "archived")
}

func (p *Postgres) transitionSeries(ctx context.Context, id uuid.UUID, target string) ([]SeriesTransition, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedID uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT id FROM series WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	type member struct {
		id               uuid.UUID
		status           string
		version, emitted int32
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.status,p.closure_version,p.closure_emitted_version
		FROM series_performances sp JOIN performances p ON p.id=sp.performance_id
		WHERE sp.series_id=$1 ORDER BY p.id FOR UPDATE OF p`, id)
	if err != nil {
		return nil, err
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.id, &m.status, &m.version, &m.emitted); err != nil {
			_ = rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrEmptySeries
	}
	for _, m := range members {
		switch target {
		case "published":
			if m.status == "archived" {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "archived member cannot be published", Cause: ErrIllegalTransition}
			}
			if m.status == "draft" {
				var sellable bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_types WHERE performance_id=$1)`, m.id).Scan(&sellable); err != nil {
					return nil, err
				}
				if !sellable {
					return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "member has no ticket type", Cause: ErrNotSellable}
				}
			}
		case "archived":
			if m.status == "draft" {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "draft member cannot be archived", Cause: ErrIllegalTransition}
			}
			if m.status == "published" && m.emitted < m.version {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "member has an owed closure event", Cause: ErrClosurePending}
			}
		}
	}
	for _, m := range members {
		if target == "published" && m.status == "draft" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='published',published_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
		if target == "archived" && m.status == "published" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='archived',archived_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
	}
	out := make([]SeriesTransition, 0, len(members))
	for _, m := range members {
		perf, pubMark, archiveMark, e := p.getPerformanceTx(ctx, tx, m.id)
		if e != nil {
			return nil, e
		}
		out = append(out, SeriesTransition{Performance: perf, PublishNeedsEmit: pubMark == nil, ArchiveNeedsEmit: target == "archived" && archiveMark == nil})
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Postgres) PublishFestival(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionFestival(ctx, id, "published")
}

func (p *Postgres) ArchiveFestival(ctx context.Context, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionFestival(ctx, id, "archived")
}

// transitionFestival keeps the group status, every member transition and the
// owed-event snapshots in one row-locked decision. Emission happens only after
// this transaction commits in the API layer.
func (p *Postgres) transitionFestival(ctx context.Context, id uuid.UUID, target string) ([]SeriesTransition, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var festivalStatus string
	var sharedCapacity int32
	if err = tx.QueryRowContext(ctx, `SELECT status,shared_capacity FROM festivals WHERE id=$1 FOR UPDATE`, id).
		Scan(&festivalStatus, &sharedCapacity); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	type member struct {
		id               uuid.UUID
		status           string
		version, emitted int32
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.status,p.closure_version,p.closure_emitted_version
		FROM performances p WHERE p.capacity_group_id=$1 ORDER BY p.id FOR UPDATE OF p`, id)
	if err != nil {
		return nil, err
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.id, &m.status, &m.version, &m.emitted); err != nil {
			_ = rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrEmptyFestival
	}
	for _, m := range members {
		switch target {
		case "published":
			if m.status == "archived" {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "archived member cannot be published", Cause: ErrIllegalTransition}
			}
			if m.status == "draft" {
				var sellable bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_types WHERE performance_id=$1)`, m.id).Scan(&sellable); err != nil {
					return nil, err
				}
				if !sellable {
					return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "member has no ticket type", Cause: ErrNotSellable}
				}
			}
		case "archived":
			if m.status == "draft" {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "draft member cannot be archived", Cause: ErrIllegalTransition}
			}
			if m.status == "published" && m.emitted < m.version {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "member has an owed closure event", Cause: ErrClosurePending}
			}
		}
	}
	for _, m := range members {
		if target == "published" && m.status == "draft" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='published',published_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
		if target == "archived" && m.status == "published" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='archived',archived_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
	}
	if festivalStatus != target {
		if _, err = tx.ExecContext(ctx, `UPDATE festivals SET status=$2 WHERE id=$1`, id, target); err != nil {
			return nil, err
		}
	}
	out := make([]SeriesTransition, 0, len(members))
	for _, m := range members {
		perf, pubMark, archiveMark, getErr := p.getPerformanceTx(ctx, tx, m.id)
		if getErr != nil {
			return nil, getErr
		}
		perf.SharedCapacity = &sharedCapacity
		out = append(out, SeriesTransition{Performance: perf, PublishNeedsEmit: pubMark == nil, ArchiveNeedsEmit: target == "archived" && archiveMark == nil})
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// A day kind has no starts_at instant; derive a representative one from its
// operating window (opening moment, resolved in the slot's local zone) so
// the public read has a stable non-null sort/display key for every kind.
// For kind 'performance' the COALESCE returns starts_at unchanged.
const publicPerformancesStartsAt = `COALESCE(p.starts_at,
	(p.operating_date + p.opens_at::time) AT TIME ZONE p.timezone)`

// The scoped and unscoped public reads are separate SQL texts sharing every
// fragment except their predicate — deliberately, rather than one text with a
// nullable scope parameter (TKT-63).
//
// A `($1::uuid[] IS NULL OR e.id = ANY($1))` predicate has to be planned for a
// NULL $1 as well, so under plan_cache_mode = force_generic_plan the planner
// cannot use events_pkey and falls back to scanning events. Filtering
// unconditionally keeps the index in every plan mode. Production runs the
// default auto mode, where both shapes index-scan and the season read is
// correctly scoped either way — this is robustness against a mode nothing here
// sets, not a live fix (ADR-019).
//
// The predicates are consts because the EXPLAIN smoke test asserts the plan of
// the predicate this file ships rather than a retyped copy of it; the rest of
// the query text is still duplicated there (TKT-65).
const (
	publicPerformancesUnscopedPredicate = `p.status = 'published'`
	publicPerformancesScopedPredicate   = `p.status = 'published' AND e.id = ANY($1::uuid[])`
)

const publicPerformancesSelect = `
	SELECT e.id, e.organizer_id, e.name, e.description, e.created_at,
	       p.id, ` + publicPerformancesStartsAt + `, p.timezone, p.kind, p.capacity_group_id, p.seat_map_id, p.status, p.published_at, p.created_at,
	       v.id, v.name, v.ga_capacity, v.created_at,
	       t.id, t.name, t.price_amount, t.currency, t.created_at,
	       s.id, s.name, sp.position, s.created_at
	FROM performances p
	JOIN events e ON e.id = p.event_id
	JOIN venues v ON v.id = p.venue_id
	JOIN ticket_types t ON t.performance_id = p.id
	LEFT JOIN series_performances sp ON sp.performance_id = p.id
	LEFT JOIN series s ON s.id = sp.series_id
	WHERE `

const publicPerformancesOrder = `
	ORDER BY ` + publicPerformancesStartsAt + `, p.id, t.price_amount, t.id`

const (
	unscopedPublicPerformancesQuery = publicPerformancesSelect + publicPerformancesUnscopedPredicate + publicPerformancesOrder
	scopedPublicPerformancesQuery   = publicPerformancesSelect + publicPerformancesScopedPredicate + publicPerformancesOrder
)

// publicPerformances returns the publicly listable slots (published AND
// priced — no sellable offer, no listing) grouped into event aggregates,
// events ordered by their earliest slot, slots by start time.
//
// eventIDs scopes the read: nil means every published event (the catalog
// listing), otherwise only those events. A non-nil *empty* slice means no
// events and returns nothing — callers must not collapse it to nil, or a
// caller with zero events would get the whole catalog back. The scoped path
// is index-backed by performances_by_event (TKT-60): a season read must cost
// what its own events cost, not what the catalog costs (ADR-004).
//
// That nil/empty distinction used to live in the SQL's `$1 IS NULL` branch; since
// TKT-63 split the query it lives only in the routing below, which is why it
// tests eventIDs == nil and never len(eventIDs) == 0.
func (p *Postgres) publicPerformances(ctx context.Context, eventIDs []uuid.UUID) ([]EventAggregate, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if eventIDs == nil {
		rows, err = p.db.QueryContext(ctx, unscopedPublicPerformancesQuery)
	} else {
		rows, err = p.db.QueryContext(ctx, scopedPublicPerformancesQuery, eventIDs)
	}
	if err != nil {
		return nil, fmt.Errorf("public read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type perfRef struct {
		agg *EventAggregate
		idx int
	}
	var (
		order          []uuid.UUID
		byEvent        = map[uuid.UUID]*EventAggregate{}
		perfSeen       = map[uuid.UUID]perfRef{}
		seriesByID     = map[uuid.UUID]*SeriesAggregate{}
		seriesPosition = map[uuid.UUID]map[uuid.UUID]int32{}
	)
	for rows.Next() {
		var (
			ev            Event
			perf          Performance
			venue         Venue
			tt            TicketType
			evName        []byte
			evDesc        []byte
			ttName        []byte
			startsAt      time.Time // COALESCE'd, never null on the public path
			seriesID      uuid.NullUUID
			seriesName    []byte
			seriesPos     sql.NullInt32
			seriesCreated sql.NullTime
			capacityGroup uuid.NullUUID
			seatMap       uuid.NullUUID
		)
		if err := rows.Scan(
			&ev.ID, &ev.OrganizerID, &evName, &evDesc, &ev.CreatedAt,
			&perf.ID, &startsAt, &perf.Timezone, &perf.Kind, &capacityGroup, &seatMap, &perf.Status, &perf.PublishedAt, &perf.CreatedAt,
			&venue.ID, &venue.Name, &venue.GACapacity, &venue.CreatedAt,
			&tt.ID, &ttName, &tt.PriceAmount, &tt.Currency, &tt.CreatedAt,
			&seriesID, &seriesName, &seriesPos, &seriesCreated,
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
		perf.StartsAt = &startsAt
		if capacityGroup.Valid {
			perf.CapacityGroupID = &capacityGroup.UUID
		}
		// TKT-172: the public detail names the seated slot's map version, so a
		// storefront can tell seated from GA. Nil for a GA slot, and the two are
		// mutually exclusive with CapacityGroupID by construction (a festival day
		// cannot be seated — CreatePerformance refuses it).
		if seatMap.Valid {
			perf.SeatMapID = &seatMap.UUID
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
		if seriesID.Valid {
			sa, exists := seriesByID[seriesID.UUID]
			if !exists {
				var name LocalizedText
				if err := json.Unmarshal(seriesName, &name); err != nil {
					return nil, fmt.Errorf("series name jsonb: %w", err)
				}
				sa = &SeriesAggregate{Series: Series{ID: seriesID.UUID, OrganizerID: ev.OrganizerID, EventID: ev.ID, Name: name, CreatedAt: seriesCreated.Time}}
				seriesByID[seriesID.UUID] = sa
				seriesPosition[seriesID.UUID] = map[uuid.UUID]int32{}
				agg.Series = append(agg.Series, *sa)
			}
			if _, exists := seriesPosition[seriesID.UUID][perf.ID]; !exists {
				seriesPosition[seriesID.UUID][perf.ID] = seriesPos.Int32
				sa.PerformanceIDs = append(sa.PerformanceIDs, perf.ID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("public read rows: %w", err)
	}
	out := make([]EventAggregate, 0, len(order))
	for _, id := range order {
		for i := range byEvent[id].Series {
			sa := seriesByID[byEvent[id].Series[i].Series.ID]
			sort.Slice(sa.PerformanceIDs, func(a, b int) bool {
				return seriesPosition[sa.Series.ID][sa.PerformanceIDs[a]] < seriesPosition[sa.Series.ID][sa.PerformanceIDs[b]]
			})
			byEvent[id].Series[i] = *sa
		}
		out = append(out, *byEvent[id])
	}
	return out, nil
}

func (p *Postgres) ListPublishedEvents(ctx context.Context) ([]EventAggregate, error) {
	return p.publicPerformances(ctx, nil)
}

func (p *Postgres) GetPublishedEvent(ctx context.Context, id uuid.UUID) (EventAggregate, error) {
	aggs, err := p.publicPerformances(ctx, []uuid.UUID{id})
	if err != nil {
		return EventAggregate{}, err
	}
	if len(aggs) == 0 {
		return EventAggregate{}, ErrNotFound
	}
	return aggs[0], nil
}

func (p *Postgres) GetPublishedSeason(ctx context.Context, id uuid.UUID) (SeasonAggregate, error) {
	season, err := getSeasonFrom(ctx, p.db, id)
	if err != nil {
		return SeasonAggregate{}, err
	}
	ids := map[uuid.UUID]bool{}
	for _, eventID := range season.EventIDs {
		ids[eventID] = true
	}
	rows, err := p.db.QueryContext(ctx, `SELECT s.event_id FROM season_series ss JOIN series s ON s.id=ss.series_id WHERE ss.season_id=$1`, id)
	if err != nil {
		return SeasonAggregate{}, err
	}
	for rows.Next() {
		var eventID uuid.UUID
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return SeasonAggregate{}, err
		}
		ids[eventID] = true
	}
	if err := rows.Close(); err != nil {
		return SeasonAggregate{}, err
	}
	// Read only this season's events. Passing the id set (never nil — an empty
	// set must stay empty, not widen to the whole catalog) keeps the read's cost
	// proportional to the season, per ADR-004.
	scoped := make([]uuid.UUID, 0, len(ids))
	for eventID := range ids {
		scoped = append(scoped, eventID)
	}
	events, err := p.publicPerformances(ctx, scoped)
	if err != nil {
		return SeasonAggregate{}, err
	}
	if len(events) == 0 {
		return SeasonAggregate{}, ErrNotFound
	}
	return SeasonAggregate{Season: season, Events: events}, nil
}

// publishedFestivalPerformancesQuery reads one festival's own published, priced
// festival_day slots + venue + ticket types, ordered chronologically by the day's
// opening instant.
//
// Scoped to the group, never the catalog (ADR-004: a single festival read must not
// scale with all published inventory), and that scoping is only real because
// performances_capacity_group_idx (0006) backs `capacity_group_id` — ADR-019 rule 1:
// a filter with no index behind it is a sequential scan wearing a WHERE clause.
//
// It is a const rather than a literal inside GetPublishedFestival so the plan
// assertion can EXPLAIN the statement production actually executes instead of a
// retyped copy of it (ADR-019 rule 2, TKT-65). Only one shape exists here — there is
// no scoped/unscoped split — so it needs none of publicPerformances' composed
// fragments. Unlike that read, `capacity_group_id = $1` is an unconditional scalar
// equality: it keeps its index under a generic plan as well as a custom one, which
// TestGetPublishedFestivalIsIndexScoped asserts.
const publishedFestivalPerformancesQuery = `
	SELECT p.id, ` + publicPerformancesStartsAt + `, p.timezone, p.kind,
	       v.id, v.name, v.ga_capacity, v.created_at,
	       t.id, t.name, t.price_amount, t.currency, t.created_at
	FROM performances p
	JOIN venues v ON v.id = p.venue_id
	JOIN ticket_types t ON t.performance_id = p.id
	WHERE p.capacity_group_id = $1 AND p.status = 'published'
	ORDER BY ` + publicPerformancesStartsAt + `, p.id, t.price_amount, t.id`

func (p *Postgres) GetPublishedFestival(ctx context.Context, id uuid.UUID) (FestivalAggregate, error) {
	festival, err := getFestivalFrom(ctx, p.db, id)
	if err != nil {
		return FestivalAggregate{}, err
	}
	if festival.Status != "published" {
		return FestivalAggregate{}, ErrNotFound
	}
	rows, err := p.db.QueryContext(ctx, publishedFestivalPerformancesQuery, id)
	if err != nil {
		return FestivalAggregate{}, fmt.Errorf("festival read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := FestivalAggregate{Festival: festival, Performances: []PerformanceAggregate{}}
	type perfRef struct{ idx int }
	seen := map[uuid.UUID]perfRef{}
	for rows.Next() {
		var (
			perf     Performance
			venue    Venue
			tt       TicketType
			startsAt time.Time
			ttName   []byte
		)
		if err := rows.Scan(
			&perf.ID, &startsAt, &perf.Timezone, &perf.Kind,
			&venue.ID, &venue.Name, &venue.GACapacity, &venue.CreatedAt,
			&tt.ID, &ttName, &tt.PriceAmount, &tt.Currency, &tt.CreatedAt,
		); err != nil {
			return FestivalAggregate{}, fmt.Errorf("scan festival read: %w", err)
		}
		if err := json.Unmarshal(ttName, &tt.Name); err != nil {
			return FestivalAggregate{}, fmt.Errorf("ticket type name jsonb: %w", err)
		}
		perf.StartsAt = &startsAt
		perf.Status = "published"
		perf.OrganizerID = festival.OrganizerID
		perf.CapacityGroupID = &id
		perf.VenueID = venue.ID
		venue.OrganizerID = festival.OrganizerID
		tt.OrganizerID = festival.OrganizerID
		tt.PerformanceID = perf.ID

		ref, ok := seen[perf.ID]
		if !ok {
			out.Performances = append(out.Performances, PerformanceAggregate{Performance: perf, Venue: venue})
			ref = perfRef{idx: len(out.Performances) - 1}
			seen[perf.ID] = ref
		}
		out.Performances[ref.idx].TicketTypes = append(out.Performances[ref.idx].TicketTypes, tt)
	}
	if err := rows.Err(); err != nil {
		return FestivalAggregate{}, fmt.Errorf("festival read rows: %w", err)
	}
	if len(out.Performances) == 0 {
		return FestivalAggregate{}, ErrNotFound
	}
	return out, nil
}

// --- Seat-map authoring (US-019 / TKT-102), draft-only. ---
//
// Each write is one INSERT ... SELECT that resolves the parent chain and
// requires the owning map to be draft, so cross-map/cross-organizer parentage
// and writes to a non-draft map are unrepresentable through the store: a
// no-match yields ErrNotFound; any UNIQUE collision yields ErrSeatMapConflict.

func (p *Postgres) CreateSeatMap(ctx context.Context, in SeatMapInput) (SeatMap, error) {
	m := SeatMap{OrganizerID: in.OrganizerID, VenueID: in.VenueID, Name: in.Name}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_maps (organizer_id, venue_id, name, orphan_prevention_enabled)
		 SELECT $1, v.id, $3, $4 FROM venues v WHERE v.id = $2 AND v.organizer_id = $1
		 RETURNING id, version, status, created_at, orphan_prevention_enabled`,
		in.OrganizerID, in.VenueID, in.Name, in.OrphanPreventionEnabled).
		Scan(&m.ID, &m.Version, &m.Status, &m.CreatedAt, &m.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMap{}, fmt.Errorf("venue: %w", ErrNotFound)
	}
	if err != nil {
		return SeatMap{}, fmt.Errorf("create seat map: %w", err)
	}
	return m, nil
}

// PublishSeatMap flips a seat map draft->published (TKT-103, COS-1). The
// transition is MONOTONIC and therefore lock-free (ADR-018 rule 1): a single
// atomic conditional UPDATE flips draft->published exactly once, so no row lock
// is needed — a concurrent transition cannot invalidate the publication's
// identity, exactly as PublishPerformance. needsEmit is true while the
// seat_map.published domain event is still owed (event_emitted_at is null);
// the caller emits, then marks. Publishing an already-published map is a
// resource no-op that still reports whether the event is owed.
func (p *Postgres) PublishSeatMap(ctx context.Context, id uuid.UUID) (SeatMap, bool, error) {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE seat_maps SET status = 'published', published_at = now()
		 WHERE id = $1 AND status = 'draft'`, id); err != nil {
		return SeatMap{}, false, fmt.Errorf("publish seat map: %w", err)
	}
	var m SeatMap
	var publishedAt sql.NullTime
	var emittedAt sql.NullTime
	err := p.db.QueryRowContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, published_at, event_emitted_at, created_at, orphan_prevention_enabled
		 FROM seat_maps WHERE id = $1`, id).
		Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &publishedAt, &emittedAt, &m.CreatedAt, &m.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMap{}, false, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("read seat map: %w", err)
	}
	if publishedAt.Valid {
		m.PublishedAt = &publishedAt.Time
	}
	if m.Status != "published" {
		// A map that could not flip and is not already published cannot be a
		// draft that just published, so it is an illegal transition target
		// (e.g. archived). Draft that already flipped falls through as published.
		return SeatMap{}, false, ErrIllegalTransition
	}
	return m, !emittedAt.Valid, nil
}

// MarkSeatMapEventEmitted records that the seat_map.published event for this map
// has been acknowledged by the stream (TKT-103). Mirrors
// MarkPerformanceEventEmitted: at-least-once, so a retry before this lands
// re-emits under the same deterministic id.
func (p *Postgres) MarkSeatMapEventEmitted(ctx context.Context, id uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE seat_maps SET event_emitted_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark seat map emitted: %w", err)
	}
	return nil
}

// lockCurrentPublishedVersion serializes all edit/pin/unpin work on a seat-map
// FAMILY, then resolves the family's current published version. EditSeatMap and
// PinSeat both call it, so a concurrent edit and pin serialize (TKT-104): the
// loser sees the winner's committed result. ErrNotFound if no version of the
// family is published (or the id/organizer does not match).
//
// It must NOT serialize by locking the current-version *row* with FOR UPDATE.
// EditSeatMap makes a new version by INSERTing a new row, which never conflicts
// with a FOR UPDATE lock on the old row — so a PinSeat blocked on the old row
// would, once the edit commits, still hold the STALE old row (PostgreSQL rechecks
// the locked row, it does not re-run the ORDER BY … LIMIT) and validate against
// the old geometry, landing an orphaned pin (ai-review F1/F2; reproduced). The
// same stale row lets two concurrent edits both derive version+1 and collide
// (F3). We therefore serialize on the FAMILY IDENTITY via a transaction-scoped
// advisory lock keyed on the family UUID — a lock that is immune to which row is
// current — and only then read the current published version, freshly, seeing
// every committed row. A UNIQUE(map_family_id, version) constraint (migration
// 0011) is the belt-and-suspenders backstop against a version collision.
func lockCurrentPublishedVersion(ctx context.Context, tx *sql.Tx, seatMapID, organizerID uuid.UUID) (id uuid.UUID, version int32, family uuid.UUID, err error) {
	// Resolve the family from any version id (organizer-scoped), then take the
	// family advisory lock BEFORE reading the current version. hashtextextended
	// maps the family UUID to the bigint pg_advisory_xact_lock expects; the lock
	// releases automatically at commit/rollback.
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		seatMapID, organizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("lock seat-map family: %w", err)
	}
	// Now that the family is exclusively held, the current published version is
	// stable for the rest of this transaction. organizer_id is carried through
	// explicitly (defense in depth, ADR-002): the family was already resolved
	// organizer-scoped and map_family_id is immutable, so this is transitively
	// redundant — but it means the read never crosses a tenant boundary even if
	// the resolution above were ever refactored away.
	err = tx.QueryRowContext(ctx,
		`SELECT id, version FROM seat_maps
		 WHERE map_family_id = $1 AND organizer_id = $2 AND status = 'published'
		 ORDER BY version DESC
		 LIMIT 1`, family, organizerID).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("read current published version: %w", err)
	}
	return id, version, family, nil
}

// EditSeatMap edits a published seat map into a new published version (TKT-104,
// COS-1/2/3/4). The transition is state-deriving — whether the edit is legal
// depends on which seats are currently pinned — so it decides under the family
// advisory lock (lockCurrentPublishedVersion, NOT a row FOR UPDATE — see ADR-029
// and that function's doc) and emits after commit.
// A pinned seat identity that the submitted geometry does not reproduce exactly
// once is orphaned; the edit is hard-rejected (ErrSeatMapEditOrphansPinned) and
// the predecessor is left untouched. Duplicate identities in the submission are
// caught by the new version's UNIQUE(seat_map_id, seat_identity) as
// ErrSeatMapConflict. Organizer scoping is enforced in the lock resolution and
// on every insert (ADR-002).
func (p *Postgres) EditSeatMap(ctx context.Context, in EditSeatMapInput) (SeatMap, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return SeatMap{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	curID, curVersion, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return SeatMap{}, false, err
	}

	// The set of identities the submitted geometry will compose, server-side, the
	// same way AddSeatMapSeat does: "section/row/seat".
	submitted := map[string]struct{}{}
	for _, s := range in.Sections {
		for _, r := range s.Rows {
			for _, seat := range r.Seats {
				submitted[s.Name+"/"+r.Label+"/"+seat.Label] = struct{}{}
			}
		}
	}

	// Pins are family-scoped and version-independent: read them under the lock so
	// a concurrent PinSeat (which holds the same lock) cannot slip a pin in
	// between this check and the commit.
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT seat_identity FROM seat_map_pins WHERE map_family_id = $1`, family)
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("read pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return SeatMap{}, false, fmt.Errorf("scan pin: %w", err)
		}
		if _, ok := submitted[identity]; !ok {
			return SeatMap{}, false, ErrSeatMapEditOrphansPinned
		}
	}
	if err := rows.Err(); err != nil {
		return SeatMap{}, false, fmt.Errorf("iterate pins: %w", err)
	}
	_ = rows.Close()

	// Create the new published version in the same family. name/organizer/venue
	// are copied from the current version so the edit is a pure geometry change.
	var newMap SeatMap
	var publishedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`INSERT INTO seat_maps (organizer_id, venue_id, name, version, status, published_at, map_family_id, orphan_prevention_enabled)
		 SELECT organizer_id, venue_id, name, $2, 'published', now(), map_family_id,
		        COALESCE($3, orphan_prevention_enabled)
		 FROM seat_maps WHERE id = $1
		 RETURNING id, organizer_id, venue_id, name, version, status, published_at, created_at, orphan_prevention_enabled`,
		curID, curVersion+1, in.OrphanPreventionEnabled).
		Scan(&newMap.ID, &newMap.OrganizerID, &newMap.VenueID, &newMap.Name,
			&newMap.Version, &newMap.Status, &publishedAt, &newMap.CreatedAt,
			&newMap.OrphanPreventionEnabled)
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("create new version: %w", err)
	}
	if publishedAt.Valid {
		newMap.PublishedAt = &publishedAt.Time
	}

	// Clone the submitted geometry into the new version. seat_identity is composed
	// server-side, exactly as AddSeatMapSeat, so a pinned identity that the caller
	// reproduced by section/row/seat labels resolves byte-identically.
	for _, s := range in.Sections {
		var sectionID uuid.UUID
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO seat_map_sections (organizer_id, seat_map_id, name, position)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			in.OrganizerID, newMap.ID, s.Name, s.Position).Scan(&sectionID); err != nil {
			if isUniqueViolation(err) {
				return SeatMap{}, false, fmt.Errorf("section: %w", ErrSeatMapConflict)
			}
			return SeatMap{}, false, fmt.Errorf("clone section: %w", err)
		}
		for _, r := range s.Rows {
			var rowID uuid.UUID
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO seat_map_rows (organizer_id, seat_map_id, section_id, label, position)
				 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				in.OrganizerID, newMap.ID, sectionID, r.Label, r.Position).Scan(&rowID); err != nil {
				if isUniqueViolation(err) {
					return SeatMap{}, false, fmt.Errorf("row: %w", ErrSeatMapConflict)
				}
				return SeatMap{}, false, fmt.Errorf("clone row: %w", err)
			}
			for _, seat := range r.Seats {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO seat_map_seats (organizer_id, seat_map_id, row_id, seat_identity, label, position)
					 VALUES ($1, $2, $3, $4 || '/' || $5 || '/' || $6, $6, $7)`,
					in.OrganizerID, newMap.ID, rowID, s.Name, r.Label, seat.Label, seat.Position); err != nil {
					if isUniqueViolation(err) {
						return SeatMap{}, false, fmt.Errorf("seat: %w", ErrSeatMapConflict)
					}
					return SeatMap{}, false, fmt.Errorf("clone seat: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return SeatMap{}, false, fmt.Errorf("commit edit: %w", err)
	}
	// A freshly created version always owes its event.
	return newMap, true, nil
}

// PinSeat records a sale/hold reference to a seat identity (TKT-104, COS-5 — the
// write path TKT-80 consumes). Under the SAME family advisory lock EditSeatMap
// takes, it re-resolves the current published version and validates the identity
// exists in that version before inserting, so an edit that already dropped the
// seat is visible here as ErrSeatIdentityNotFound. Idempotent on
// (map_family_id, seat_identity, pinned_by).
func (p *Postgres) PinSeat(ctx context.Context, in PinSeatInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	curID, _, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return err
	}

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM seat_map_seats WHERE seat_map_id = $1 AND seat_identity = $2)`,
		curID, in.SeatIdentity).Scan(&exists); err != nil {
		return fmt.Errorf("check seat identity: %w", err)
	}
	if !exists {
		return ErrSeatIdentityNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO seat_map_pins (organizer_id, map_family_id, seat_identity, pinned_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (map_family_id, seat_identity, pinned_by) DO NOTHING`,
		in.OrganizerID, family, in.SeatIdentity, in.PinnedBy); err != nil {
		return fmt.Errorf("insert pin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pin: %w", err)
	}
	return nil
}

// UnpinSeat clears a pin (sale cancelled / hold released). Idempotent: removing
// an absent pin is a no-op. It runs under the SAME family advisory lock as
// PinSeat/EditSeatMap (ai-review F4): a standalone DELETE could not see an
// uncommitted concurrent PinSeat and would report a release that then never
// happened once the pin commits — serializing on the family closes that window.
func (p *Postgres) UnpinSeat(ctx context.Context, in PinSeatInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve the family (organizer-scoped) and take the family advisory lock, so
	// the delete serializes with a concurrent pin/edit on the same family.
	var family uuid.UUID
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		in.SeatMapID, in.OrganizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown map/organizer: nothing to unpin, stay idempotent.
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return fmt.Errorf("lock seat-map family: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM seat_map_pins
		 WHERE organizer_id = $1 AND map_family_id = $2 AND seat_identity = $3 AND pinned_by = $4`,
		in.OrganizerID, family, in.SeatIdentity, in.PinnedBy); err != nil {
		return fmt.Errorf("unpin seat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpin: %w", err)
	}
	return nil
}

// PinSeats pins a seat set atomically (TKT-80): one family advisory lock, every identity
// validated against the current published version, all inserted or — if any is absent —
// none, returning ErrSeatIdentityNotFound. This mirrors the single-seat PinSeat's
// lock-then-validate-then-insert but over a set, so an inventory seat-hold's pins land
// all-or-nothing under the same lock EditSeatMap takes.
func (p *Postgres) PinSeats(ctx context.Context, in BatchPinInput) error {
	if len(in.SeatIdentities) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	curID, _, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return err
	}
	for _, identity := range in.SeatIdentities {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM seat_map_seats WHERE seat_map_id = $1 AND seat_identity = $2)`,
			curID, identity).Scan(&exists); err != nil {
			return fmt.Errorf("check seat identity: %w", err)
		}
		if !exists {
			return ErrSeatIdentityNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO seat_map_pins (organizer_id, map_family_id, seat_identity, pinned_by)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (map_family_id, seat_identity, pinned_by) DO NOTHING`,
			in.OrganizerID, family, identity, in.PinnedBy); err != nil {
			return fmt.Errorf("insert pin: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pins: %w", err)
	}
	return nil
}

// UnpinSeats clears a seat set atomically under the same family advisory lock. Idempotent:
// removing an absent pin is a no-op, so a retried release cannot fail.
func (p *Postgres) UnpinSeats(ctx context.Context, in BatchPinInput) error {
	if len(in.SeatIdentities) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var family uuid.UUID
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		in.SeatMapID, in.OrganizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // unknown map/organizer: nothing to unpin, stay idempotent
	}
	if err != nil {
		return fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return fmt.Errorf("lock seat-map family: %w", err)
	}
	for _, identity := range in.SeatIdentities {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM seat_map_pins
			 WHERE organizer_id = $1 AND map_family_id = $2 AND pinned_by = $3 AND seat_identity = $4`,
			in.OrganizerID, family, in.PinnedBy, identity); err != nil {
			return fmt.Errorf("unpin seat: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpins: %w", err)
	}
	return nil
}

// ListSeatMapPins drains seat_map_pins one bounded page at a time, keyset-ordered by the
// primary key (TKT-112). The scan is bounded by the page, which the PRIMARY KEY's unique
// index backs — no additional index, and no ADR-019 subset claim to prove, because this is
// a deliberate FULL drain for an operator command rather than a scoped read.
//
// Every pin is returned regardless of `pinned_by` namespace: the reconciler owns the
// hold/sale classification, and a catalog-side filter would silently define which pins are
// reclaimable in the service that has no way to know.
//
// The seat-map id is resolved per FAMILY (newest version), not per creation version — pins
// are version-independent (ADR-029) and `UnpinSeats` resolves the family from whatever
// version id it is handed, so the newest member reaches every pin in the lineage. The join
// is an inner one: a pin whose family has no seat_maps row left cannot be named by any
// version id, so it is unreachable through the family-locked path and is not listed.
func (p *Postgres) ListSeatMapPins(ctx context.Context, after uuid.UUID, limit int) ([]SeatMapPin, error) {
	if limit <= 0 || limit > MaxSeatMapPinPage {
		return nil, fmt.Errorf("seat-map pin page limit must be 1..%d, got %d", MaxSeatMapPinPage, limit)
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT p.id, p.organizer_id, m.id, p.seat_identity, p.pinned_by
		   FROM seat_map_pins p
		   JOIN LATERAL (
		        SELECT s.id FROM seat_maps s
		         WHERE s.map_family_id = p.map_family_id AND s.organizer_id = p.organizer_id
		         ORDER BY s.version DESC LIMIT 1
		   ) m ON true
		  WHERE p.id > $1
		  ORDER BY p.id
		  LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list seat-map pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SeatMapPin{}
	for rows.Next() {
		var pin SeatMapPin
		if err := rows.Scan(&pin.ID, &pin.OrganizerID, &pin.SeatMapID, &pin.SeatIdentity, &pin.PinnedBy); err != nil {
			return nil, fmt.Errorf("scan seat-map pin: %w", err)
		}
		out = append(out, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seat-map pins: %w", err)
	}
	return out, nil
}

func (p *Postgres) AddSeatMapSection(ctx context.Context, in SeatMapSectionInput) (SeatMapSection, error) {
	s := SeatMapSection{Name: in.Name, Position: in.Position}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_sections (organizer_id, seat_map_id, name, position)
		 SELECT $1, m.id, $3, $4 FROM seat_maps m
		 WHERE m.id = $2 AND m.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id`,
		in.OrganizerID, in.SeatMapID, in.Name, in.Position).Scan(&s.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapSection{}, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapSection{}, fmt.Errorf("section: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapSection{}, fmt.Errorf("add section: %w", err)
	}
	return s, nil
}

func (p *Postgres) AddSeatMapRow(ctx context.Context, in SeatMapRowInput) (SeatMapRow, error) {
	r := SeatMapRow{Label: in.Label, Position: in.Position}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_rows (organizer_id, seat_map_id, section_id, label, position)
		 SELECT $1, s.seat_map_id, s.id, $4, $5 FROM seat_map_sections s
		 JOIN seat_maps m ON m.id = s.seat_map_id
		 WHERE s.id = $3 AND s.seat_map_id = $2 AND s.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id`,
		in.OrganizerID, in.SeatMapID, in.SectionID, in.Label, in.Position).Scan(&r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapRow{}, fmt.Errorf("section: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapRow{}, fmt.Errorf("row: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapRow{}, fmt.Errorf("add row: %w", err)
	}
	return r, nil
}

func (p *Postgres) AddSeatMapSeat(ctx context.Context, in SeatMapSeatInput) (SeatMapSeat, error) {
	seat := SeatMapSeat{Label: in.Label, Position: in.Position}
	// seat_identity is composed here from the parent labels — "section/row/seat"
	// — so it is deterministic and stable (the TKT-104 contract) and never
	// caller-supplied.
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_seats (organizer_id, seat_map_id, row_id, seat_identity, label, position)
		 SELECT $1, r.seat_map_id, r.id, sec.name || '/' || r.label || '/' || $4, $4, $5
		 FROM seat_map_rows r
		 JOIN seat_map_sections sec ON sec.id = r.section_id
		 JOIN seat_maps m ON m.id = r.seat_map_id
		 WHERE r.id = $3 AND r.seat_map_id = $2 AND r.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id, seat_identity`,
		in.OrganizerID, in.SeatMapID, in.RowID, in.Label, in.Position).
		Scan(&seat.ID, &seat.SeatIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapSeat{}, fmt.Errorf("row: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapSeat{}, fmt.Errorf("seat: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapSeat{}, fmt.Errorf("add seat: %w", err)
	}
	return seat, nil
}

func (p *Postgres) ListVenueSeatMaps(ctx context.Context, venueID uuid.UUID) ([]SeatMap, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, created_at, orphan_prevention_enabled
		   FROM seat_maps WHERE venue_id = $1 ORDER BY version, name, id`, venueID)
	if err != nil {
		return nil, fmt.Errorf("list seat maps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SeatMap, 0)
	for rows.Next() {
		var m SeatMap
		if err := rows.Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &m.CreatedAt, &m.OrphanPreventionEnabled); err != nil {
			return nil, fmt.Errorf("scan seat map: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSeatMapVersions returns every version of the family that seatMapID belongs
// to (TKT-105 COS-3), newest first, each carrying published_at. The family is
// resolved from any version id via a subselect on map_family_id (immutable,
// UNIQUE(map_family_id, version) — migration 0011); an unknown id yields no rows
// and ErrNotFound. Unscoped-by-organizer like its sibling public reads
// (ListVenueSeatMaps, GetSeatMapGeometry) — the map id is unguessable and the
// read is non-sensitive geometry metadata.
func (p *Postgres) ListSeatMapVersions(ctx context.Context, seatMapID uuid.UUID) ([]SeatMap, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, published_at, created_at, orphan_prevention_enabled
		   FROM seat_maps
		  WHERE map_family_id = (SELECT map_family_id FROM seat_maps WHERE id = $1)
		  ORDER BY version DESC`, seatMapID)
	if err != nil {
		return nil, fmt.Errorf("list seat-map versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SeatMap, 0)
	for rows.Next() {
		var m SeatMap
		if err := rows.Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &m.PublishedAt, &m.CreatedAt, &m.OrphanPreventionEnabled); err != nil {
			return nil, fmt.Errorf("scan seat-map version: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	return out, nil
}

// UpdateVenueGACapacity sets a venue's GA capacity (TKT-105 COS-5). The
// organizer_id predicate scopes the write to the tenant (ADR-002); no matching
// row yields ErrNotFound (unknown venue or cross-tenant).
func (p *Postgres) UpdateVenueGACapacity(ctx context.Context, in VenueGACapacityInput) (Venue, error) {
	v := Venue{ID: in.VenueID, OrganizerID: in.OrganizerID, GACapacity: in.GACapacity}
	err := p.db.QueryRowContext(ctx,
		`UPDATE venues SET ga_capacity = $3
		  WHERE id = $1 AND organizer_id = $2
		  RETURNING name, created_at`,
		in.VenueID, in.OrganizerID, in.GACapacity).Scan(&v.Name, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Venue{}, fmt.Errorf("venue: %w", ErrNotFound)
	}
	if err != nil {
		return Venue{}, fmt.Errorf("update venue ga_capacity: %w", err)
	}
	return v, nil
}

// seatMapSeatsScopedQuery is the seat-level geometry read, referenced as a const
// so the ADR-019 scan-scope proof EXPLAINs the exact shipped statement. It
// filters seats by seat_map_id, backed by seat_map_seats_by_map.
const seatMapSeatsScopedQuery = `SELECT id, row_id, seat_identity, label, position
	FROM seat_map_seats WHERE seat_map_id = $1 ORDER BY position`

func (p *Postgres) GetSeatMapGeometry(ctx context.Context, seatMapID uuid.UUID) (SeatMapGeometry, error) {
	var g SeatMapGeometry
	err := p.db.QueryRowContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, created_at, orphan_prevention_enabled
		   FROM seat_maps WHERE id = $1`, seatMapID).
		Scan(&g.Map.ID, &g.Map.OrganizerID, &g.Map.VenueID, &g.Map.Name,
			&g.Map.Version, &g.Map.Status, &g.Map.CreatedAt, &g.Map.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapGeometry{}, ErrNotFound
	}
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("get seat map: %w", err)
	}

	// Rows keyed by their parent so the three flat reads assemble into a tree.
	sections := map[uuid.UUID]*SeatMapSection{}
	rowsByID := map[uuid.UUID]*SeatMapRow{}

	secRows, err := p.db.QueryContext(ctx,
		`SELECT id, name, position FROM seat_map_sections WHERE seat_map_id = $1 ORDER BY position`, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read sections: %w", err)
	}
	defer func() { _ = secRows.Close() }()
	for secRows.Next() {
		var s SeatMapSection
		if err := secRows.Scan(&s.ID, &s.Name, &s.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan section: %w", err)
		}
		s.Rows = []SeatMapRow{}
		g.Sections = append(g.Sections, s)
	}
	if err := secRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("sections rows: %w", err)
	}
	for i := range g.Sections {
		sections[g.Sections[i].ID] = &g.Sections[i]
	}

	rowRows, err := p.db.QueryContext(ctx,
		`SELECT id, section_id, label, position FROM seat_map_rows WHERE seat_map_id = $1 ORDER BY position`, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read rows: %w", err)
	}
	defer func() { _ = rowRows.Close() }()
	for rowRows.Next() {
		var r SeatMapRow
		var sectionID uuid.UUID
		if err := rowRows.Scan(&r.ID, &sectionID, &r.Label, &r.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan row: %w", err)
		}
		r.Seats = []SeatMapSeat{}
		if sec, ok := sections[sectionID]; ok {
			sec.Rows = append(sec.Rows, r)
		}
	}
	if err := rowRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("rows rows: %w", err)
	}
	for si := range g.Sections {
		for ri := range g.Sections[si].Rows {
			rowsByID[g.Sections[si].Rows[ri].ID] = &g.Sections[si].Rows[ri]
		}
	}

	seatRows, err := p.db.QueryContext(ctx, seatMapSeatsScopedQuery, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read seats: %w", err)
	}
	defer func() { _ = seatRows.Close() }()
	for seatRows.Next() {
		var s SeatMapSeat
		var rowID uuid.UUID
		if err := seatRows.Scan(&s.ID, &rowID, &s.SeatIdentity, &s.Label, &s.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan seat: %w", err)
		}
		if r, ok := rowsByID[rowID]; ok {
			r.Seats = append(r.Seats, s)
		}
	}
	if err := seatRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("seats rows: %w", err)
	}
	return g, nil
}
