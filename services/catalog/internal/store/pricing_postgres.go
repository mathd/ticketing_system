package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Persistence for price-rule resolution (TKT-151 / ADR-036). The comparator
// itself is pure and lives in pricing.go; everything here is the half that
// needs a database.

// priceRuleWriteGateQuery is ADR-036 §3's integrity trade, paid back at the
// write path. price_rules.scope_id carries no foreign key -- the target table
// depends on scope_level -- so the store proves the target exists and belongs
// to the organizer, in the same INSERT ... SELECT shape catalog already uses
// for seat-map authoring. Exactly one UNION branch can produce a row, because
// each is guarded on scope_level. No match => no insert => ErrNotFound.
//
// This is honest-writer consistency, not tamper-evidence: a direct DB writer
// bypasses it entirely (ADR-021).
const priceRuleWriteGateQuery = `
INSERT INTO price_rules (organizer_id, scope_level, scope_id, action_kind,
                         amount, currency, priority, force_ancestor_override,
                         effective_from, effective_until, channel_code)
SELECT $1, $2, $3, 'absolute', $4, $5, $6, $7, $8, $9, $10
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
RETURNING id, organizer_id, scope_level, scope_id, action_kind, amount,
          currency, priority, force_ancestor_override,
          effective_from, effective_until, channel_code, created_at`

// pricingScopesQuery derives the five scope identities from a ticket type in
// one read (ADR-036 §1). The LEFT JOIN is what makes the series edge partial:
// series_performances.performance_id is UNIQUE, so it yields at most one row,
// and NULL when the slot belongs to no series.
const pricingScopesQuery = `
SELECT t.organizer_id, t.price_amount, t.currency,
       p.id, sp.series_id, p.event_id, p.venue_id
FROM ticket_types t
JOIN performances p ON p.id = t.performance_id AND p.organizer_id = t.organizer_id
LEFT JOIN series_performances sp ON sp.performance_id = p.id
WHERE t.id = $1`

// priceRuleCandidatesQuery is the exact statement production runs, referenced
// as a const so the ADR-019 plan assertion binds to it rather than to a
// hand-copied reduction that is free to drift (TKT-63's lesson).
//
// The predicate matches (scope_level, scope_id) PAIRS. Matching scope_id alone
// would be a correctness bug, not a shortcut: UUID uniqueness is per table, so
// an unrelated event's rule could be loaded as a candidate for a ticket type
// that happened to share its id (ADR-036 §3).
//
// It carries NO channel predicate either, and for a reason of the same shape
// (TKT-237). Filtering channel in SQL would be cheaper and would break two
// things: the resolver could no longer report a channel-agnostic rule as
// `less_channel_specific` (it would never see the winner it lost to), and adding
// `channel_code` to the WHERE clause pushes scope_id into a post-index Filter,
// reddening the ADR-019 plan assertion this const exists to bind. Channel
// eligibility is the resolver's, exactly as the window is. fee_rules made the
// same call in 0016 and its candidate query has no channel predicate either.
//
// It deliberately carries NO time predicate. Filtering by window in SQL would
// be cheaper and WRONG: an expired rule must still be loaded to be reported as
// outside_window_past, a future one as outside_window_future, and a future
// wrong-currency rule must still fail the resolution. The time filter is the
// resolver's (ADR-036 §4 step 2), and the scope pair is what bounds cardinality.
//
// A slot with no series passes SQL NULL for $4. The ('series', NULL) row
// comparison is unknown rather than true, so it matches nothing -- which is
// also why the parameter must be a typed nil and never uuid.Nil, or it would
// match a rule whose scope_id is the zero UUID.
const priceRuleCandidatesQuery = `
SELECT id, organizer_id, scope_level, scope_id, action_kind, amount,
       currency, priority, force_ancestor_override,
       effective_from, effective_until, channel_code, created_at
FROM price_rules
WHERE organizer_id = $1
  AND (scope_level, scope_id) IN (
        ('ticket_type', $2::uuid),
        ('slot',        $3::uuid),
        ('series',      $4::uuid),
        ('event',       $5::uuid),
        ('venue',       $6::uuid)
      )
ORDER BY id ASC`

// CreatePriceRule inserts a rule, refusing one whose scope_id does not name a
// real row of scope_level's kind owned by the organizer.
func (p *Postgres) CreatePriceRule(ctx context.Context, in PriceRuleInput) (PriceRule, error) {
	var r PriceRule
	err := p.db.QueryRowContext(ctx, priceRuleWriteGateQuery,
		in.OrganizerID, string(in.ScopeLevel), in.ScopeID, in.Amount, in.Currency,
		in.Priority, in.ForceAncestorOverride, in.EffectiveFrom, in.EffectiveUntil,
		in.ChannelCode,
	).Scan(&r.ID, &r.OrganizerID, &r.ScopeLevel, &r.ScopeID, &r.ActionKind, &r.Amount,
		&r.Currency, &r.Priority, &r.ForceAncestorOverride,
		&r.EffectiveFrom, &r.EffectiveUntil, &r.ChannelCode, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PriceRule{}, ErrNotFound
	}
	return r, err
}

// ResolveTicketTypePrice answers "what does this ticket type cost at `at`, and
// why" -- catalog's question since ADR-036 §6 amended ADR-002.
//
// Both reads run in one read-only REPEATABLE READ transaction. Under READ
// COMMITTED they would observe different snapshots, and a series attachment
// landing between them could make a single resolution internally inconsistent
// -- deriving scopes from one state and loading rules against another. This is
// snapshot coherence, not locking: a concurrent insert legitimately resolves
// either before or after the snapshot.
func (p *Postgres) ResolveTicketTypePrice(ctx context.Context, ticketTypeID uuid.UUID, channel *string, at time.Time) (RuleSelection, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return RuleSelection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		organizerID uuid.UUID
		base        Money
		scopes      PricingScopes
		seriesID    uuid.NullUUID
	)
	err = tx.QueryRowContext(ctx, pricingScopesQuery, ticketTypeID).Scan(
		&organizerID, &base.Amount, &base.Currency,
		&scopes.SlotID, &seriesID, &scopes.EventID, &scopes.VenueID)
	if errors.Is(err, sql.ErrNoRows) {
		return RuleSelection{}, ErrNotFound
	}
	if err != nil {
		return RuleSelection{}, err
	}
	scopes.TicketTypeID = ticketTypeID
	// Typed nil, never uuid.Nil -- see priceRuleCandidatesQuery.
	var seriesParam any
	if seriesID.Valid {
		id := seriesID.UUID
		scopes.SeriesID = &id
		seriesParam = id
	}

	rows, err := tx.QueryContext(ctx, priceRuleCandidatesQuery, organizerID,
		scopes.TicketTypeID, scopes.SlotID, seriesParam, scopes.EventID, scopes.VenueID)
	if err != nil {
		return RuleSelection{}, err
	}
	defer func() { _ = rows.Close() }()

	var rules []PriceRule
	for rows.Next() {
		var r PriceRule
		if err = rows.Scan(&r.ID, &r.OrganizerID, &r.ScopeLevel, &r.ScopeID, &r.ActionKind,
			&r.Amount, &r.Currency, &r.Priority, &r.ForceAncestorOverride,
			&r.EffectiveFrom, &r.EffectiveUntil, &r.ChannelCode, &r.CreatedAt); err != nil {
			return RuleSelection{}, err
		}
		rules = append(rules, r)
	}
	if err = rows.Err(); err != nil {
		return RuleSelection{}, err
	}

	sel, err := SelectPricingRule(at, PricingCandidates{BasePrice: base, Scopes: scopes, Rules: rules, Channel: channel})
	if err != nil {
		return RuleSelection{}, err
	}
	// The pure seam has no idea who owns this; the store does.
	sel.OrganizerID = organizerID
	return sel, nil
}
