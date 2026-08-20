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
	// The public-read cache invalidation seam (TKT-206) —
	// public_read_invalidation.go.
	publicReadInvalidatorFields
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
