package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// Persistence for split-schedule resolution (TKT-216 / ADR-047). The comparator
// is pure and lives in splits.go; this is the half that needs a database.

// splitScheduleWriteGateQuery is ADR-036 §3's integrity trade, paid back at the
// write path for the third time: scope_id carries no foreign key because the
// target table depends on scope_level, so the store proves the target exists and
// belongs to the organizer.
//
// Honest-writer consistency, not tamper-evidence (ADR-021).
const splitScheduleWriteGateQuery = `
INSERT INTO split_schedules (organizer_id, scope_level, scope_id, fee_code, channel_code,
                             priority, force_ancestor_override, effective_from, effective_until)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
FROM (
    SELECT organizer_id FROM ticket_types WHERE $2 = 'ticket_type' AND id = $3
    UNION ALL
    SELECT organizer_id FROM performances WHERE $2 = 'slot'        AND id = $3
    UNION ALL
    SELECT organizer_id FROM series       WHERE $2 = 'series'      AND id = $3
    UNION ALL
    SELECT organizer_id FROM events       WHERE $2 = 'event'       AND id = $3
    UNION ALL
    SELECT organizer_id FROM venues       WHERE $2 = 'venue'       AND id = $3
) target
WHERE target.organizer_id = $1
RETURNING id`

// splitScheduleCandidatesQuery is the exact statement production runs, held as a
// const so the ADR-019 plan assertion binds to it rather than to a hand-copied
// reduction that is free to drift.
//
// The predicate matches (scope_level, scope_id) PAIRS. Matching scope_id alone
// would be a correctness bug: UUID uniqueness is per table, so an unrelated
// event's schedule could be loaded as a candidate for a ticket type that shares
// its id.
//
// No time predicate and no channel predicate, for the reasons the fee query
// documents: an expired schedule must still be LOADED so it can be reported as
// the answer to "why is this fee not being split", and a schedule for another
// channel is dropped in the resolver rather than in SQL so the two filters have
// one definition.
const splitScheduleCandidatesQuery = `
SELECT s.id, s.organizer_id, s.scope_level, s.scope_id, s.fee_code, s.channel_code,
       s.priority, s.force_ancestor_override, s.effective_from, s.effective_until,
       p.payee_id, p.share_bps, y.kind, y.display_name, y.external_reference
FROM split_schedules s
JOIN split_schedule_parts p ON p.schedule_id = s.id
JOIN payees y ON y.id = p.payee_id AND y.organizer_id = s.organizer_id
WHERE s.organizer_id = $1
  AND (s.scope_level, s.scope_id) IN (
        ('ticket_type', $2::uuid),
        ('slot',        $3::uuid),
        ('series',      $4::uuid),
        ('event',       $5::uuid),
        ('venue',       $6::uuid)
      )
ORDER BY s.id ASC, p.payee_id ASC`

// CreatePayee registers someone money can be owed to.
func (p *Postgres) CreatePayee(ctx context.Context, in Payee) (Payee, error) {
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO payees (organizer_id, kind, display_name, external_reference)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		in.OrganizerID, in.Kind, in.DisplayName, in.ExternalReference).Scan(&in.ID)
	return in, err
}

// CreateSplitSchedule writes a schedule and its parts in ONE transaction.
//
// It has to be one transaction, and not for tidiness: the balance trigger is
// DEFERRED, so a schedule is unbalanced for the whole of its own creation. Two
// transactions would mean the first one committing an unbalanced schedule, which
// is exactly the state the trigger exists to forbid.
func (p *Postgres) CreateSplitSchedule(ctx context.Context, in SplitSchedule) (uuid.UUID, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var id uuid.UUID
	err = tx.QueryRowContext(ctx, splitScheduleWriteGateQuery,
		in.OrganizerID, string(in.ScopeLevel), in.ScopeID, in.FeeCode, in.ChannelCode,
		in.Priority, in.ForceAncestorOverride, in.EffectiveFrom, in.EffectiveUntil).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	for _, part := range in.Parts {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO split_schedule_parts (schedule_id, payee_id, organizer_id, share_bps)
			VALUES ($1,$2,$3,$4)`, id, part.Payee.ID, in.OrganizerID, part.ShareBps); err != nil {
			return uuid.Nil, err
		}
	}
	// The balance check fires HERE, at commit, not at any statement above.
	if err = tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// loadSplitSchedules reads every candidate schedule for the derived scopes and
// reassembles the header/parts join into whole schedules.
//
// It takes a queryer rather than the pool so it can run inside the SAME
// repeatable-read transaction as the fee resolution: a schedule authored between
// the two reads would otherwise make one resolution internally inconsistent,
// splitting a fee that was resolved against a different snapshot.
func loadSplitSchedules(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, organizerID uuid.UUID, scopes PricingScopes) ([]SplitSchedule, error) {
	var seriesParam any
	if scopes.SeriesID != nil {
		seriesParam = *scopes.SeriesID
	}
	rows, err := q.QueryContext(ctx, splitScheduleCandidatesQuery, organizerID,
		scopes.TicketTypeID, scopes.SlotID, seriesParam, scopes.EventID, scopes.VenueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	byID := map[uuid.UUID]*SplitSchedule{}
	var order []uuid.UUID
	for rows.Next() {
		var (
			s    SplitSchedule
			part SplitPart
		)
		if err = rows.Scan(&s.ID, &s.OrganizerID, &s.ScopeLevel, &s.ScopeID, &s.FeeCode,
			&s.ChannelCode, &s.Priority, &s.ForceAncestorOverride, &s.EffectiveFrom,
			&s.EffectiveUntil, &part.Payee.ID, &part.ShareBps, &part.Payee.Kind,
			&part.Payee.DisplayName, &part.Payee.ExternalReference); err != nil {
			return nil, err
		}
		existing, ok := byID[s.ID]
		if !ok {
			s.Parts = nil
			byID[s.ID] = &s
			order = append(order, s.ID)
			existing = byID[s.ID]
		}
		part.Payee.OrganizerID = organizerID
		existing.Parts = append(existing.Parts, part)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SplitSchedule, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}
