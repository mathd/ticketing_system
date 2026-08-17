package store

// Operator sweep for misconfigured rule currencies (TKT-243).
//
// WHY THIS EXISTS. TKT-237 moved channel eligibility ahead of the currency check
// in SelectPricingRule (pricing.go:264-296), and it had to: a currency mismatch
// aborts the whole resolution, so one misconfigured `pos` rule was returning 500
// for every `reseller` request and every public one — a cross-channel outage,
// and on a public endpoint an oracle for a rule the channel filter exists to
// hide. The correct order has a cost, and this file is that cost paid down: a
// wrong-currency rule on a channel nobody is currently buying through is now
// invisible to resolution until a sale arrives on that channel, and then it
// fails closed at the worst possible moment. This sweep finds it first.
//
// It REPORTS. It is not a gate, not a write path, and not an integrity control
// (ADR-021 — name the adversary): it reads the same tables a writer with
// database access writes, so it is an operator aid under honest-writer
// assumptions and nothing more.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// RuleKind says which table a finding came from. Price and fee rules have the
// same scope/channel/window shape and the same blind spot, so one sweep covers
// both and the caller needs to know which is which to go fix it.
//
// Split schedules are deliberately absent: they have no currency column at all
// (0017_payees_and_split_schedules.sql:57-59 — shares are basis points, which
// are currency-independent), so there is no mismatch to find.
type RuleKind string

const (
	RuleKindPrice RuleKind = "price"
	RuleKindFee   RuleKind = "fee"
)

// RuleCurrencyMismatch is one (rule, ticket type) PAIR whose currencies
// disagree.
//
// The pair — not the rule — is the unit, and that is not a modelling
// preference. A rule attaches to one of five scope levels with no foreign key
// (ADR-036 §3), so a venue-scoped rule covers every ticket type under that
// venue, each carrying its own ticket_types.currency. One rule can be correct
// for one ticket type and wrong for another simultaneously, which is the same
// asymmetry ADR-036 §4 step 1 names as the reason write-time validation cannot
// do this job.
type RuleCurrencyMismatch struct {
	Kind               RuleKind
	RuleID             uuid.UUID
	OrganizerID        uuid.UUID
	TicketTypeID       uuid.UUID
	ScopeLevel         string
	ScopeID            uuid.UUID
	FeeCode            string // fee rules only; empty for price rules
	RuleCurrency       string
	TicketTypeCurrency string
	ChannelCode        *string // nil = channel-agnostic
	EffectiveFrom      *string
	EffectiveUntil     *string
}

// ruleCurrencyMismatchQuery is the sweep, parameterized by the rule table it
// reads. Declared as a const so what production runs is what a reader reviews,
// following the convention priceRuleCandidatesQuery set (pricing_postgres.go:57).
//
// It INVERTS pricingScopesQuery (pricing_postgres.go:50-56). That query takes one
// ticket type and derives its five scope identities; this one derives those
// identities for every ticket type and joins the rule table on the matching
// (scope_level, scope_id) pair. One set-based statement — a per-rule fan-out in
// Go would issue a query per rule and still have to solve the same join.
//
// Three predicates, and each is load-bearing:
//
//   - the scope PAIR, never scope_id alone. UUID uniqueness is per table, so an
//     unrelated event's rule could otherwise match a ticket type that happens to
//     share its id — "a correctness bug, not a shortcut" (ADR-036 §3).
//   - r.organizer_id = t.organizer_id. Rules carry no FK to their scope target,
//     so this is the only thing preventing a cross-tenant pair when two
//     organizers share a scope id (ADR-002). It is also free: both
//     price_rules_scope and fee_rules_scope lead with organizer_id.
//   - effective_until IS NULL OR effective_until > now(). See below.
//
// And two deliberate OMISSIONS, which are what make this sweep worth having:
//
//   - NO channel predicate and NO join to `channels`. The registry is a lookup,
//     not a constraint (0018_channels.sql:10-17): a code that was never
//     registered sells exactly like one that was. Consulting the registry would
//     systematically miss the rules most likely to be misconfigured, and
//     filtering by channel would reproduce the very blind spot TKT-237 created.
//   - NO effective_from predicate. A rule whose window has not opened yet is
//     precisely what we are hunting — it will price the moment it opens and
//     nothing today would notice (ADR-036 §4 step 1 catches it "deliberately").
//
// The one time predicate that IS here excludes rules whose window has already
// CLOSED, and dropping it would be a real defect rather than extra diligence.
// ADR-036 §4 step 1 and ADR-046 §8 both say a closed-window rule is inert and
// must not be failed on, because doing so would be "permanent and unrecoverable,
// since currency is immutable and effective_until only shortens". An operator
// handed such a finding can do nothing with it: they cannot change the currency,
// reopen the window, or rescue the row by any write. Reporting it is noise that
// grows without bound as rules retire, and a report full of unfixable rows
// teaches operators to ignore the fixable ones beside it.
//
// One-shot operator read, not a hot path (ADR-019): no index is added for it,
// following ListOrphanPreventionCandidates (postgres.go:803) and
// ListPublishedUngroupedPerformances (postgres.go:844). The ORDER BY is total
// (id is the primary key) so two runs over unchanged data produce identical
// reports and an operator can diff them.
func ruleCurrencyMismatchQuery(table, feeCodeExpr string) string {
	return `
SELECT r.id, r.organizer_id, t.id, r.scope_level, r.scope_id, ` + feeCodeExpr + `,
       r.currency, t.currency, r.channel_code,
       to_char(r.effective_from  AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
       to_char(r.effective_until AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
FROM ticket_types t
JOIN performances p ON p.id = t.performance_id AND p.organizer_id = t.organizer_id
LEFT JOIN series_performances sp ON sp.performance_id = p.id
JOIN ` + table + ` r
  ON r.organizer_id = t.organizer_id
 AND (r.scope_level, r.scope_id) IN (
       ('ticket_type', t.id),
       ('slot',        p.id),
       ('series',      sp.series_id),
       ('event',       p.event_id),
       ('venue',       p.venue_id)
     )
WHERE r.currency <> t.currency
  AND (r.effective_until IS NULL OR r.effective_until > now())
ORDER BY r.organizer_id, t.id, r.id`
}

// ListRuleCurrencyMismatches reports every (rule, ticket type) pair, across
// price and fee rules, whose currencies disagree and whose rule can still apply.
//
// Read-only. It answers a question; it decides nothing.
func (p *Postgres) ListRuleCurrencyMismatches(ctx context.Context) ([]RuleCurrencyMismatch, error) {
	out, err := p.sweepRuleCurrency(ctx, RuleKindPrice, ruleCurrencyMismatchQuery("price_rules", `''`), nil)
	if err != nil {
		return nil, err
	}
	return p.sweepRuleCurrency(ctx, RuleKindFee, ruleCurrencyMismatchQuery("fee_rules", `r.fee_code`), out)
}

func (p *Postgres) sweepRuleCurrency(ctx context.Context, kind RuleKind, query string, into []RuleCurrencyMismatch) ([]RuleCurrencyMismatch, error) {
	rows, err := p.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sweep %s rule currencies: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		m := RuleCurrencyMismatch{Kind: kind}
		var from, until sql.NullString
		if err := rows.Scan(&m.RuleID, &m.OrganizerID, &m.TicketTypeID, &m.ScopeLevel, &m.ScopeID,
			&m.FeeCode, &m.RuleCurrency, &m.TicketTypeCurrency, &m.ChannelCode, &from, &until); err != nil {
			return nil, fmt.Errorf("scan %s rule mismatch: %w", kind, err)
		}
		if from.Valid {
			m.EffectiveFrom = &from.String
		}
		if until.Valid {
			m.EffectiveUntil = &until.String
		}
		into = append(into, m)
	}
	// Without this a truncated read reports "no mismatches" — the one output
	// this command must never produce when it has not actually read the table.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s rule mismatches: %w", kind, err)
	}
	return into, nil
}
