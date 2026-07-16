package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Channel allocations (TKT-78 / ADR-024): opaque per-channel caps carved out of a pool's
// capacity, with an optional DB-time give-back. Consumption is derived from claims via the
// shared liveClaims predicate — never a re-derived expiry expression, never a counter.

// activeAllocation mirrors liveClaims for allocation rows: an allocation reserves capacity
// until its release_at passes, decided by PostgreSQL time. clock_timestamp(), not now():
// now() is frozen at transaction start, so a hold transaction queued on the pool lock
// across the cutoff would decide with stale time and sell a released channel. Judging at
// decision time keeps the release boundary exact under contention (ai-review finding 1);
// the global capacity check never depends on this predicate, so the two time bases cannot
// combine into an oversell.
const activeAllocation = `(release_at IS NULL OR release_at > clock_timestamp())`

// consumingClaims is what counts against a channel's cap: confirmed consumption is
// permanent, live holds are temporary. Built on liveClaims so expiry semantics can never
// fork between pool and channel accounting.
const consumingClaims = `(status='confirmed' OR ` + liveClaims + `)`

// reservedForChannelsSQL computes the capacity still reserved for active allocations —
// what the public channel may not touch. Consumed quantity is clamped at the cap so an
// over-consumed channel (cap lowered is rejected, but operational math stays safe) can
// never inflate the public share.
const reservedForChannelsSQL = `SELECT COALESCE(sum(GREATEST(a.cap::bigint - COALESCE(u.used,0), 0)),0)
	FROM channel_allocations a
	LEFT JOIN LATERAL (
		SELECT sum(quantity)::bigint AS used FROM claims
		WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND ` + consumingClaims + `
	) u ON true
	WHERE a.pool_id=$1 AND ` + activeAllocation

type ChannelAllocation struct {
	Channel   string     `json:"channel"`
	Cap       int32      `json:"cap"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
}

type ChannelAvailability struct {
	Channel   string     `json:"channel"`
	Cap       int32      `json:"cap"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
	Released  bool       `json:"released"`
	Held      int32      `json:"held"`
	Confirmed int32      `json:"confirmed"`
	Available int32      `json:"available"`
}

// ReplaceChannelAllocations atomically replaces the pool's full allocation set under the
// pool lock, so capacity can move between channels without a transient window where the
// sum overshoots. Caps are validated against pool capacity and against each channel's
// current consumption; an empty set returns everything to the public channel.
func (p *Postgres) ReplaceChannelAllocations(ctx context.Context, org, slot uuid.UUID, allocs []ChannelAllocation) ([]ChannelAllocation, error) {
	seen := map[string]bool{}
	var total int64
	for _, a := range allocs {
		if a.Channel == "" || a.Cap <= 0 {
			return nil, fmt.Errorf("allocation needs a channel and a positive cap")
		}
		if seen[a.Channel] {
			return nil, fmt.Errorf("duplicate channel %q", a.Channel)
		}
		seen[a.Channel] = true
		total += int64(a.Cap)
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity int32
	err = tx.QueryRowContext(ctx, `SELECT capacity FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if total > int64(capacity) {
		return nil, ErrUnavailable
	}
	for _, a := range allocs {
		var consumed int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, a.Channel).Scan(&consumed); err != nil {
			return nil, err
		}
		if consumed > int64(a.Cap) {
			return nil, ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM channel_allocations WHERE pool_id=$1`, slot); err != nil {
		return nil, err
	}
	for _, a := range allocs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO channel_allocations(pool_id,channel_code,cap,release_at) VALUES($1,$2,$3,$4)`, slot, a.Channel, a.Cap, a.ReleaseAt); err != nil {
			return nil, err
		}
	}
	if allocs == nil {
		allocs = []ChannelAllocation{}
	}
	return allocs, tx.Commit()
}

// channelAvailabilities lists every allocation row (released ones included, with zero
// availability) plus its derived consumption. Runs inside or outside a pool lock.
func channelAvailabilities(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, pool uuid.UUID, globalRemaining int64) ([]ChannelAvailability, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.channel_code, a.cap, a.release_at,
			(a.release_at IS NOT NULL AND a.release_at <= clock_timestamp()),
			COALESCE(h.held,0), COALESCE(cf.confirmed,0)
		FROM channel_allocations a
		LEFT JOIN LATERAL (SELECT sum(quantity) AS held FROM claims WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND `+liveClaims+`) h ON true
		LEFT JOIN LATERAL (SELECT sum(quantity) AS confirmed FROM claims WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND status='confirmed') cf ON true
		WHERE a.pool_id=$1 ORDER BY a.channel_code`, pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ChannelAvailability{}
	for rows.Next() {
		var c ChannelAvailability
		if err = rows.Scan(&c.Channel, &c.Cap, &c.ReleaseAt, &c.Released, &c.Held, &c.Confirmed); err != nil {
			return nil, err
		}
		if !c.Released {
			c.Available = clampAvailable(min(globalRemaining, int64(c.Cap)-int64(c.Held)-int64(c.Confirmed)))
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func clampAvailable(v int64) int32 {
	if v < 0 {
		return 0
	}
	return int32(v)
}
