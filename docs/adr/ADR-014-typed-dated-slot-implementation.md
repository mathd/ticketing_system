# ADR-014: Typed dated-slot implementation choices

Date: 2026-07-14

## Status

Accepted

## Context

ADR-005 (amended by the TKT-50/US-008 spike) decided that a performance is one
kind of dated slot, and that festival days and park operating days share the
same catalog machinery via a `kind` discriminator plus attributes (operating
window, re-entry policy, closure). US-009 (TKT-51) implements that shape. The
spike fixed the *rules*; several *representation* choices were explicitly left
to implementation. This ADR records them so later verticals (passes TKT-12,
lodging TKT-13, festivals TKT-14) build on stable ground.

## Possible Solutions

- **Table/endpoint naming — keep `performances` vs rename to `slots`:**
    - Rename pros: the URL/table name matches the ADR-005 "slot" concept.
    - Rename cons: churns all five services' generated models + the storefront
      TS types + the smoke suite, and breaks the committed M1 contract, for zero
      functional gain — the `performances` table *is* the slot table.
- **Attribute storage — jsonb `re_entry_policy` vs typed columns:**
    - jsonb pros: one column, flexible.
    - jsonb cons: no DB-level validation of mode/max_entries; invalid catalog
      state is representable.
- **Event evolution — bump `Schema` 2→3 vs add `kind` additively at Schema 2:**
    - A bump cons: inventory's consumer hard-switches on `Schema == 2` and
      rejects 3, so a bump would stop provisioning unless inventory changes in
      lockstep (out of US-009 scope, TKT-4).
- **Closure idempotency — outbox table vs monotonic version + single marker.**

## Decision

We adopt, for US-009:

1. **Keep the `performances` table and `/performances/*` endpoints**; add a
   `kind` discriminator. "Slot" stays the conceptual name (ADR-005), not a
   required identifier. A nicer alias can be a view when a downstream vertical
   wants one.
2. **Typed columns, not jsonb, for re-entry** (`re_entry_mode`, `max_entries`,
   `requires_exit`) with CHECK constraints (`count_limited ⇔ max_entries`);
   likewise `kind`, `closure_status`, and a per-kind temporal CHECK
   (`performance ⇒ starts_at`; day kinds ⇒ operating window). `starts_at` is
   relaxed to nullable. Operating times are `HH:MM` text with a format CHECK;
   capacity authority stays in Inventory (no count on the slot); a nullable
   `capacity_group_id` is a forward-compat seam only.
3. **No event `Schema` bump. `kind` is added additively at Schema 2** to
   `performance.published`. Additive optional fields are backward compatible
   (ADR-009); a bump is reserved for a breaking change. Inventory does not fork
   on kind (ADR-005), so it needs no change.
   *Refined by [ADR-017](./ADR-017-domain-event-schema-evolution.md): this decision stands for
   `kind`, but "additive ⇒ compatible" is not general — a field that changes what a consumer **does**
   (TKT-53's `capacity_group_id` moves inventory's pool key) requires a bump even though it parses
   cleanly. The lockstep objection weighed above is **accepted, not overturned**: TKT-53 bumped by
   changing producer and consumer in one commit (ADR-017 §4a).*
4. **Closure uses a monotonic `closure_version` + a single
   `closure_emitted_version` outbox marker**, not a table. The
   `closed`/`reopened` envelope id is derived from the version (deterministic,
   so a retried transition de-duplicates, a new toggle does not). Emission is
   synchronous in the transition handler (emit-then-mark, like publish/archive),
   and the opposite toggle is refused while the current version's event is still
   owed (`ErrClosurePending`), so the single marker never drops a transition.
   New subjects are `platform.catalog.performance.closed` / `.reopened`
   (namespace-consistent with the existing `performance.*` subjects).

## Consequences

- **Positive:**
    - Zero cross-service churn and an unbroken M1 contract; the migration is
      purely additive with a guarded, all-or-nothing rollback (mirrors 0003).
    - Invalid catalog state (bad kind/re-entry/closure combinations) is
      unrepresentable — the DB is the backstop behind handler validation.
    - Inventory keeps provisioning unchanged; the claim path stays unforked.
    - Closure is at-least-once and loss-free without new outbox machinery.
- **Negative:**
    - `performances` is now a slight misnomer for festival/operating-day rows;
      the name lags the concept until a view or later rename is justified.
    - Operating times as `HH:MM` text (not a `time` type) defer real time
      arithmetic to whenever a query needs it (YAGNI; the format CHECK guards
      validity meanwhile).
    - The closure-pending guard makes a rare interleaving (toggling while an
      emission is still owed) a 409 the client must retry, rather than queueing.
