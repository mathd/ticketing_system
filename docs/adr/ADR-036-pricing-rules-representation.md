# ADR-036: Pricing rules are typed declarative rows, not a custom DSL

Date: 2026-07-30

## Status

Accepted (TKT-150; decision taken under the owner-waived gates of that run, recorded on the
ticket). Deliverable of the TKT-5 spike. Consumed by TKT-151 (hierarchy + provenance), TKT-152
(effective windows), TKT-153 (the sale path).

*Supersedes, on one narrow point, the ordering implied by `docs/product/prd-v1.md:36` — see
§ Scope levels.*

***Amends [ADR-002](./ADR-002-services-from-day-one.md) on the pricing-evaluation boundary only** —
see § 6. ADR-002's five-service cut and every other ownership row stand unchanged.*

## Context

TKT-5 needs a price to resolve through a hierarchy with explicit overrides and to change over time
(early-bird tiers). Today the entire pricing model is one column: `ticket_types.price_amount
bigint` plus `currency char(3)` (`services/catalog/internal/store/migrations/0001_catalog_schema.sql`).
`commerce.(*Server).reserve` reads it through catalog's `/internal/ticket-types/{id}` and uses the
value verbatim for the inventory hold and the `reservations` row
(`services/commerce/internal/api/server.go:153,170`).

The owner — who has built three ticketing platforms — is open to a **custom DSL** for complex
event programming, and the PRD lists "DSL vs declarative rules" as an explicitly deferred
decision. The alternative is **declarative config**: typed rows with a fixed, engine-owned
evaluation order and no user-authored expression language.

This decision is needed *now*, before TKT-151, because it fixes the shape of production pricing
data. Changing it later is a migration over rows that priced real sales, not an edit.

Constraints that are not ours to choose:

- **ADR-002** — `catalog` owns rule definitions; `commerce` owns "pricing/fee/promo evaluation"
  (`ADR-002:42`). Whatever is chosen must land on one side of that line and say which — **or move
  the line explicitly, by amending ADR-002.** Asserting that a boundary change "does not really
  change the boundary" is not one of the options; § 6 takes the amendment route.
- **ADR-001** — money is integer minor units + ISO-4217; floats are banned on money paths.
- **ADR-009** — the OpenAPI document is the source of truth and drift fails the gate.
- **ADR-004 / ADR-019** — a catalog read that claims subset-proportional cost owes an index behind
  its filter, and two tests to prove it.
- **ADR-020** — plain `CREATE INDEX`; `CONCURRENTLY` is not adopted here.

Current state confirmed by reading the code: there is no pricing-rule schema, resolver, effective
window or multiplier anywhere in `catalog` or `commerce`. This is greenfield inside a built system.

## Possible Solutions

- **Option 1 — Custom DSL.** Operators author expressions (`when venue = X and before 2026-03-01
  then 4500`); catalog stores source text and an evaluator runs it.
    - Pros: authoring density — one program can describe every festival day, tier predicate,
      relative house policy and exception without row proliferation. A natural future vocabulary
      for the "complex event programming" the owner has in mind. Expressiveness is not bounded by
      what we anticipated.
    - Cons: grammar and AST versioning; a parser, type-checker and diagnostics good enough that an
      operator can fix their own mistake; sandboxing and resource bounds on evaluation; the source
      is an **opaque string** to OpenAPI, so the contract can validate nothing structurally
      (ADR-009 buys us nothing); and the evaluator must live in exactly one service or its
      semantics get duplicated — either choice is a second architectural decision this one would
      force. Nothing in TKT-151–153 needs user-authored expressions or control flow.
- **Option 2 — Typed declarative rows (chosen).** A rule is a row: one scope, one typed action,
  one optional effective window, an explicit priority and an override flag. Evaluation order is
  owned by the engine and is not authorable.
    - Pros: structurally validatable by the OpenAPI contract; index-backed lookup; a pure,
      table-driven resolver as the test seam; every failure mode is enumerable in a truth table.
    - Cons: row proliferation for cases a program would express once (see § Consequences); the
      expressible set is fixed by us, so a genuinely new pricing shape needs a schema change
      rather than an operator writing it.
- **Option 3 — Do nothing (keep the flat per-ticket-type price).** Rejected: it is TKT-5's
  problem statement. Recorded because it is the honest baseline — the flat price stays correct for
  every organizer who never authors a rule, and this decision must not break that.

## Decision

**We adopt Option 2: pricing rules are typed declarative rows.** A custom DSL is rejected *for
now* on cost, not on principle: its costs are all real and immediate, and its benefit —
authoring density — has no story asking for it yet.

The choice is deliberately built to be reversible. See § Escape hatch.

### 1. Scope levels, and where they come from

Five levels, each **derived** from the requested ticket type rather than stored on the rule:

| Level | Derivation from `ticket_type_id` |
|---|---|
| `ticket_type` | the id itself |
| `slot` | `ticket_types.performance_id` |
| `series` | the row in `series_performances` for that performance — **at most one** (`performance_id` is `UNIQUE`), and optional |
| `event` | `performances.event_id` |
| `venue` | `performances.venue_id` |

**Specificity order — narrowest first:**

    ticket_type  >  slot  >  series  >  event  >  venue

**This order is part containment and part policy, and the two must not be confused.** An earlier
draft of this ADR claimed the whole order was "derived from the schema". That was wrong, and the
distinction matters: a containment fact is checkable against the database, while a policy choice
is a decision someone can disagree with and must therefore be argued.

*Containment (checkable):*

- `ticket_type > slot` — `ticket_types.performance_id` is a `NOT NULL` FK
  (`0001_catalog_schema.sql:49`). Every ticket type belongs to exactly one slot.
- `slot > series` — a series is a *set* of slots (`series_performances`), so the slot is the
  narrower of the two. Membership is **optional and at most one** (`performance_id` is `UNIQUE`,
  `0005_series_seasons.sql:15`), which makes this a **partial** edge: a slot with no membership
  simply contributes no series candidate. (Written backwards in an earlier revision — `>` here
  means *narrower than*, and a set is not narrower than its members.)
- `series ⊂ event` — `series.event_id` is `NOT NULL REFERENCES events`
  (`0005_series_seasons.sql:7`), and `AttachPerformanceToSeries` refuses a performance whose
  `event_id` differs from the series' (`services/catalog/internal/store/postgres.go:187-201`).
  **Note the enforcement level:** `series_performances` carries only independent FKs to `series`
  and `performances`, so nothing in the *schema* stops a cross-event membership — the equality is
  enforced by application code alone. It is a real invariant of this codebase, not a database
  constraint, and a direct DB writer can violate it.

*Policy (a decision, not a fact):*

- **`event > venue` is a choice.** `performances` carries **independent** FKs to `event_id` and
  `venue_id` (`0001_catalog_schema.sql:32-33`): an event may span venues and a venue hosts many
  events, so the two are **incomparable** in the schema. The same applies to `series` vs `venue`.
  We rank event above venue because that matches operator intent: a venue rule is a *house
  default* ("nothing here sells under 20€"), and an event rule is a deliberate statement about one
  show. The narrower-intent rule should win. This is exactly the ranking an explicit override
  (§4 step 3) exists to invert when the house default is meant to be binding.

`docs/product/prd-v1.md:36` phrases the hierarchy as "venue → series → event → price level", which
puts series *above* event. That phrasing predates the schema, and **this ADR supersedes it.** Do
not "fix" the order back to match the PRD; if it is ever revisited, revisit the containment facts
against the foreign keys and the policy choice on its own merits.

**The hierarchy is a DAG, not a chain**, and the levels above are not the only parents in the
catalog. A slot is reachable from its event directly (`performances.event_id`) *and*, when it
belongs to one, through a series. A `festival_day` slot additionally carries
`performances.capacity_group_id` (`0006_festivals.sql`), and **seasons** have independent
many-to-many membership to both series and events (`season_series`, `season_events`,
`0005_series_seasons.sql:28-37`), so a season can reach the same performance by two paths.

The derivation table resolves this by producing **at most one scope id per level** — seasons and
festivals are deliberately *not* scope levels in v1 (§ Consequences) — which makes the candidate
set well-defined; the ordering above then totalises it. The truth table (§4) exercises the three
shapes that matter: a slot with no series, a slot in a series, and a festival day.

**Festival is not a scope level in v1**, and this is a named cost, not an oversight — see
§ Consequences.

### 2. What a rule may express

**Absolute price only.** The action is a tagged union with exactly one member today:

    action_kind = 'absolute'   →   amount (bigint, minor units) + currency (ISO-4217)

Justification: it serves every case the epic actually has (early-bird tiers, per-day festival
prices, a house price for a venue); it makes "one winner" mean one final unit price, so there is
no stacking order to define; and it introduces no percentage rounding and no multiplication
overflow. Deltas overlap the fee epic (TKT-6) and percentage reductions overlap promotions — both
have their own epics, and importing their semantics here would be this ticket deciding theirs.

The action is **tagged** even though the union has one member, so adding a relative action later
is an additive contract change rather than a breaking one.

**When a relative action is added**, it must be integer-safe: basis points, checked
multiplication, and a documented rounding direction. Floats stay banned (ADR-001). The strongest
argument against absolute-only is a genuinely relative house policy — *"everything at this venue
is +5%"* — which absolute rules can only express by duplicating values across heterogeneous ticket
types and rewriting them when base prices move. If that case turns up, the answer is to widen the
tagged union, **not** to reach for a DSL.

**Amount bounds are part of the shape, not an implementation detail.** `price_rules.amount` is
`bigint` with `CHECK (amount >= 0 AND amount <= 9007199254740991)`. The upper bound is not
arbitrary: the OpenAPI `Money.amount` schema caps at `9007199254740991`
(`services/catalog/api/openapi.yaml:1011-1022`) so every consumer, the storefront included, can
represent it exactly. A rule amount above that would be persisted happily and then fail runtime
response validation as a 500 (ADR-028) — a money path failing at read time because of a write we
allowed. Bound it at the write.

**A pre-existing gap this exposes, which is *not* fixed here.**
`ticket_types.price_amount` is `bigint NOT NULL CHECK (price_amount >= 0)`
(`0001_catalog_schema.sql:51`) with **no upper bound**, and it has never needed one because
`getTicketType` is a hand-mounted `/internal/` route outside the OpenAPI contract, so no validator
ever sees it. The new operation in §6 **is** contract-declared, so a legal existing base price
between `9007199254740992` and `MaxInt64` would make it 500 — which contradicts this ADR's own
promise that data with no rules resolves unchanged. The window is theoretical (no such row exists;
`gen_random_uuid`-era seed data is nowhere near it) but the contradiction is real. It is a
**pre-existing defect surfaced by this decision, not introduced by it**, so it is filed as its own
ticket rather than smuggled into TKT-151.

**Currency is not converted.** A rule's `currency` must equal the ticket type's. A mismatch is
invalid configuration and **fails the resolution loudly** — it is never treated as "this rule does
not apply", because silently skipping a misconfigured rule on a money path sells at the wrong
price and looks like nothing happened. §4 step 1 fixes *when* that check runs — earlier than you
would guess, but not on rules that can never apply again.

Note a distinct, pre-existing limitation: `commerce` currently rejects any offer whose currency is
not `EUR` (`services/commerce/internal/api/server.go:148`) while `catalog` stores arbitrary
three-letter codes. That is an implementation limitation of the sale path, **not** this model's
currency policy. Do not read one as the other.

**No ticket-type selector on broader rules.** A rule at scope X applies to every ticket type
beneath X. To price one tier differently, author a `ticket_type`-scoped rule. The tempting
alternative — a `ticket_type_id` selector on a venue-scoped rule — is useless in this schema:
`ticket_types` are per-performance (`ticket_types.performance_id`), so there is no stable "VIP"
identity that spans performances for such a selector to name. A cross-performance tier identity is
owed by a future catalog story; until it exists, a selector would be a column that can only ever
match one row.

### 3. Storage shape and the query that must be provable

Rules are **append-mostly rows** (§ *Append-mostly* below) in one `price_rules` table in
**catalog**, polymorphically scoped:

    id, organizer_id, scope_level, scope_id, action_kind, amount, currency,
    effective_from, effective_until, priority, force_ancestor_override, created_at

Resolution loads candidates with a single scoped read. **The predicate must match
`(scope_level, scope_id)` pairs — matching on `scope_id` alone is a correctness bug, not a
shortcut:**

```sql
SELECT ... FROM price_rules
WHERE organizer_id = $1
  AND (scope_level, scope_id) IN ( ('ticket_type',$2), ('slot',$3),
                                   ('series',$4), ('event',$5), ('venue',$6) )
```

backed by `CREATE INDEX price_rules_scope ON price_rules (organizer_id, scope_level, scope_id)` —
**plain `CREATE INDEX`** (ADR-020).

**Why the level must be in the predicate.** UUID uniqueness is per table, not global: nothing in
this schema prevents an `events.id` and a `ticket_types.id` from being equal. An untyped
`scope_id = ANY($2::uuid[])` would then load an *event*-scoped rule belonging to a different event
and offer it as a candidate, and the §4 comparator would happily rank it. The poison fixture is
one row:

    requested ticket_type.id = X ; requested performance.event_id = A
    unrelated event.id = X ; rule = (scope_level='event', scope_id=X)   → wrongly loaded

Today every id comes from `gen_random_uuid()`, so this is improbable rather than likely — which is
exactly the kind of "improbable" that becomes an incident once ids are ever imported, migrated or
supplied by a caller. Correctness by construction costs one index column. **TKT-151's poison-row
test owes this fixture**, so the property is pinned rather than assumed.

The rejected alternative — five nullable typed FK columns with an "exactly one is non-null" check
— preserves referential integrity but turns the lookup into a five-way `OR` or a `UNION ALL`,
whose generic plan is materially harder to pin. We take the integrity loss knowingly and pay it
back at the **write** path: the store validates that `scope_id` names a real row of the kind
`scope_level` claims, using the same `INSERT … SELECT` gate pattern catalog already uses for
seat-map authoring. A malformed rule is unrepresentable through the store even though the database
alone would permit it.

**ADR-019 evidence: the existing helper does not fit, and TKT-151 must generalize it.**
`explainGenericPlan` (`services/catalog/internal/store/season_smoke_test.go:201`) prepares a
statement with **exactly one** parameter — `uuid` or `uuid[]` — and this query has several. The
earlier claim that the helper "applies unchanged" was wrong. TKT-151 owes:

- a **generalized** `explainGenericPlan` (multi-parameter), extended in place rather than forked —
  ADR-019 is explicit that forking it is how one copy quietly stops asserting anything;
- both of ADR-019's tests: a poison-row **result-scope** test (the fixture above) *and* an
  `EXPLAIN`-under-`force_generic_plan` **scan** assertion over **the production query constant**,
  not a hand-copied reduction of it;
- a seeded rule set large enough, and `ANALYZE`d, that a **sequential scan is the wrong and
  expensive choice** — which is also the only condition under which the assertion can fail for the
  right reason. (Stated backwards in an earlier revision: seeding until a seq scan is *cheaper*
  would make the planner correct to choose it and the test wrong to forbid it. ADR-019 §2 is
  explicit about this.) Index-*compatibility* of the predicate shape is not the claim; an index
  scan under a blind plan is.

**Append-mostly, not immutable — the honest word.** A rule's *pricing* fields are immutable:
`scope_level`, `scope_id`, `action_kind`, `amount`, `currency`, `priority` and
`force_ancestor_override` never change after insert. Exactly one field is mutable —
`effective_until`, and only to **close** a window (move it earlier, never reopen or extend it).
Superseding a price means inserting a new rule and closing the predecessor's window, which reuses
the mechanism time already provides and needs no revision counter. Calling this "immutable rows"
would be wrong, and the imprecision hides a real requirement about **supersession under
concurrency**.

A supersession is *two* writes — close the predecessor, insert the successor — so it is one
transaction or it is a bug:

```sql
BEGIN;
  SELECT id FROM price_rules WHERE id = $1 FOR UPDATE;          -- serialize the pair
  UPDATE price_rules SET effective_until = $2
   WHERE id = $1
     AND effective_until IS NOT DISTINCT FROM $3                -- CAS on the value the caller read
     AND (effective_until IS NULL OR effective_until > $2);     -- and monotonically closing
  -- RowsAffected = 0  →  someone superseded this predecessor since the caller read it.
  --                      ROLLBACK. Do NOT insert a successor. Re-read and retry or report.
  INSERT INTO price_rules (...) VALUES (...);                   -- only if RowsAffected = 1
COMMIT;
```

**All three conditions are required, and each of the first two alone has been claimed sufficient
in an earlier revision of this ADR — wrongly.**

- The **monotonic** clause (`effective_until > $2`) stops a window being reopened or extended. It
  does *not* give single-successor semantics: T1 closes at `20` and inserts S1; T2 then closes at
  `10`, which still satisfies `20 > 10`, and inserts S2. Both successors are live from `20`
  onward. The row lock does not help — it serializes the two transactions, and the second one is
  still wrong.
- The **CAS** clause (`IS NOT DISTINCT FROM $3`, the value the caller read) is what closes that:
  T2's expected value was `NULL`, the row now holds `20`, so its update matches zero rows.
- **`RowsAffected = 0 → ROLLBACK`** is what turns "my close was a no-op" into "my successor must
  not exist". Without it the successor is inserted anyway and the CAS bought nothing.

The zero-row disposition is the half that is easiest to forget, and the CAS is the half that is
easiest to think you already have.

Combined with TKT-153 storing a full provenance **snapshot** rather than a bare rule id, the
record of what a buyer was charged stays true even after the rule is closed.

**Name the adversary (ADR-021).** This is **honest-writer consistency, not tamper-evidence.**
Immutable rows and stored snapshots protect the history against ordinary editing and against the
application; they prove nothing against someone who can write to catalog's database, who can
insert, rewrite or delete rule rows at will. Do not describe pricing provenance as "tamper-evident"
or, unqualified, as "auditable".

### 4. Conflict semantics — the total order

Given the candidate rules for the ≤5 derived scopes, at evaluation instant `at`:

1. **Currency check — on every loaded rule that is not permanently past, before the window
   filter.** A rule matching one of the derived scopes whose `currency` differs from the ticket
   type's fails the resolution (§2), **unless its window has already closed**
   (`effective_until <= at`), in which case it is inert and is reported as `outside_window_past`
   like any other expired rule.

   Both halves of that sentence are load-bearing, and the ADR got this wrong twice in opposite
   directions:

   - *Checking only surviving rules* (first revision) lets a misconfigured rule whose window has
     not yet opened sit silently until it opens, and then reprice mysteriously. That is the silent
     swallow §2 forbids.
   - *Checking every loaded rule* (second revision) is worse: a wrong-currency rule that expired
     years ago would fail every resolution for that scope **forever**, and it is unrecoverable —
     `currency` is immutable and `effective_until` can only be shortened (§3), so no write can
     rescue it. A dead row would become a permanent outage on a money path.

   Restricting the check to rules that are current **or still in the future** is the smallest rule
   that catches every misconfiguration that could *ever* apply, while letting a rule that can never
   apply again stay harmlessly dead. Stated exactly, because a looser phrasing invites the wrong
   implementation: a wrong-currency rule fails resolution during **any** resolution in which it
   could apply now **or at some future instant** — which includes a not-yet-open rule, deliberately.
   It is not "detected the moment it becomes capable of pricing"; it is detected the first time
   anyone resolves that scope while the rule still has a future. Write-time validation cannot do
   the job, because one venue-scoped rule spans many ticket types whose currencies may differ.
2. **Window filter.** Keep rules where `effective_from <= at < effective_until`; a null bound is
   unbounded. The interval is **half-open `[from, until)`**. A reversed window is unrepresentable:
   `CHECK (effective_from IS NULL OR effective_until IS NULL OR effective_from < effective_until)`.
   Without it an instant could be simultaneously "past" and "future" relative to one rule, and the
   two provenance reasons in §5 would both apply with no stated precedence.
3. **Override partition.** If any surviving rule has `force_ancestor_override = true`, the
   competition is **restricted to those rules** and the scope order is **inverted**:
   `venue > event > series > slot > ticket_type`. Otherwise the normal order applies:
   `ticket_type > slot > series > event > venue`.
4. **Same scope level:** the higher explicit `priority` wins.
5. **Equal priority:** lowest `id` (uuid ascending) wins.
6. **No surviving rule:** the ticket type's own `price_amount`/`currency` is the answer. Existing
   data with no rules resolves to today's price, unchanged.

Step 5 is deliberately semantically uninteresting. Operators express intent through `priority`;
the id tie-break exists only so that database order, insertion timestamps or a query plan can
never decide a price. (`created_at` would be the wrong tie-break: two rules inserted in one
transaction share `now()`.)

**"Explicit override" is defined by step 3 and by the truth table, not by the word.** The word
"override" is not otherwise defined anywhere in this repo, and the reading chosen here — *a forced
rule beats its descendants, and among forced rules the broadest wins* — is the "house rule you
cannot undercut" reading. It was chosen because it is the cheapest to reverse: it is one boolean
column and one branch in a pure function, so a different reading costs a comparator edit and a
truth-table row, with no schema change and no data migration. **This is the decision in TKT-5 most
likely to be wrong.** It is written down here precisely so that it is cheap to correct.

TKT-151 implements this as a pure function — `SelectPricingRule(at, candidates) RuleSelection` —
and its table-driven tests are the truth table. **The signature takes `at` from the start**, so
TKT-152 is purely additive: it fills in the window columns and the window rows, it does not
reshape the seam.

**Truth-table rows are owned by exactly one story.** The first draft of this ADR handed the whole
table to TKT-151 while also giving windows to TKT-152 — a contradiction that would have been
litigated at TKT-151's plan gate. Ownership is fixed here instead:

| Case | Expected | Owner |
|---|---|---|
| no rules | base price, `fallback_reason: no_eligible_rule` | TKT-151 |
| one rule at each level, in turn | that rule wins | TKT-151 |
| rules at several levels | narrowest wins; the others appear as `less_specific` | TKT-151 |
| slot **not** in any series | series level contributes no candidate; resolution unaffected | TKT-151 |
| slot **in** a series, plus an event rule | **series wins** (the ordering that contradicts the PRD) | TKT-151 |
| festival day with an event rule | the event rule applies; no festival-level candidate exists | TKT-151 |
| scope-id collision poison row (§3) | the foreign-scope rule is never loaded | TKT-151 |
| forced venue rule + ordinary event rule | forced venue rule wins; event rule is `forced_ancestor` (unforced, excluded by a forced **ancestor**) | TKT-151 |
| forced **event** rule + ordinary **venue** rule | forced event rule wins; venue rule is `excluded_by_forced_rule` (unforced, excluded by a forced **non-ancestor**) | TKT-151 |
| two forced rules at different levels | broader wins; the narrower is `lower_forced_scope` (both forced) | TKT-151 |
| two rules at the same level, different priority | higher priority; loser is `lower_priority` | TKT-151 |
| two rules, same level and priority | lowest id; loser is `stable_id_tiebreak` | TKT-151 |
| rule currency ≠ ticket type currency | resolution **errors** | TKT-151 |
| currency mismatch on a rule whose window is **closed** | **no error** — inert, reported `outside_window_past` | TKT-152 |
| currency mismatch on a rule whose window has **not opened yet** | resolution **errors** — it still has a future | TKT-152 |
| rule whose window closed before `at` | ineligible, reported `outside_window_past` | TKT-152 |
| rule whose window opens after `at` | ineligible, reported `outside_window_future` | TKT-152 |
| rule at `at == effective_until` exactly | ineligible (half-open) | TKT-152 |
| rule at `at == effective_from` exactly | eligible | TKT-152 |
| all tiers expired | falls back to base price, as the no-rule case | TKT-152 |

**TKT-152 ships the window columns *and* their semantics — TKT-151 ships neither.** An earlier
revision split them (columns in TKT-151, filter in TKT-152) and that was wrong twice over: it
contradicted both tickets as written (TKT-151 scopes time windows *out*; TKT-152's approach says
it "extends TKT-151's rule table with the window columns"), and it left an incoherent intermediate
state — a column that accepts a timed rule the shipped resolver ignores. All-`NULL` tests pass
whether or not step 2 exists, so nothing would have caught it. Columns and the filter that gives
them meaning land together.

What TKT-151 *does* owe is the seam: `SelectPricingRule(at, candidates)` takes the evaluation
instant from the start, and **step 2** degrades to a no-op when no rule carries a window. Step 1
does not: TKT-151 runs the currency check on every loaded rule, because with no windows in the
schema every rule is unbounded and therefore always capable of applying. TKT-152 is then additive
— a migration, a predicate, and the rows below — rather than a
reshaping of the signature every TKT-151 test is written against.

### 5. Provenance — what a resolution reports

```
resolver_version   int
evaluated_at       timestamptz         -- the instant resolution was evaluated against
base_price         Money               -- the ticket type's own price
resolved_price     Money               -- what the winner produced, or base_price
winner             null | {
    rule_id, scope_level, scope_id, action_kind, amount, currency,
    effective_from, effective_until, priority, forced }
candidates         [ { ...same identity fields..., reason } ]
fallback_reason    'no_eligible_rule'  -- present only when winner is null
```

`candidates` holds **every considered rule except the winner** — the winner appears once, in
`winner`, and never also in `candidates`. Stated explicitly because "candidates" and "the losers"
pull in opposite directions, and two literal implementations of a looser sentence would disagree.

`reason` is a closed enum, and it must be **total over §4** — every way a rule can lose needs a
value, or an implementer invents one:

| Reason | Lost because |
|---|---|
| `less_specific` | a narrower scope won under the normal order |
| `forced_ancestor` | **unforced**, excluded by step 3, and the winning forced rule is **broader in the §1 order** — the "a house rule overrode you" case |
| `excluded_by_forced_rule` | **unforced**, excluded by step 3, and the winning forced rule is **not broader** — narrower, or at the same scope level |
| `lower_forced_scope` | **forced**, but a broader forced rule beat it under the inverted order |
| `lower_priority` | same scope level, lower explicit priority |
| `stable_id_tiebreak` | same level and priority, higher id |
| `outside_window_past` | its window closed at or before `at` (`effective_until <= at` — the half-open boundary belongs here) |
| `outside_window_future` | its window opens after `at` |

"Broader" and "narrower" here mean **position in the §1 order**, not graph ancestry. That
distinction is deliberate: §1 makes `event` and `venue` incomparable in the schema, so ancestry
would leave the commonest case — a forced venue rule beating an ordinary event rule — with no
reason at all. The §1 order is total over the five levels, so "broader / not broader" always
decides, including when the two rules sit at the *same* level (which lands in
`excluded_by_forced_rule`).

The three forced-related reasons are **mutually exclusive by construction** — partition on *was
the loser forced?*, then on *is the winner its ancestor?* — so the mapping is a function, not a
judgement call. That precision was missing in an earlier revision, where `forced_ancestor` and
`lower_forced_scope` were given the same definition and the truth table then assigned
`forced_ancestor` to an *unforced* rule; two implementers reading it would have disagreed.

`excluded_by_forced_rule` was missing entirely from the first draft, and its absence is a good
example of why an enum needs a proof rather than a plausible list: an ordinary **venue** rule
losing to a forced **event** rule fits none of the other seven — it is not less specific (it is
broader), it is not forced, and it lost on neither priority, id, nor time. TKT-151's truth table
carries that row explicitly.

**Why report the losers.** Not because "which level won" is otherwise unknowable — `winner`
carries `scope_level`, so it answers that on its own. Two narrower reasons:

1. **It makes the test adversarial.** A resolver hard-coded to return the ticket-type rule
   satisfies every winner-only assertion. Asserting the whole selection object — who lost, and
   why — is what distinguishes a real comparator from one that got the easy cases right.
2. **It is the operational answer.** The single most common pricing support question is *"why is
   it showing 60 and not the early-bird 45?"*, and the useful reply is the losing row plus its
   reason. Window-ineligible rules are reported for exactly this: they lost on time rather than
   scope, they are already loaded, and reporting them costs one enum value.

### 6. Where evaluation runs — and the ADR-002 amendment that makes it legal

**Catalog is the single authority for the rule-resolved unit price.** Catalog stores the rules,
derives the scopes, selects the winner and returns `base_price`, the winning typed `action`, the
derived `resolved_price` and the provenance. **Commerce consumes `resolved_price` and does not
recompute it.**

**This is a service-boundary change, and it is recorded as one.** ADR-002's table assigns
"pricing/fee/promo evaluation" to `commerce` (`ADR-002:42`); putting rule selection *and* the
resolved unit price in `catalog` moves part of that. An earlier draft of this ADR asserted the
move "does not weaken ADR-002" — that was a dodge, not a resolution, and a reviewer was right to
reject it. Either the boundary moves explicitly or it does not move at all.

**Amendment to ADR-002 — scope: the pricing-evaluation row only.** "Pricing evaluation" in
ADR-002's table is narrowed to **sale-time composition**: assembling unit price + fees + promos +
taxes into an order total, and everything downstream of it (TKT-6, TKT-32, TKT-8, TKT-9). That
stays entirely in `commerce`. **Rule-based resolution of a ticket type's unit price moves to
`catalog`**, alongside the rule definitions ADR-002 already assigns there. Every other row of the
five-service cut is untouched. This follows the precedent of ADR-022 superseding ADR-008 on
*placement only* — a narrow, named amendment rather than a rewrite. A cross-reference is added to
ADR-002 so the amendment is discoverable from the decision it amends, not only from here.

**Why move it rather than split it.** Under an absolute-only action set, "applying" the action is
the identity function. Splitting it (catalog selects, commerce applies) would put the identity
function on the far side of a service boundary and create a second place the unit price can be
computed — a divergence bug waiting for its first mismatch, and one that would show up as two
different prices for the same offer. Shipping the typed `action` anyway means that if commerce
must one day apply relative actions itself, it already receives the input; the amendment would
then narrow again rather than needing a new contract.

**The honest cost of the amendment:** a boundary that has moved once is easier to argue into
moving again, and TKT-6/TKT-32/TKT-8 will each have a moment where pushing one more thing into
catalog looks convenient. The line to hold is the one stated above — catalog answers *"what does
this ticket type cost, and why"*, commerce answers *"what does this order total"*.

**The operation is declared in catalog's OpenAPI document** — not hand-mounted under `/internal/`
like `getTicketType` (`services/catalog/internal/api/server.go:104-115`). This matters more than
convention: the argument that decided DSL-vs-config was that a structured shape can be validated
by the contract. Hiding it behind an undeclared internal route would discard the exact benefit
that won the decision. It therefore carries the full contract-first cost: OpenAPI schema,
regenerated `openapi_gen.go` / `api-types.gen.ts` **committed before the gate runs**, and coverage
in `services/catalog/internal/api/coverage_test.go` (catalog is out of the smoke happy-path gate —
ADR-030).

**Cache tier (ADR-004): `no-store` at sale time.** This is a correctness argument, not a
performance one. A resolved price feeds a money decision, which is ADR-004's "never" tier; and
once TKT-152 adds windows, the response's correctness *expires at a known instant*, so caching it
past `effective_until` serves a stale price to a buyer. If a public browse-time price read is
later wanted, it is a **different** endpoint, and its `max-age` must be bounded by the distance to
the next window boundary.

**Not event-bound.** Neither rules nor resolved prices enter a domain event in TKT-151–153;
`OrderCompletedData` carries identifiers and quantity, not price. Adding either later is an
ADR-017 decision (bump on consumer semantics), not a payload edit.

### 7. Escape hatch

If the owner later wants the DSL, **the declarative shape becomes the compilation target**:
catalog stores and validates DSL source, compiles it to typed rows, and commerce keeps consuming
the same contract. No second evaluator and no duplicated semantics across services.

**Stated precisely, because the first draft overclaimed it.** This is not "reversible at the price
of a compiler". A row expresses *one unconditional absolute result for one scope*; a DSL worth
having expresses conditions and control flow, which these rows cannot represent — the ADR itself
says membership-conditional pricing has no home here. So a real DSL needs the row shape *widened*
(predicates, a richer action union) and the resolver extended to match. What the escape hatch
actually buys is narrower and still worth having:

- **no data migration** — existing rows remain valid instances of the widened shape;
- **no second evaluator** and no cross-service semantics duplication;
- **no change to the sale path** — commerce keeps consuming `resolved_price` + provenance whatever
  the authoring surface becomes.

The compiler is the cheap part; widening the IR is the real work, and it is work this decision
defers rather than avoids.

What this decision makes hard, honestly:

- **Authoring density.** A festival with per-day prices needs one rule per day. A program would
  say it once.
- **Relative policies.** Not expressible until the tagged union is widened.
- **Conditional logic** ("this price only if the buyer holds a membership") has no home at all —
  that is entitlement/promo territory, and by design it is not smuggled in here.

## Consequences

- **Positive:**
    - TKT-151 has an executable specification: the §4 truth table is its test suite, and
      `SelectPricingRule(at, candidates)` is a pure function that needs no database to test.
    - The contract is structurally validatable, so a malformed rule is rejected at the boundary
      rather than at evaluation time on a money path.
    - Organizers with no rules keep today's behaviour exactly — the base price is the documented
      fallback, not an accident.
    - The chosen shape is the compilation target a future DSL would need, so reversing this
      decision costs a compiler **plus** a widening of the row shape (§7) — but no data migration
      and no change to the sale path. That is a smaller promise than "reversible at the price of a
      compiler", and it is the one this ADR can keep.
    - The lookup is one index-backed scoped query over `(scope_level, scope_id)` pairs. Proving it
      needs `explainGenericPlan` generalized to several parameters — a precedent to extend, not one
      to reuse as-is.
- **Negative:**
    - **Festival-wide pricing has no scope level.** A festival's days are slots joined by
      `performances.capacity_group_id`; pricing all of them together means either one rule per day
      or a rule at their common `event`/`venue`. This is the clearest row-proliferation cost of
      rejecting a DSL, and the extension path is a sixth scope level derived from
      `capacity_group_id` — deferred because no story needs it yet.
    - **Per-tier pricing across many slots** costs one rule per ticket type until a stable
      cross-performance tier identity exists.
    - Polymorphic scoping trades database-enforced referential integrity for a provable query
      plan; the guarantee moves to the store's write path, where it is code and can be bypassed by
      a direct DB writer.
    - `resolver_version` in provenance is a commitment: changing the comparator is a versioned
      change, because stored snapshots must remain interpretable.
    - A one-day document spike cannot validate authoring UX with real operators. DSL pressure will
      return when bulk editing and complex programming get concrete stories, and that is the
      moment to re-open this, not before.

## References

- TKT-150 (this spike), TKT-5 (epic), TKT-151 / TKT-152 / TKT-153 (dependents)
- [ADR-001](./ADR-001-go-typescript-stack.md) — money is integer minor units; floats banned
- [ADR-002](./ADR-002-services-from-day-one.md) — the five-service cut; **amended here** on the
  pricing row only: catalog owns rule definitions *and* unit-price resolution, commerce owns
  sale-time composition (§6)
- [ADR-004](./ADR-004-cache-first-read-path.md) — volatility-tiered TTLs
- [ADR-009](./ADR-009-contract-first-apis.md) — contract-first, drift fails the gate
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) — if this ever becomes event-bound
- [ADR-019](./ADR-019-catalog-read-path-scoping.md) — the two tests a scoped read owes
- [ADR-020](./ADR-020-catalog-index-build-concurrency.md) — plain `CREATE INDEX`
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary before claiming integrity
- [ADR-030](./ADR-030-catalog-coverage-gate-scope.md) — catalog covers its operations in its own suite
- `docs/product/prd-v1.md` — TKT-5 capability row; superseded on the hierarchy ordering
