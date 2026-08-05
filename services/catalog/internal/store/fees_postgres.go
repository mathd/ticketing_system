package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Persistence for fee-rule resolution (TKT-214 / ADR-046). The comparator itself
// is pure and lives in fees.go; everything here is the half that needs a
// database.

// feeRuleWriteGateQuery is ADR-036 §3's integrity trade, paid back at the write
// path. fee_rules.scope_id carries no foreign key -- the target table depends on
// scope_level -- so the store proves the target exists and belongs to the
// organizer, in the same INSERT ... SELECT shape price rules and seat-map
// authoring already use. Exactly one UNION branch can produce a row, because
// each is guarded on scope_level. No match => no insert => ErrNotFound.
//
// This is honest-writer consistency, not tamper-evidence: a direct DB writer
// bypasses it entirely (ADR-021).
const feeRuleWriteGateQuery = `
INSERT INTO fee_rules (organizer_id, scope_level, scope_id, fee_code, basis,
                       amount, rate_bps, currency, incidence, channel_code,
                       priority, force_ancestor_override,
                       effective_from, effective_until)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
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
RETURNING id, organizer_id, scope_level, scope_id, fee_code, basis, amount,
          rate_bps, currency, incidence, channel_code, priority,
          force_ancestor_override, effective_from, effective_until, created_at`

// feeRuleCandidatesQuery is the exact statement production runs, referenced as a
// const so the ADR-019 plan assertion binds to it rather than to a hand-copied
// reduction that is free to drift.
//
// The predicate matches (scope_level, scope_id) PAIRS. Matching scope_id alone
// would be a correctness bug, not a shortcut: UUID uniqueness is per table, so an
// unrelated event's rule could be loaded as a candidate for a ticket type that
// happened to share its id (ADR-036 §3).
//
// It deliberately carries NO time predicate and NO channel predicate. Both would
// be cheaper and WRONG:
//   - an expired rule must still be loaded to be reported as
//     outside_window_past, and a future one as outside_window_future;
//   - a wrong-currency rule must fail the resolution whether or not its window is
//     open and whether or not it belongs to the requested channel -- a rule
//     misconfigured for another channel is still misconfigured, and filtering it
//     out in SQL would hide it until a sale arrived on that channel.
//
// Cardinality is bounded by the scope pair, which is what the index serves.
//
// A slot with no series passes SQL NULL for $4. The ('series', NULL) row
// comparison is unknown rather than true, so it matches nothing -- which is also
// why the parameter must be a typed nil and never uuid.Nil, or it would match a
// rule whose scope_id is the zero UUID.
const feeRuleCandidatesQuery = `
SELECT id, organizer_id, scope_level, scope_id, fee_code, basis, amount,
       rate_bps, currency, incidence, channel_code, priority,
       force_ancestor_override, effective_from, effective_until, created_at
FROM fee_rules
WHERE organizer_id = $1
  AND (scope_level, scope_id) IN (
        ('ticket_type', $2::uuid),
        ('slot',        $3::uuid),
        ('series',      $4::uuid),
        ('event',       $5::uuid),
        ('venue',       $6::uuid)
      )
ORDER BY id ASC`

// CreateFeeRule inserts a rule, refusing one whose scope_id does not name a real
// row of scope_level's kind owned by the organizer.
func (p *Postgres) CreateFeeRule(ctx context.Context, in FeeRuleInput) (FeeRule, error) {
	var r FeeRule
	err := p.db.QueryRowContext(ctx, feeRuleWriteGateQuery,
		in.OrganizerID, string(in.ScopeLevel), in.ScopeID, in.FeeCode, string(in.Basis),
		in.Amount, in.RateBps, in.Currency, string(in.Incidence), in.ChannelCode,
		in.Priority, in.ForceAncestorOverride, in.EffectiveFrom, in.EffectiveUntil,
	).Scan(&r.ID, &r.OrganizerID, &r.ScopeLevel, &r.ScopeID, &r.FeeCode, &r.Basis,
		&r.Amount, &r.RateBps, &r.Currency, &r.Incidence, &r.ChannelCode, &r.Priority,
		&r.ForceAncestorOverride, &r.EffectiveFrom, &r.EffectiveUntil, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FeeRule{}, ErrNotFound
	}
	return r, err
}

// ResolveTicketTypeFees answers "which fees apply to this ticket type, in this
// channel, at `at`, and why".
//
// channel nil is the default/public context, where only channel-agnostic rules
// are eligible. Omitting the channel is NOT a wildcard.
//
// Both reads run in one read-only REPEATABLE READ transaction, for the same
// reason price resolution does: under READ COMMITTED a series attachment landing
// between them could make a single resolution internally inconsistent, deriving
// scopes from one state and loading rules against another. This is snapshot
// coherence, not locking.
func (p *Postgres) ResolveTicketTypeFees(ctx context.Context, ticketTypeID uuid.UUID, channel *string, at time.Time) (FeeSelection, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return FeeSelection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		organizerID uuid.UUID
		base        Money
		scopes      PricingScopes
		seriesID    uuid.NullUUID
	)
	// The scope derivation is shared with price resolution deliberately: the
	// five levels are ADR-036's, and two derivations of one hierarchy is how the
	// two answers start disagreeing about which series a slot is in.
	err = tx.QueryRowContext(ctx, pricingScopesQuery, ticketTypeID).Scan(
		&organizerID, &base.Amount, &base.Currency,
		&scopes.SlotID, &seriesID, &scopes.EventID, &scopes.VenueID)
	if errors.Is(err, sql.ErrNoRows) {
		return FeeSelection{}, ErrNotFound
	}
	if err != nil {
		return FeeSelection{}, err
	}
	scopes.TicketTypeID = ticketTypeID
	// Typed nil, never uuid.Nil -- see feeRuleCandidatesQuery.
	var seriesParam any
	if seriesID.Valid {
		id := seriesID.UUID
		scopes.SeriesID = &id
		seriesParam = id
	}

	rows, err := tx.QueryContext(ctx, feeRuleCandidatesQuery, organizerID,
		scopes.TicketTypeID, scopes.SlotID, seriesParam, scopes.EventID, scopes.VenueID)
	if err != nil {
		return FeeSelection{}, err
	}
	defer func() { _ = rows.Close() }()

	var rules []FeeRule
	for rows.Next() {
		var r FeeRule
		if err = rows.Scan(&r.ID, &r.OrganizerID, &r.ScopeLevel, &r.ScopeID, &r.FeeCode,
			&r.Basis, &r.Amount, &r.RateBps, &r.Currency, &r.Incidence, &r.ChannelCode,
			&r.Priority, &r.ForceAncestorOverride, &r.EffectiveFrom, &r.EffectiveUntil,
			&r.CreatedAt); err != nil {
			return FeeSelection{}, err
		}
		rules = append(rules, r)
	}
	if err = rows.Err(); err != nil {
		return FeeSelection{}, err
	}

	// The ticket type's currency is what every rule is validated against -- the
	// fee and the thing it is a fee on must be the same money.
	sel, err := SelectFeeRules(at, FeeCandidates{
		Currency: base.Currency, Scopes: scopes, Channel: channel, Rules: rules,
	})
	if err != nil {
		return FeeSelection{}, err
	}
	// The pure seam has no idea who owns this; the store does.
	sel.OrganizerID = organizerID
	return sel, nil
}
