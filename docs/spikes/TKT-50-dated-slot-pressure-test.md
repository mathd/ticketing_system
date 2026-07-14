# Spike report — TKT-50 (US-008): Pressure-test the generalized dated-slot model

**Date:** 2026-07-14 · **Type:** timeboxed analysis spike (no production code) · **Decides:** amends ADR-005, adds ADR-013
**Outcome:** ✅ **HOLD (with a documented attribute shape) + one scoped exception.** The unified dated-slot / single-claim model survives re-entry, mid-day capacity change, and weather closure **for the reservation path**. Live concurrent-occupancy capping (fire-code in/out counting) is a genuinely different accounting primitive and is carved out to **ADR-013**, out of US-009 scope.

> This is a decision doc, not code (spike quality gate: no `make check` surface). The exit is the recorded verdict below plus the concrete attribute set + state machine US-009 (TKT-51) implements.

## Why this spike exists

ADR-005 made every admission-granting offer a **dated slot in one inventory machinery**; passes are entitlements to claim slot entries; slot attributes carry domain specifics and *the claim primitive does not fork*. ADR-005 §Consequences flags the abstraction as load-bearing and names three pressure tests before it hardens: **re-entry, mid-day capacity changes, weather closures**. US-009 hardens `performances` into a typed slot; a bad generic shape there hurts every downstream vertical (passes TKT-12, lodging TKT-13, festivals TKT-14). This spike de-risks that shape first.

## The M1 baseline this is grounded in (verified, not assumed)

| Concern | Where it lives today | Relevant invariant |
|---|---|---|
| The dated slot | `catalog.performances` (`starts_at`, `timezone`, `status draft\|published\|archived`), no concert-specific fields | Publish/archive is a single explicit state machine (US-007); publication emits a versioned domain event |
| Capacity + no-oversell | `inventory.inventory_pools(slot_id PK, capacity, confirmed_quantity, source_event_id)` + `claims(held\|confirmed\|released\|expired)` | ADR-010: DB is the sole write-side correctness boundary; `confirmed + held ≤ capacity`; **capacity adjustments after publication require a new explicit event and cannot silently overwrite a pool that already has claims** |
| Admission / redemption | `access.tickets` + append-only immutable `lifecycle_events(event_type IN issued\|delivered\|redeemed)` with `UNIQUE(ticket_id, event_type)` | A performance ticket redeems **exactly once** — single-redemption is a DB constraint, not app logic |

The load-bearing observation: the M1 stack already splits the concern three ways — **Catalog** describes the slot, **Inventory** reserves a finite right to a slot, **Access** records the physical act of entering. The pressure tests are really asking *which of the three owns each new attribute*, and whether any case forces the single claim primitive to fork.

---

## Case 1 — Re-entry (park pass holder exits and re-enters the same operating day)

**The tension.** A performance ticket is single-redemption by DB constraint (`UNIQUE(ticket_id,'redeemed')`). A park pass grants *multiple* entries on one operating-day slot. Naively, multi-entry breaks that constraint.

**Resolution — it does not fork the claim primitive; it is an Access-layer attribute + event-shape change.**

- **Catalog slot attribute:** `re_entry_policy { mode: single | multi | count_limited, max_entries?: int, requires_exit?: bool }`.
  - `single` — the performance case; `requires_exit` irrelevant; `max_entries` = 1 implicitly.
  - `multi` — unlimited entries; `requires_exit=true` means an `exit` must be recorded before the next `entry` counts (turnstile parks), `false` means re-scan is idempotent within the day.
  - `count_limited` — `max_entries = N` per operating day, reset scope = the operating day (see Case-4 operating-date semantics).
- **Access event shape:** the single `redeemed` terminal event generalizes to an append-only **entry/exit stream**. The current table-wide `UNIQUE(ticket_id, event_type)` cannot host it — several `entry` rows for one ticket would violate it. So US-009's downstream (not this spike) **replaces that constraint with a partial unique index scoped to redemption**: `CREATE UNIQUE INDEX ... ON lifecycle_events(ticket_id) WHERE event_type = 'redeemed'`. `issued`/`delivered` keep their existing per-type uniqueness; `entry`/`exit` get **no** uniqueness (repeated rows are the point). The **precise redefined Access invariant**: a *single-entry* slot's ticket has **exactly one** `redeemed` event and no `entry`/`exit`; a *multi-entry* slot's ticket has **zero** `redeemed` events and any number of `entry`/`exit` events. The single-redemption guarantee is not weakened — it is enforced, unchanged in strength, by the partial index for exactly the tickets it applies to. What `redeemed` *means* is pinned per mode: `single` → first (and only) scan; `multi`/`count_limited` → recorded as `entry`, with `redeemed` reserved for `single`.
- **Entitlement vs claim (the part that must not be fudged).** A pass is an **entitlement** (a right to enter), not a per-visit claim. Each re-entry does **not** create a new inventory claim and does **not** re-consume day capacity — otherwise an unlimited pass would exhaust the operating-day pool on the second scan. The inventory claim is taken **once** when the entitlement is admitted against the day slot (or provisioned at pass-sale time as an entitlement claim, TKT-12's problem), and subsequent physical entries are Access events that consume **no** capacity. This is exactly ADR-005's "passes are entitlements to claim slot entries" made precise: entitlement → validates the right; claim → consumes slot capacity once; entry/exit → physical presence, uncounted against the reservation pool.

**Boundary:** Catalog owns `re_entry_policy` (describes the slot). Inventory owns the **one** capacity claim per admission-right (unchanged primitive). Access owns the entry/exit event stream and enforces the policy at the gate. No fork.

---

## Case 2 — Mid-day capacity change (raise or cut while entries are live)

**Where the number-of-authority lives:** the **inventory pool** (`inventory_pools.capacity`), already decided by ADR-010. Catalog carries **no** count. Catalog emits the *initial resolved* capacity in the publication event (today derived from `venues.ga_capacity`); Inventory owns the mutable pool capacity and the post-publication adjustment policy. A change is a **new explicit capacity event** (versioned, idempotent — see below), never a silent overwrite (ADR-010).

**Raise:** trivial — a capacity event increases `inventory_pools.capacity`; `confirmed + held ≤ capacity` still holds; new claims become available. No correctness issue.

**Cut — the hard edge.** A cut *below* current demand cannot be represented by simply lowering `capacity`, because ADR-010's pool invariant is `confirmed + held ≤ capacity` — the floor is `confirmed_quantity + held_quantity`, not `confirmed_quantity` alone (an in-flight hold is a claim the pool has already promised headroom to). Three candidate representations were considered:

1. **Reject-below-demand** — the pool refuses a capacity event that would drop `capacity < confirmed + held`; the operator can only cut down to current sold-plus-held. Simplest, preserves the invariant by construction, but can't express "stop selling, we're over" after the fact.
2. **Separate requested vs effective capacity** — store the operator's requested cap independent of the enforced floor (`effective = max(requested, confirmed + held)`). Expressive but adds a second number to reason about and to trace.
3. **Explicit `sales_halted` pool state** — capacity stays ≥ confirmed (invariant intact); a separate flag stops *new* claims regardless of headroom.

**Decision for US-009's schema.** The *rule* is decided here; only its *storage representation* is deferred. The rule: a cut is applied as `capacity := max(new, confirmed + held)` — the invariant floor — **plus** `sales_halted` semantics (block new claims) when the operator's intent is "below current confirmed+held demand". A confirmed admission is a promise already made; capacity policy is **forward-only** — never force-release a confirmed claim. Held-but-unconfirmed claims are not force-expired either; they drain via normal expiry (ADR-010), after which a re-issued cut can go lower. This needs **no catalog attribute** — US-009 adds nothing here beyond ensuring the publication/capacity event envelope can carry a capacity delta. What is left to the inventory-side capacity-adjustment ticket is purely the *representation* of that rule (an `effective`-capacity column vs a separate `sales_halted` flag vs both) — not the behaviour, which is fixed above; US-009 must only avoid putting a mutable count in Catalog.

**Boundary:** Inventory owns the count, the invariant, and the cut policy. Catalog owns nothing here. Access is unaffected (occupancy ≠ capacity — see the scoped exception).

---

## Case 3 — Weather closure (operating day closes partway through)

**Slot state — an attribute, not a new lifecycle state.** Closure must **not** be a fourth value alongside `draft|published|archived`: it is orthogonal. A closed day is still `published` (it existed, it sold, its history is preserved) — it is not `archived` (archive = "never happened publicly / removed from storefront"). Modelling closure as a lifecycle value would collide with archive's public-listing and idempotency semantics (US-007).

Closure is `published + closure { status: open | closed, closed_at, reason }`, orthogonal to the publish/archive lifecycle:

- **State machine:** `open —(close)→ closed —(reopen?)→ open`. Reopen **is** allowed (a weather hold that lifts) and is why closure is a toggle attribute, not a terminal transition.
- **Cross-rule with the lifecycle (archive).** Closure is orthogonal, so archive stays legal from `published` **regardless of `closure.status`** — archiving a `closed` day is allowed (it removes a day that ended early from the storefront) and simply carries the `closed` attribute into history; it does not first require reopening, and closure never becomes a fourth lifecycle value. The one ordering constraint: `closure` transitions are only meaningful while `published` (you cannot close a `draft` or an `archived` slot).
- **Emits a domain event** (`slot.closed` / `slot.reopened`), versioned + idempotent.

**Effect on holders, by Access state (refund-relevant states enumerated — refund *policy* is downstream, this spike only names the signal):**

| Holder state at closure | Effect | Refund signal |
|---|---|---|
| Admitted, still inside (`entry`, no `exit`) | Service partially delivered | Policy: typically none / partial — downstream |
| Admitted, already exited (`entry`+`exit`) | Service delivered | None |
| No-show, not yet arrived (claim confirmed, no `entry`) | Entry now denied | **Refund-eligible** — the signal this case must produce |
| Re-entry attempt after closure (had `entry`+`exit`) | Denied (day is closed) | Policy — downstream |
| Delayed entry after a reopen | Allowed (day is `open` again) | None |

- **Inventory:** on `closed`, stops offering the slot for **new** claims (archive-like, but mid-live and reversible) — a held-but-unconfirmed claim expires normally; confirmed claims are untouched (forward-only, as Case 2).
- **Access:** denies `entry` while `status=closed`; existing `entry`/`exit` events are immutable history.
- **Refund signalling:** keyed off Access redemption state — a confirmed claim with **no** `entry` event at closure is the refund-eligible signal Payments/Commerce consume. This spike names the signal and the states; the refund *policy/mechanics* are explicitly a downstream ticket, not US-009.

**Boundary:** Catalog owns the `closure` attribute + its state machine + the domain event. Inventory reacts (stop offering). Access reacts (deny entry) and is the source of truth for "who had actually entered." Payments consumes the refund signal.

---

## Cross-cutting angles the three cases surface for the US-009 schema

These do not need code now, but US-009's schema must not preclude them:

- **Operating-hours + timezone (real schema surface).** `performances.starts_at` (an instant) is insufficient for an operating *day*. A slot needs a **local operating date** + `timezone` + `opens_at`/`closes_at`, because: re-entry `count_limited` reset scope, capacity-day identity, and closure windows are all keyed to the *local* day, not a UTC instant. US-009 must handle **midnight-spanning** days (a park open 10:00–02:00) and **DST** gaps/folds — the operating day is a local-date concept, and the pool/redemption identity is the (slot, local-date) pair, not a timestamp. This is the single most schema-affecting finding.
- **Festival shared-capacity forward-compat.** US-009 introduces `kind = festival_day`; US-011/TKT-14 later has festival days draw on **shared** festival capacity rather than per-day independent pools. US-009's slot schema must **not preclude** a slot pointing at a shared-capacity group (e.g. a nullable `capacity_group_id` seam), even though the shared-capacity **claim mechanics** stay out of scope (owned by TKT-14, deferred in the PRD). Non-goal here; just don't dead-end it.
- **Domain-event idempotency.** Closure and capacity-change events are operationally sensitive and event-driven; they must carry deterministic event IDs + a monotonic version/sequence and be replay/duplicate-safe — the same discipline the M1 consumers already use (`consumed_events` dedupe, poor-man's outbox). US-009's transitions emit these; tests cover idempotent re-emission.

## Concrete output for US-009 (TKT-51) — the decided shape

**Slot attribute set (Catalog):**

- `kind` ∈ `performance | festival_day | operating_day` — existing performances migrate to `performance` with **no behavioural change** (M1 US-003/004/005/006 tests stay green).
- **Operating window:** local operating date + `timezone` + `opens_at` / `closes_at` (nullable for `performance`, which keeps `starts_at`); midnight-spanning + DST handled as local-date semantics.
- `re_entry_policy { mode, max_entries?, requires_exit? }` — `single` for `performance`.
- `closure { status: open|closed, closed_at?, reason? }` — orthogonal to `draft|published|archived`.
- `capacity_group_id?` — nullable seam for future shared festival capacity; unused until TKT-14.
- **No mutable count on the slot** — capacity authority stays in Inventory.

**State machines:**

- Lifecycle (unchanged, US-007): `draft → published → archived`.
- Closure (new, orthogonal): `open ⇄ closed` while `published`.
- Capacity (Inventory, ADR-010): forward-only; raise freely; cut = `max(new, confirmed)` + block-new, never force-release.
- Redemption (Access): `single` → one `redeemed`; `multi`/`count_limited` → append-only `entry`/`exit`; single-redemption DB guarantee retained for the `redeemed` type.

**Claim path:** unchanged and unforked — one confirmed claim per admission-right; passes are entitlements that claim once; physical re-entries consume no capacity.

## Verdict

✅ **The unified dated-slot / single-claim model HOLDS** with the attribute shape above. All three pressure cases resolve to *attributes on the slot* + *reactions in Inventory/Access*, with the claim primitive unforked — exactly ADR-005's bet.

⚠️ **One scoped exception → ADR-013.** Live **concurrent-occupancy capping** (a fire-code "no more than N people physically inside right now" limit, decremented on exit) is **not** the same primitive as monotonic no-oversell reservation. Reservation counts *admissions sold* (a confirmed claim leaves only by explicit release/refund, never by time); occupancy counts *bodies present* (rises on entry, falls on exit). Forcing occupancy into the claim core would fork it. It is carved out: **not required for US-009**, which stores only slot identity/kind/hours/re-entry/closure; live occupancy, if ever built, attaches by `slot_id` in Access or a future capacity-control component — **never** as a live count in Catalog or in Inventory claims. See ADR-013.

Either outcome (hold + exception) leaves US-009 with a decided schema — the spike's exit condition.

## References

- [ADR-005 — Unified dated-slot admission](../adr/ADR-005-unified-dated-slot-admission.md) (amended by this spike)
- [ADR-013 — Concurrent-occupancy capping is out of the reservation claim core](../adr/ADR-013-occupancy-capping-scoped-exception.md) (added by this spike)
- [ADR-010 — PostgreSQL claim transaction and hold lifecycle](../adr/ADR-010-postgres-claim-transaction.md) · [ADR-003 — Append-only audit trail](../adr/ADR-003-append-only-audit-trail.md)
- [PRD v1 — TKT-2 catalog user stories US-007…US-011](../product/prd-v1.md)
- Baseline code: `services/catalog/internal/store/migrations/0001_catalog_schema.sql` · `services/inventory/internal/store/migrations/0001_inventory.sql` · `services/access/internal/store/migrations/0002_redeemed_lifecycle.sql`
