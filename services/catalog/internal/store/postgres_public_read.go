package store

// The published read path: event, season and festival aggregates as the public
// contract exposes them. A scoped read is only scoped if an index backs the
// filter (ADR-019).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

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
	// Iteration errors reach the caller only through Err(); Close() answers for the
	// driver. Dropping one here silently narrows the id set, and the season then renders
	// missing events as though the organizer never attached them (TKT-184).
	if err := rows.Err(); err != nil {
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
	SELECT p.id, ` + publicPerformancesStartsAt + `, p.timezone, p.kind, p.event_id,
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
			&perf.ID, &startsAt, &perf.Timezone, &perf.Kind, &perf.EventID,
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
		// EventID comes from the row (TKT-306). It was left ZERO here while
		// publicPerformances filled it from the joined event, so a FestivalAggregate
		// consumer reading it got a nil UUID that looks populated rather than absent.
		// Harmless only for as long as nothing reads it — the festival payload does not
		// (server_public_read.go) — which is a trap rather than a design. Scanned
		// directly from performances.event_id: no join, and the column is already
		// indexed (migration 0007), so ADR-019's scoping is unchanged and
		// TestGetPublishedFestivalIsIndexScoped still holds.
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
