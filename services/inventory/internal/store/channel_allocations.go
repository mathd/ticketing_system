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

// windowOpen is TKT-238's predicate: may this channel sell RIGHT NOW (ADR-054)?
//
// Half-open [opens_at, closes_at) — sellable AT the open bound, not at the close. NULL on
// either side is unbounded there. Same convention as the price/fee/split rule windows, for
// the same reason: two windows that abut must not both admit the instant between them.
//
// clock_timestamp(), not now(), for exactly the reason activeAllocation above gives — and
// the two are separate predicates deliberately. `release_at` answers "does this allocation
// still reserve capacity"; a window answers "may it sell". A closed window does NOT release
// capacity (ADR-054 §Decision B): a presale's cap is a promise, and a promise that
// evaporates until its window opens is not one. So this predicate appears in the CLAIM
// paths and never in reservedForChannelsSQL.
//
// One const, reused. consumedQuantity's comment above records why: six call sites sum a
// claim's consumption and one of them silently used a different expression. A window
// predicate copied by hand into three claim paths would fork the same way, and the fork
// would be invisible until a hold succeeded on a closed channel.
const windowOpen = `(opens_at IS NULL OR opens_at <= clock_timestamp())
	AND (closes_at IS NULL OR closes_at > clock_timestamp())`

// codeWindowOpen is TKT-239's validity predicate for a presale code (ADR-055). Same
// half-open [opens_at, closes_at) convention and the same clock_timestamp() reasoning as
// windowOpen above — it is a separate const only because it reads presale_codes columns
// rather than channel_allocations ones, and inlining either into the other's query would
// be the hand-copy consumedQuantity's comment warns about.
//
// A code's window and its CHANNEL's window are independent and BOTH must admit the
// instant. A code valid all week on a channel that opens Friday sells Friday.
const codeWindowOpen = `(opens_at IS NULL OR opens_at <= clock_timestamp())
	AND (closes_at IS NULL OR closes_at > clock_timestamp())`

// consumingClaims is what counts against a channel's cap: confirmed consumption is
// permanent, live holds are temporary. Built on liveClaims so expiry semantics can never
// fork between pool and channel accounting.
const consumingClaims = `(status='confirmed' OR ` + liveClaims + `)`

// codeRedeemedQuantity is HOW MUCH of a presale code has been redeemed: the same
// consumed-quantity sum that counts against a channel cap, scoped to the code instead of
// the pool.
//
// Built from consumedQuantity and consumingClaims deliberately — a code's redemption count
// must mean exactly what a channel's consumption means, or a refund would return channel
// capacity while leaving a redemption burnt, and a hold expiry would free a seat while
// keeping its code spent. Derived, never a counter: a counter cannot be decremented by
// LAZY hold expiry without a sweeper, and ADR-010 forbids requiring one for correctness.
//
// NOT SCOPED BY pool_id, and that is the whole difficulty of this ticket. A channel
// allocation is PRIMARY KEY (pool_id, channel_code), so every other derived count in this
// file is pool-scoped and the pool row lock serializes it. A presale code is
// (organizer_id, channel_code, code) and spans every slot in the presale, so this sum
// reads rows the pool lock does not cover. The presale_codes ROW LOCK is what serializes
// it — see redeemPresaleCode.
const codeRedeemedQuantity = `SELECT COALESCE(sum(` + consumedQuantity + `),0)::bigint FROM claims
	WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3 AND ` + consumingClaims

// consumedQuantity is HOW MUCH a counted claim consumes — net of anything a refund gave
// back (TKT-161). Confirmed capacity is no longer permanent: a refund returns part of it,
// and a channel that kept counting the original quantity would refuse to resell seats the
// pool has already put back.
//
// It is a single expression on purpose. Six call sites sum a claim's consumption, and one
// of them (channelRowsSQL) never used consumingClaims at all — it sums a bare
// `status='confirmed'`. Six hand-written variants is how the fifth one gets missed.
const consumedQuantity = `(CASE WHEN status='confirmed' THEN quantity-returned_quantity ELSE quantity END)`

// reservedForChannelsSQL computes the capacity still reserved for active allocations —
// what the public channel may not touch. Consumed quantity is clamped at the cap so an
// over-consumed channel (cap lowered is rejected, but operational math stays safe) can
// never inflate the public share.
const reservedForChannelsSQL = `SELECT COALESCE(sum(GREATEST(a.cap::bigint - COALESCE(u.used,0), 0)),0)
	FROM channel_allocations a
	LEFT JOIN LATERAL (
		SELECT sum(` + consumedQuantity + `)::bigint AS used FROM claims
		WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND ` + consumingClaims + `
	) u ON true
	WHERE a.pool_id=$1 AND ` + activeAllocation

type ChannelAllocation struct {
	Channel   string     `json:"channel"`
	Cap       int32      `json:"cap"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
	// The sales window (TKT-238). Nil on either side is unbounded there: no
	// OpensAt means always open, no ClosesAt means never closes. A reversed
	// window is unrepresentable — the migration's CHECK refuses it.
	OpensAt  *time.Time `json:"opens_at,omitempty"`
	ClosesAt *time.Time `json:"closes_at,omitempty"`
	// RequiresCode gates the channel behind a presale unlock code (TKT-239 /
	// ADR-055). False by default, so an allocation that predates this field sells
	// exactly as it did. Orthogonal to the window: a window says WHEN, this says
	// WHO, and both must admit a claim.
	RequiresCode bool `json:"requires_code,omitempty"`
	// SoldBy binds the allocation to ONE reseller (TKT-246, amending ADR-024).
	// Unset means unbound = public, which is every allocation predating this field.
	//
	// A uuid rather than a bool for the same reason RequiresCode is not enough on
	// its own: "spoken for" would let reseller B consume reseller A's stock. Judged
	// in the claim paths under the pool row lock (see sellerAdmits), never here.
	SoldBy *uuid.UUID `json:"sold_by,omitempty"`
}

type ChannelAvailability struct {
	Channel   string     `json:"channel"`
	Cap       int32      `json:"cap"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
	Released  bool       `json:"released"`
	// The sales window and whether it is open right now (TKT-238). STAFF ONLY:
	// the public availability read reports 0 for a closed channel and says
	// nothing about why. An operator needs the why — a channel showing 0 because
	// it has not opened yet and one showing 0 because it sold out are different
	// problems — and a buyer does not.
	OpensAt    *time.Time `json:"opens_at,omitempty"`
	ClosesAt   *time.Time `json:"closes_at,omitempty"`
	WindowOpen bool       `json:"window_open"`
	Held       int32      `json:"held"`
	Confirmed  int32      `json:"confirmed"`
	Available  int32      `json:"available"`
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
	// During a draining cut (TKT-76) a new allocation set must fit the requested
	// target, not the materialized clamp floor.
	var capacity int32
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(target_capacity, capacity) FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity)
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
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(`+consumedQuantity+`),0) FROM claims WHERE pool_id=$1 AND channel_code=$2 AND `+consumingClaims, slot, a.Channel).Scan(&consumed); err != nil {
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO channel_allocations(pool_id,channel_code,cap,release_at,opens_at,closes_at,requires_code,sold_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			slot, a.Channel, a.Cap, a.ReleaseAt, a.OpensAt, a.ClosesAt, a.RequiresCode, a.SoldBy); err != nil {
			return nil, err
		}
	}
	if allocs == nil {
		allocs = []ChannelAllocation{}
	}
	return allocs, p.commitAvailability(tx, slot)
}

// channelAvailabilities lists every allocation row (released ones included, with zero
// availability) plus its derived consumption. Runs inside or outside a pool lock.
func channelAvailabilities(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, pool uuid.UUID, globalRemaining int64) ([]ChannelAvailability, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.channel_code, a.cap, a.release_at,
			(a.release_at IS NOT NULL AND a.release_at <= clock_timestamp()),
			a.opens_at, a.closes_at, (`+windowOpen+`),
			COALESCE(h.held,0), COALESCE(cf.confirmed,0)
		FROM channel_allocations a
		LEFT JOIN LATERAL (SELECT sum(quantity) AS held FROM claims WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND `+liveClaims+`) h ON true
		LEFT JOIN LATERAL (SELECT sum(`+consumedQuantity+`) AS confirmed FROM claims WHERE pool_id=a.pool_id AND channel_code=a.channel_code AND status='confirmed') cf ON true
		WHERE a.pool_id=$1 ORDER BY a.channel_code`, pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ChannelAvailability{}
	for rows.Next() {
		var c ChannelAvailability
		if err = rows.Scan(&c.Channel, &c.Cap, &c.ReleaseAt, &c.Released,
			&c.OpensAt, &c.ClosesAt, &c.WindowOpen, &c.Held, &c.Confirmed); err != nil {
			return nil, err
		}
		// A released OR window-closed channel has nothing claimable, so both
		// report 0 — the same answer the claim path gives. They are separate
		// fields rather than one "unavailable" flag because they are different
		// facts with different remedies: a released allocation is over, a closed
		// window is not its turn yet, and only the second will fix itself.
		if !c.Released && c.WindowOpen {
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
