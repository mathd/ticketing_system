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

// channelColumns is the full-row projection every operator read, the create
// and the update return, in the order scanChannel reads it. One list, so a
// column added later (as revision was, TKT-325) cannot reach some reads and
// silently come back as Go's zero value from others.
const channelColumns = `id, organizer_id, code, display_name, kind, enabled, revision, created_at, updated_at`

const operatorChannelsQuery = `
SELECT ` + channelColumns + `
FROM channels
WHERE organizer_id = $1
ORDER BY code`

const getChannelQuery = `
SELECT ` + channelColumns + `
FROM channels
WHERE id = $1`

// lockChannelForUpdateQuery reads the caller's channel under a row lock. The
// organizer predicate is in the locking read itself, so another organizer's
// row is never locked, never compared and never described (TKT-251: scoping a
// write means scoping its failure path too).
const lockChannelForUpdateQuery = `
SELECT ` + channelColumns + `
FROM channels
WHERE id = $1 AND organizer_id = $2
FOR UPDATE`

// scanChannel reads one channelColumns row.
func scanChannel(row interface{ Scan(...any) error }, c *Channel) error {
	return row.Scan(&c.ID, &c.OrganizerID, &c.Code, &c.DisplayName, &c.Kind, &c.Enabled, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
}

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
	// revision is left to its column default, 0 (0025).
	err = scanChannel(p.db.QueryRowContext(ctx, `
INSERT INTO channels (organizer_id, code, display_name, kind, enabled)
VALUES ($1, $2, $3, $4, $5)
RETURNING `+channelColumns,
		in.OrganizerID, code, in.DisplayName, string(in.Kind), in.Enabled,
	), &c)
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

// UpdateChannel replaces a channel's mutable fields, if the caller's
// expected revision is still the stored one (TKT-325, ADR-080).
//
// The order is the contract, and each step is in its place for a reason:
//
//  1. The pure write gate, before any database work.
//  2. In one transaction, an organizer-scoped SELECT ... FOR UPDATE. No row
//     means ErrNotFound: the channel does not exist, or it belongs to another
//     organizer, and the two must not be distinguishable to a caller who
//     guessed an id. Because ownership is decided here, first, another
//     organizer never learns whether a revision or a code is right.
//  3. The revision comparison, UNDER THAT LOCK. A comparison before the lock,
//     or without it, lets two writers holding the same revision both pass
//     and both apply; the lock makes the second one wait and then read the
//     first one's revision. There is no revision predicate on the UPDATE, so
//     this lock is the only thing that serializes the comparison.
//  4. The immutable code. After the revision, so a stale editor is told to
//     reload rather than that a field is wrong.
//  5. The UPDATE, scoped like the read, with revision = revision + 1 on every
//     success, including an unchanged replacement.
//
// Success is returned only after the commit succeeds. A refusal rolls back
// and writes nothing: no field, no timestamp, no revision.
//
// The organizer predicate is what makes this a TENANT boundary rather than a
// convention (TKT-236 ai-review). Without it, a caller holding a channel's id
// and code could rename, re-kind, enable or disable another organizer's
// channel. An id is not an authorization boundary, and the back office takes
// the id from a form field.
//
// updated_at is written explicitly. Catalog has no updated_at trigger
// anywhere, so a store that forgets leaves a stale value rather than failing
// loudly.
func (p *Postgres) UpdateChannel(ctx context.Context, organizerID, id uuid.UUID, in ChannelUpdate) (Channel, error) {
	code, err := validateChannelWrite(in.Code, in.DisplayName, in.Kind)
	if err != nil {
		return Channel{}, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Channel{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var current Channel
	err = scanChannel(tx.QueryRowContext(ctx, lockChannelForUpdateQuery, id, organizerID), &current)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, err
	}
	if current.Revision != in.ExpectedRevision {
		return Channel{}, ErrChannelRevisionMismatch
	}
	if current.Code != code {
		return Channel{}, ErrChannelCodeImmutable
	}

	var c Channel
	err = scanChannel(tx.QueryRowContext(ctx, `
UPDATE channels
SET display_name = $3, kind = $4, enabled = $5, revision = revision + 1, updated_at = now()
WHERE id = $1 AND organizer_id = $2
RETURNING `+channelColumns,
		id, organizerID, in.DisplayName, string(in.Kind), in.Enabled,
	), &c)
	if err != nil {
		// The row is locked by this transaction and scoped the same way, so
		// no-rows here is not a refusal anyone can cause; report it as is.
		return Channel{}, err
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, err
	}
	return c, nil
}

// GetChannel returns one channel, enabled or not.
func (p *Postgres) GetChannel(ctx context.Context, id uuid.UUID) (Channel, error) {
	var c Channel
	err := scanChannel(p.db.QueryRowContext(ctx, getChannelQuery, id), &c)
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
		if err := scanChannel(rows, &c); err != nil {
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
