package store

// Performances, ticket types and slot lifecycle: create, publish, archive and
// the close/reopen toggle. Every state-deriving transition decides under a row
// lock and emits after commit (ADR-018).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

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
	// Fingerprinted over the NORMALIZED values — `kind` and `mode` after
	// defaulting, not as submitted. Fingerprinting the raw request would make
	// `kind: ""` and `kind: "performance"` two fingerprints for one identical
	// row, so a replay of a semantically identical request would 409.
	//
	// starts_at is normalized HERE, to the precision timestamptz keeps, and the
	// same value then feeds both the fingerprint and the INSERT below. Two
	// representations of one instant is exactly how a retry ends up conflicting
	// with its own original (ai-review pass 2); one value cannot.
	if in.StartsAt != nil {
		normalized := normalizeStartsAt(*in.StartsAt)
		in.StartsAt = &normalized
	}
	print := performanceFingerprint(in, kind, mode)

	var id uuid.UUID
	idempotencyBarrier()
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO performances
		   (organizer_id, event_id, venue_id, kind, starts_at, operating_date,
		    opens_at, closes_at, timezone, re_entry_mode, max_entries, requires_exit, seat_map_id,
		    idempotency_key, request_fingerprint)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 ON CONFLICT (organizer_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		 RETURNING id`,
		in.OrganizerID, in.EventID, in.VenueID, kind, in.StartsAt, in.OperatingDate,
		in.OpensAt, in.ClosesAt, in.Timezone, mode, in.ReEntry.MaxEntries, in.ReEntry.RequiresExit, in.SeatMapID,
		nullableKey(in.IdempotencyKey), nullableFingerprint(in.IdempotencyKey, print)).
		Scan(&id)
	// No row means the key is already taken — the signal to replay, not a
	// failure. See CreateEvent for why this branch must precede the generic one.
	if errors.Is(err, sql.ErrNoRows) {
		existing, found, match, lookupErr := replayLookup(ctx, p.db, "performances", in.OrganizerID, in.IdempotencyKey, print)
		if lookupErr != nil {
			return Performance{}, fmt.Errorf("replay performance: %w", lookupErr)
		}
		if !found {
			// See CreateEvent.replayEvent: ErrNotFound so this surfaces as the
			// declared 404 rather than a 500. Unreachable through the service —
			// catalog archives and never deletes these rows.
			return Performance{}, fmt.Errorf("replayed performance: %w", ErrNotFound)
		}
		if !match {
			return Performance{}, ErrIdempotencyConflict
		}
		id = existing
		perf, _, _, err := p.getPerformance(ctx, id)
		if err != nil {
			return Performance{}, err
		}
		return perf, nil
	}
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
	// Money stays integer minor units + ISO code all the way into the hash
	// (ADR-001): the amount is rendered with strconv, never formatted as a
	// float, so nothing on this path has ever been one.
	print := fingerprint(in.PerformanceID.String(), string(name), fingerprintInt(in.PriceAmount), in.Currency)

	idempotencyBarrier()
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO ticket_types (organizer_id, performance_id, name, price_amount, currency,
		                           idempotency_key, request_fingerprint)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (organizer_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		 RETURNING id, created_at`,
		in.OrganizerID, in.PerformanceID, name, in.PriceAmount, in.Currency,
		nullableKey(in.IdempotencyKey), nullableFingerprint(in.IdempotencyKey, print)).
		Scan(&tt.ID, &tt.CreatedAt)
	// No row means the key is already taken — replay, not failure. See
	// CreateEvent for why this branch must precede the generic one.
	if errors.Is(err, sql.ErrNoRows) {
		existing, found, match, lookupErr := replayLookup(ctx, p.db, "ticket_types", in.OrganizerID, in.IdempotencyKey, print)
		if lookupErr != nil {
			return TicketType{}, fmt.Errorf("replay ticket type: %w", lookupErr)
		}
		if !found {
			// See CreateEvent.replayEvent.
			return TicketType{}, fmt.Errorf("replayed ticket type: %w", ErrNotFound)
		}
		if !match {
			return TicketType{}, ErrIdempotencyConflict
		}
		// A replay creates nothing, so it must NOT re-announce public-read
		// invalidation below: the listability change was announced by the call
		// that actually inserted.
		return p.GetTicketType(ctx, existing)
	}
	if err != nil {
		return TicketType{}, fmt.Errorf("insert ticket type: %w", err)
	}
	// Pricing a previously unpriced published slot makes it publicly listable,
	// so this changes list membership as well as detail. Autocommit: announced
	// after the insert succeeded.
	p.notifyPublicRead(PublicReadAll)
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

func (p *Postgres) PublishPerformance(ctx context.Context, organizerID, id uuid.UUID) (Performance, bool, error) {
	// The transition is gated on a sellable offer existing (ErrNotSellable
	// otherwise): the publication event and public visibility must never
	// disagree. Single atomic statement flips draft->published exactly
	// once; the returned row also says whether the domain event is owed.
	res, err := p.db.ExecContext(ctx,
		`UPDATE performances SET status = 'published', published_at = now()
		 WHERE id = $1 AND organizer_id = $2 AND status = 'draft'
		   AND capacity_group_id IS NULL
		   AND EXISTS (SELECT 1 FROM ticket_types t WHERE t.performance_id = $1)`, id, organizerID)
	if err != nil {
		return Performance{}, false, fmt.Errorf("publish: %w", err)
	}
	flipped, _ := res.RowsAffected()
	if flipped > 0 {
		// Autocommit, so there is no commit to hang the notification on — and
		// this is the single most important write for the cached public reads.
		// Announced HERE, before the canonical re-read below: the row is already
		// public, and if that re-read failed the invalidation must still have
		// happened or the event stays invisible for a full tier.
		p.notifyPublicRead(PublicReadAll)
	}
	if flipped == 0 {
		// Nothing flipped: not found, NOT OURS, already published, or unpriced
		// draft. The organizer is re-asserted before classifying, because the
		// classification below is itself an information channel: it answers
		// ErrGroupedSlotLifecycle / ErrNotSellable / ErrIllegalTransition, each of
		// which reveals the row's state. Scoping only the UPDATE above would
		// refuse the write and still tell a cross-tenant caller what the victim's
		// slot is doing (TKT-251). One scoped existence check collapses "no such
		// slot" and "not yours" into the same ErrNotFound.
		var ours bool
		if err = p.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM performances WHERE id = $1 AND organizer_id = $2)`,
			id, organizerID).Scan(&ours); err != nil {
			return Performance{}, false, err
		}
		if !ours {
			return Performance{}, false, ErrNotFound
		}
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
	        f.shared_capacity, p.seat_map_id, COALESCE(sm.orphan_prevention_enabled, false)`

// The seat-map join is on p.seat_map_id — the EXACT bound version (TKT-183). Joining
// the map FAMILY and taking its current version would look identical until someone
// edits the map, and then every emission would silently describe a version the slot is
// not bound to (ADR-029). LEFT, because a GA slot has no map and must stay a GA slot.
const performanceFrom = `FROM performances p JOIN venues v ON v.id = p.venue_id
	 LEFT JOIN festivals f ON f.id = p.capacity_group_id
	 LEFT JOIN seat_maps sm ON sm.id = p.seat_map_id`

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
		&archiveEmitted, &perf.CreatedAt, &perf.Capacity, &sharedCapacity, &seatMap,
		&perf.OrphanPreventionEnabled)
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

// ListOrphanPreventionCandidates returns fully-hydrated published performances bound to
// a seat-map version with the rule ON (TKT-183), keyset-paginated by id.
//
// The predicate has no "already corrected" column, deliberately. Correction state would
// be a second source of truth about something only inventory can actually confirm, and a
// catalog row reading "corrected" while inventory's pool has no projection is worse than
// no row at all. Convergence comes from the identity instead: a re-run re-emits the SAME
// deterministic id, which inventory's consumed_events and JetStream's dedup window
// absorb as no-ops. So this stays a full reconciliation that is safe to re-run — and
// that is what closes ADR-041's rolling-deployment race, since a slot published at
// schema 4 by an undrained old replica is simply picked up by the next run.
//
// Archived slots are excluded: the wave repairs the rule setting of live inventory, and
// an archived pool has none to repair.
//
// One-shot operator read, not a hot path (ADR-019): no index is added for it.
func (p *Postgres) ListOrphanPreventionCandidates(ctx context.Context, after *uuid.UUID, limit int) ([]Performance, error) {
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
		 WHERE p.status = 'published' AND p.seat_map_id IS NOT NULL
		   AND sm.orphan_prevention_enabled `+cursor+`
		 ORDER BY p.id LIMIT $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("list orphan prevention candidates: %w", err)
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
func (p *Postgres) ArchivePerformance(ctx context.Context, organizerID, id uuid.UUID) (Performance, bool, bool, error) {
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
		 FROM performances WHERE id = $1 AND organizer_id = $2 FOR UPDATE`, id, organizerID).Scan(&status, &capacityGroup, &closureVersion, &closureEmitted)
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
	if err := p.commitPublicRead(tx, PublicReadAll); err != nil {
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

func (p *Postgres) CloseSlot(ctx context.Context, organizerID, id uuid.UUID, reason *string) (Performance, bool, bool, error) {
	return p.toggleClosure(ctx, organizerID, id, "closed", reason)
}

func (p *Postgres) ReopenSlot(ctx context.Context, organizerID, id uuid.UUID) (Performance, bool, bool, error) {
	return p.toggleClosure(ctx, organizerID, id, "open", nil)
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
func (p *Postgres) toggleClosure(ctx context.Context, organizerID, id uuid.UUID, target string, reason *string) (Performance, bool, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Performance{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, closureStatus string
	var version, emittedVersion int32
	err = tx.QueryRowContext(ctx,
		`SELECT status, closure_status, closure_version, closure_emitted_version
		 FROM performances WHERE id = $1 AND organizer_id = $2 FOR UPDATE`, id, organizerID).
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
