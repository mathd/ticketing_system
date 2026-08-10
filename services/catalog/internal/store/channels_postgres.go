package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// Persistence for the sales-channel registry (TKT-235 / epic TKT-17). The write
// gate and the exactness rule are pure and live in channels.go; this is the half
// that needs a database.

// publicChannelsQuery is the exact statement production runs, held as a const so
// the ADR-019 plan assertion binds to it rather than to a hand-copied reduction
// that is free to drift from the shipped SQL.
//
// The partial index channels_enabled_by_organizer (organizer_id, code) WHERE
// enabled backs both the filter and the ordering. ADR-019: a scoped read is only
// scoped if an index backs the filter — copying a query shape that scales ships
// a no-op, so the smoke test asserts the plan, not just the rows.
const publicChannelsQuery = `
SELECT code, display_name
FROM channels
WHERE organizer_id = $1 AND enabled
ORDER BY code`

const operatorChannelsQuery = `
SELECT id, organizer_id, code, display_name, kind, enabled, created_at, updated_at
FROM channels
WHERE organizer_id = $1
ORDER BY code`

const getChannelQuery = `
SELECT id, organizer_id, code, display_name, kind, enabled, created_at, updated_at
FROM channels
WHERE id = $1`

// CreateChannel registers a channel.
//
// No write gate of the splits/fees kind is needed: organizer_id carries a real
// foreign key here, because unlike scope_id it names exactly one table. A
// missing organizer is a 23503 and comes back as ErrNotFound.
func (p *Postgres) CreateChannel(ctx context.Context, in ChannelInput) (Channel, error) {
	code, err := validateChannelWrite(in.Code, in.DisplayName, in.Kind)
	if err != nil {
		return Channel{}, err
	}
	var c Channel
	err = p.db.QueryRowContext(ctx, `
INSERT INTO channels (organizer_id, code, display_name, kind, enabled)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, organizer_id, code, display_name, kind, enabled, created_at, updated_at`,
		in.OrganizerID, code, in.DisplayName, string(in.Kind), in.Enabled,
	).Scan(&c.ID, &c.OrganizerID, &c.Code, &c.DisplayName, &c.Kind, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	switch {
	case isUniqueViolation(err):
		return Channel{}, ErrChannelCodeTaken
	case isFKViolation(err):
		return Channel{}, ErrNotFound
	case err != nil:
		return Channel{}, err
	}
	return c, nil
}

// UpdateChannel replaces a channel's mutable fields.
//
// The code is immutable. It is compared under the same statement that writes, so
// there is no read-then-write window in which another writer could change the
// code between the check and the update: the WHERE clause carries the comparison
// and a mismatch simply matches no row. Distinguishing "no such channel" from
// "code differs" then takes one follow-up read, which only runs on the error
// path.
//
// updated_at is written explicitly — catalog has no updated_at trigger anywhere,
// so a store that forgets leaves a stale value rather than failing loudly.
func (p *Postgres) UpdateChannel(ctx context.Context, id uuid.UUID, in ChannelUpdate) (Channel, error) {
	code, err := validateChannelWrite(in.Code, in.DisplayName, in.Kind)
	if err != nil {
		return Channel{}, err
	}
	var c Channel
	err = p.db.QueryRowContext(ctx, `
UPDATE channels
SET display_name = $3, kind = $4, enabled = $5, updated_at = now()
WHERE id = $1 AND code = $2
RETURNING id, organizer_id, code, display_name, kind, enabled, created_at, updated_at`,
		id, code, in.DisplayName, string(in.Kind), in.Enabled,
	).Scan(&c.ID, &c.OrganizerID, &c.Code, &c.DisplayName, &c.Kind, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Either the channel does not exist, or it does and the submitted code
		// differs from the stored one. Only the second is ErrChannelCodeImmutable
		// — reporting it for an unknown id would tell a caller that an id it
		// guessed exists.
		if _, getErr := p.GetChannel(ctx, id); getErr != nil {
			return Channel{}, getErr
		}
		return Channel{}, ErrChannelCodeImmutable
	}
	if err != nil {
		return Channel{}, err
	}
	return c, nil
}

// GetChannel returns one channel, enabled or not.
func (p *Postgres) GetChannel(ctx context.Context, id uuid.UUID) (Channel, error) {
	var c Channel
	err := p.db.QueryRowContext(ctx, getChannelQuery, id).
		Scan(&c.ID, &c.OrganizerID, &c.Code, &c.DisplayName, &c.Kind, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, err
	}
	return c, nil
}

// ListChannels is the operator read: every channel the organizer has defined,
// enabled and disabled, with full definitions.
func (p *Postgres) ListChannels(ctx context.Context, organizerID uuid.UUID) ([]Channel, error) {
	rows, err := p.db.QueryContext(ctx, operatorChannelsQuery, organizerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.OrganizerID, &c.Code, &c.DisplayName, &c.Kind, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListEnabledChannels is the public read: enabled channels only, as code +
// display name.
//
// The enabled filter is in the SQL, not applied afterwards in Go. That is the
// point of the partial index, and it is also what keeps a disabled channel from
// ever being loaded into a response the public can see — a post-filter would put
// the row in memory first and rely on every caller remembering to drop it.
func (p *Postgres) ListEnabledChannels(ctx context.Context, organizerID uuid.UUID) ([]PublicChannel, error) {
	rows, err := p.db.QueryContext(ctx, publicChannelsQuery, organizerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PublicChannel{}
	for rows.Next() {
		var c PublicChannel
		if err := rows.Scan(&c.Code, &c.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
