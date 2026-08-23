package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The voided-ticket feed (TKT-162, ADR-066): the refunded and exchanged tickets
// of one organizer, paged, so an offline scanner can refuse a voided ticket
// without a network round-trip.
//
// TKT-157 refuses a voided ticket at a LIVE gate; TKT-269 makes an offline admit
// visible after the fact. Neither stops it, because the scanner has no way to
// learn the ticket was voided. This is distribution only — the scanner owns local
// storage, freshness and the admission decision (TKT-271).
//
// Both voiding facts are carried. `refunded` and `exchanged` are already treated
// as one class at the live gate (`ticketCommerciallyVoid`), and an exchanged
// ticket is the sharper offline case: its replacement is live elsewhere, so
// admitting the original admits the exchange twice.

// VoidedTicket is one voided ticket and the position of the fact that voided it.
type VoidedTicket struct {
	TicketID uuid.UUID
	// OccurredAt and EventID are the keyset position, not information the scanner
	// needs — they travel back inside the opaque cursor.
	OccurredAt time.Time
	EventID    uuid.UUID
}

// VoidedCursor is the keyset position between pages.
//
// (occurred_at, event_id), because occurred_at alone is not unique: two tickets
// of one order are voided by a single refund inside one transaction and can share
// an instant exactly. The row that lost such a tie would be skipped. The event id
// breaks it deterministically, and both components are immutable because
// lifecycle events are append-only.
type VoidedCursor struct {
	OccurredAt time.Time
	EventID    uuid.UUID
	// Ceiling is the high-water mark: the newest instant the FIRST page was
	// allowed to see, carried unchanged through every later page.
	//
	// Without it a walk down a newest-first feed can never observe a void that
	// happens during the walk — the new event is newer than the cursor, and the
	// cursor only moves backwards, so every remaining page excludes it and the
	// scanner reaches next_cursor: null holding an incomplete list it believes is
	// complete (ai-review [high]). That is the precise failure this feed exists to
	// prevent, so it cannot be left to "the next sync will pick it up": the
	// scanner has no way to know it needs one.
	//
	// With the ceiling, a page walk is a consistent snapshot as of its first page.
	// Voids after that are simply not in this walk, and the scanner learns about
	// them on its next pull — which it knows to make, because freshness is a
	// clock, not a cursor.
	Ceiling time.Time
	// OrganizerID is the organizer the cursor was ISSUED for.
	//
	// A cursor is only a position, so it cannot read another organizer's rows —
	// the query filters on the authenticated organizer regardless. What it can do
	// unbound is SUPPRESS: a cursor copied from another organizer's page, or
	// forged with a future timestamp, makes the holder's own next page skip rows
	// or come back empty, with no error anywhere. For a revocation feed that is
	// the dangerous direction — silently missing revocations is exactly the state
	// this ticket exists to prevent. Binding it makes that a refusal instead.
	OrganizerID uuid.UUID
}

// IsZero reports the absence of a cursor: the first page on the way in, the last
// page on the way out.
func (c VoidedCursor) IsZero() bool {
	return c.OccurredAt.IsZero() && c.EventID == uuid.Nil
}

// voidedFeedQuery is a const so the ADR-019 scan-scope proof can EXPLAIN the
// exact statement production executes, rather than a lookalike that drifts.
//
// The relation order is `tickets → lifecycle_events` deliberately. The planner
// may reorder joins, but the query must offer an organizer-leading access path:
// the tempting alternative — an index on lifecycle_events (occurred_at DESC, id
// DESC) WHERE event_type IN (...) — optimises the GLOBAL voided stream and would
// read other organizers' voided rows before discarding them. That returns the
// right answer, so every assertion about the returned rows still passes; it is
// the scan, not the result, that is wrong, which is the precise defect ADR-019
// exists to catch.
//
// The keyset predicate is the row-value form `(occurred_at, id) < ($2, $3)`,
// which Postgres can drive straight off an index ordering; the equivalent
// `occurred_at < $2 OR (occurred_at = $2 AND id < $3)` is the same rows and a
// worse plan.
//
// $2/$3 are always bound: the first page passes a sentinel far in the future
// rather than switching to a second SQL string, because two statements mean the
// plan proof covers one of them.
const voidedFeedQuery = `
	SELECT e.ticket_id, e.occurred_at, e.id
	  FROM tickets AS t
	  JOIN lifecycle_events AS e ON e.ticket_id = t.id
	 WHERE t.organizer_id = $1
	   AND e.event_type IN ('refunded', 'exchanged')
	   AND e.occurred_at <= $4
	   AND (e.occurred_at, e.id) < ($2, $3)
	 ORDER BY e.occurred_at DESC, e.id DESC
	 LIMIT $5`

// feedOrganizerIndex is the index the scan-scope proof asserts the plan uses.
// Named here rather than spelled in the test so the migration, the query and the
// assertion cannot drift apart silently.
const feedOrganizerIndex = "tickets_organizer_feed_idx"

// VoidedFeedPageLimit bounds one page, and matches this service's existing batch
// bound (ReconcileRequest's maxItems) rather than inventing a second number for
// one API.
const VoidedFeedPageLimit = 100

// VoidedTickets returns one page of an organizer's voided tickets, newest first,
// and the cursor for the next page — or a zero cursor when the page is the last.
//
// `organizer` comes from the authenticated scanner device and nowhere else; this
// function has no opinion about who may ask, which is why the authorization check
// does not live here.
//
// It fetches limit+1 to learn whether more exist without a second count query: a
// count would be a second scan of the same index for information the extra row
// already carries.
//
// A note on what "bounded" means here, because the ADR states it as a limit
// rather than a guarantee: the RESPONSE is bounded by the limit, and the scan is
// bounded by the authenticated organizer's voided set — not by the page size. An
// organizer with a very large voided history pays a sort proportional to it.
// Closing that would need a denormalised feed or an organizer column on
// lifecycle_events, which is a bigger change than this ticket (ADR-066).
func (p *Postgres) VoidedTickets(ctx context.Context, organizer uuid.UUID, after VoidedCursor, limit int) ([]VoidedTicket, VoidedCursor, error) {
	if organizer == uuid.Nil {
		// Fails closed rather than reading every organizer's revocations. The
		// caller should never reach here without an authenticated organizer, and
		// if it does, an empty feed would be the wrong answer twice over: it
		// would look like "nothing is revoked".
		return nil, VoidedCursor{}, fmt.Errorf("voided feed requires an organizer")
	}
	if limit <= 0 || limit > VoidedFeedPageLimit {
		limit = VoidedFeedPageLimit
	}
	if !after.IsZero() && after.OrganizerID != uuid.Nil && after.OrganizerID != organizer {
		// Belt to the handler's braces. The handler refuses a foreign cursor with
		// a 400 before reaching here; this makes the store incapable of applying
		// one even if a future caller forgets.
		return nil, VoidedCursor{}, fmt.Errorf("voided feed cursor belongs to another organizer")
	}
	if after.IsZero() {
		// The first page starts above every possible event and sets the ceiling
		// for the whole walk.
		//
		// The keyset sentinel is a timestamp no lifecycle event can carry, not
		// time.Now(): a clock read here would race a ticket voided during this
		// very request and drop it from the first page a scanner ever pulls — the
		// one page that must be complete.
		//
		// The CEILING is p.now(), and that is a different decision from the
		// sentinel above rather than an inconsistency. It is the snapshot boundary:
		// everything at or before it belongs to this walk, everything after it
		// belongs to the next pull. Reading it here — once, on the first page — is
		// what makes the walk consistent.
		after = VoidedCursor{
			OccurredAt: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
			EventID:    uuid.Max,
			Ceiling:    p.now(),
		}
	}
	if after.Ceiling.IsZero() {
		// A cursor with no ceiling cannot be honoured as a snapshot, and silently
		// treating it as unbounded is how the gap this closes would come back. The
		// caller is a page walk that lost its boundary; make it start over.
		return nil, VoidedCursor{}, fmt.Errorf("voided feed cursor carries no ceiling")
	}

	rows, err := p.db.QueryContext(ctx, voidedFeedQuery, organizer, after.OccurredAt, after.EventID, after.Ceiling, limit+1)
	if err != nil {
		return nil, VoidedCursor{}, fmt.Errorf("read voided tickets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := make([]VoidedTicket, 0, limit)
	var next VoidedCursor
	for rows.Next() {
		var v VoidedTicket
		if err := rows.Scan(&v.TicketID, &v.OccurredAt, &v.EventID); err != nil {
			return nil, VoidedCursor{}, fmt.Errorf("scan voided ticket: %w", err)
		}
		if len(page) == limit {
			// The limit+1'th row is not returned; it only proves there is more.
			// The cursor is the LAST EMITTED row, so the next page resumes exactly
			// where this one stopped — taking it from the probe row instead would
			// skip a ticket, and a skipped ticket in a revocation feed is a
			// refunded holder walking through a gate.
			last := page[limit-1]
			next = VoidedCursor{
				OccurredAt: last.OccurredAt, EventID: last.EventID,
				OrganizerID: organizer,
				// Carried unchanged: the ceiling belongs to the WALK, not to the
				// page. Recomputing it here would reintroduce the gap one page at
				// a time.
				Ceiling: after.Ceiling,
			}
			break
		}
		page = append(page, v)
	}
	if err := rows.Err(); err != nil {
		return nil, VoidedCursor{}, fmt.Errorf("iterate voided tickets: %w", err)
	}
	return page, next, nil
}
