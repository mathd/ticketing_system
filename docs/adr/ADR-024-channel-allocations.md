# ADR-024: Channel Allocations as Derived-Usage Rows Inside the Pool Lock

Date: 2026-07-16

## Status

Accepted

**Sales windows, deferred here, are now decided by [ADR-054](./ADR-054-per-channel-sales-windows.md) (2026-08-10, TKT-238).** This ADR's accounting rules are extended rather than changed: a window gates claims and never releases capacity, so `reservedForChannelsSQL` is untouched, and the same clock_timestamp() discipline applies for the same reason.

**The allocation set carries a REVISION (2026-08-15, TKT-250).** Amended below in §Decision (8), §Consequences and §The adversary. This ADR's original §Consequences accepted that the full-set PUT had no stale-write protection, "acceptable while allocation editing is single-operator" — and TKT-244 falsified that premise by shipping the first UI for the endpoint. A replace may now present the revision it is replacing; it is compared under the same pool row lock as everything else here and refused with a coded 409 when stale. The accounting is untouched: a revision says *whether this save may proceed*, never *how much* is left. **It is lost-update protection between honest operators, not authorization and not tamper-evidence.**

**An allocation may bind to a SELLER (2026-08-12, TKT-246).** Amended below in §Decision (7) and §Consequences. An allocation gains a nullable `sold_by`: unset means public — every allocation predating this — and set means only that reseller may consume it. Judged in the claim paths under the same pool row lock as everything else here, in the order **window → seller → code → capacity**. The accounting is untouched: a binding says *who* may consume a cap, never *how much* is left, so `reservedForChannelsSQL` and every derived count are unchanged.

## Context

US-015 (TKT-78) splits a slot's sellable capacity across sales channels — opaque codes with
per-channel caps and an optional scheduled give-back (`release_at`) — so presales and resellers
cannot starve the public on-sale. The constraints come from ADR-010: the inventory pool row is
the single serialization point, PostgreSQL time decides lazy transitions, and correctness must
not depend on a background job. Channel registry, per-channel pricing and sales windows are
explicitly out of scope (TKT-17/TKT-5); channel codes here are exact opaque strings — no
normalization, no case folding.

## Possible Solutions

- **Materialized sub-pools** — each channel a child pool with its own capacity row.
    - Pros: reuses the pool accounting shape.
    - Cons: every sale coordinates parent and child capacity, introducing a second lock order
      (against ADR-010); give-back at `release_at` becomes a capacity *transfer* needing a
      sweeper or a mutation, not a predicate.
- **Allocation rows with consumed counters** — cap plus a counter maintained on every hold.
    - Pros: cheap reads.
    - Cons: counter/claim drift is a standing risk; expiry (a lazy state, not an event) cannot
      decrement a counter without a sweeper — the exact failure ADR-010 bans.
- **Allocation rows with derived usage** (chosen) — `channel_allocations(pool_id, channel_code,
  cap, release_at)`; claims carry a nullable `channel_code`; consumption is always a sum over
  claims under the pool lock.
    - Pros: single lock order preserved; zero drift by construction; release is a pure DB-time
      predicate (`release_at IS NULL OR release_at > clock_timestamp()`), symmetric with hold
      expiry. `clock_timestamp()`, not `now()`: `now()` freezes at transaction start, so a hold
      transaction queued on the pool lock across the cutoff would decide with stale time and
      sell a released channel. The global capacity check never depends on this predicate, so
      the two time bases cannot combine into an oversell.
    - Cons: adds indexed claim aggregation to the serialized write path (measured under
      US-019/TKT-82 if it ever matters).

## Decision

We adopt allocation rows with derived usage. Specifics:

- **Accounting (all under the pool `FOR UPDATE`, all int64):** channel consumption =
  confirmed + live claims (the shared `liveClaims` predicate — never a re-derived expiry
  expression) carrying that `channel_code`. A channel hold needs an *active* allocation with
  cap headroom **and** pool headroom. A public hold (claims.channel_code NULL — the implicit
  default channel, no allocation row) additionally may not eat capacity still reserved for
  active allocations: `Σ GREATEST(cap − consumed, 0)` over active allocations.
- **Scheduled give-back is lazy:** past `release_at` an allocation stops matching the active
  predicate — new holds on that channel reject, its unsold remainder is publicly claimable,
  and existing holds finish their lifecycle untouched. No sweeper, no released flag.
- **Administration:** `PUT /internal/slots/{id}/channel-allocations` atomically replaces the
  full set under the pool lock (no transient over-commitment while moving cap between
  channels). Caps must sum ≤ pool capacity and each cap must cover its channel's current
  consumption. State-idempotent PUT — no idempotency-key registry. Omitting a channel closes
  it; historical claims keep their attribution (no FK from claims to allocations).
- **Idempotency (ADR-009):** `channel` joins the buyer-hold request fingerprint. The
  channel-less fingerprint stays byte-identical to the pre-channel format, so idempotency
  records created before this migration keep replaying; a replay that changes or drops the
  channel is a key reuse.
- **Public availability is channel-scoped (semantic change):** `GET /slots/{id}/availability`
  keeps pool aggregates for `capacity`/`held`/`confirmed`, but `available` now means "claimable
  on the requested channel" — the optional `channel` query parameter scopes it; omitted means
  public/default. The endpoint stays in ADR-004's 5-second remaining-capacity tier, so release
  visibility may lag DB time by up to 5s on reads; write correctness switches exactly at
  PostgreSQL time. Staff availability adds `public_available` and a per-channel breakdown.
- **Operational holds (ADR-023) stay unchanneled:** they consume pool capacity only (and can
  therefore exhaust the pool while a channel has nominal headroom — accepted); conversion
  produces a public buyer hold. Allocation configuration changes are sellability config, not
  claim lifecycle mutations — they do not enter `claim_history`.
- **An allocation may bind to a seller (TKT-246).** `channel_allocations.sold_by`, a nullable
  uuid. NULL = unbound = anyone may consume it, which is every allocation predating the
  column, so the default is exactly the prior behaviour. Set = only that reseller.
    - **A uuid, not a boolean.** "This allocation is spoken for" would let reseller B consume
      reseller A's stock — the same class of bug this decision exists to close, one layer in.
    - **No foreign key**, for the reason ADR-056 gives for the same column on commerce's
      orders: the reseller registry lives in *commerce*, so an FK is impossible across the
      database boundary and would be wrong anyway — revoking or rotating a credential must
      not rewrite who a past allocation was bound to.
    - **Refusal order: window → seller → code → capacity**, under the pool `FOR UPDATE`, in
      both channelled claim paths (`CreateHold` and `PlaceGroupReservation`; draw-down stays
      exempt as quantity-neutral). Authorization precedes capacity for ADR-054's reason — a
      channel-property refusal masked by a full pool makes a gated channel read as a sellout
      exactly when the on-sale is busiest. It follows the window because a closed channel is
      selling to nobody, and it precedes the code because `redeemPresaleCode` *mutates*: a
      refusal after it would burn a scarce redemption on a caller who was never eligible.
    - **The refusal is the ordinary capacity refusal, deliberately not a distinct code.** A
      "seller mismatch" answer would tell an unauthenticated prober that a channel exists and
      is bound to someone else — the enumeration oracle `presale_code_invalid` was made
      uniform to prevent (TKT-239). `sold_by` must not become discoverable through a refusal.
    - **A full-set PUT that omits `sold_by` unbinds**, consistent with every other field in
      this replace-set contract. An editor that round-trips the set must carry it, or it
      performs an authorization change by omission (TKT-236's shape). Pinned by a test.
- **Idempotency is scoped to the seller, structurally (TKT-246).** `claims` was
  `UNIQUE (organizer_id, idempotency_key)`. Two reseller credentials may share an organizer and
  keys are caller-chosen, so two partners sending `"1"` landed on one row — and because
  `CreateHold` returns a fingerprint-matching row as a **replay before it reads `sold_by`**, the
  second partner was handed the first's authorized hold with the seller guard never running. The
  guard was not beaten, it was *skipped*.
  Migration `0016` adds a nullable `claims.reseller_scope` and splits the constraint into two
  partial unique indexes — `(organizer_id, idempotency_key) WHERE reseller_scope IS NULL` and
  `(organizer_id, reseller_scope, idempotency_key) WHERE NOT NULL`. Existing rows all have NULL and
  keep the identical constraint.
  **A key prefix is not a namespace.** The first attempt derived `r:<uuid>:<key>` in Go; public
  keys remained arbitrary strings in the same column, so a public caller could send that exact
  string, take the row first, and permanently deny that reseller that key. The namespace has to be
  a field the caller does not supply. The stored key stays the caller's, verbatim, on both paths.
- **Where a reseller identity may ENTER inventory (TKT-246).** Only through
  `POST /internal/holds`. The public `POST /holds` does not accept `reseller_id` — the field is
  absent from its schema, so `additionalProperties: false` refuses it at the validator.
  **Inventory has no authenticated notion of a caller**, so any identity it accepts is exactly as
  trustworthy as the least-guarded route that supplies it. `POST /holds` is not under `/internal/`,
  the gateway proxies it, and the first version of this change accepted `reseller_id` there — an
  authorization input from an unauthenticated body, which is the defect TKT-240 was reverted for,
  recreated one service down. Two independent guards now: the gateway 404s every `/internal/` path
  (ADR-002) and the handler requires the service credential. Neither is trusted alone.
  Inventory authenticates the **caller**, not the reseller, and trusts that caller to have
  authenticated the reseller (commerce, from the partner credential — ADR-056).
- **Who may name a channel (TKT-246).** A channel only reaches this decision from an
  *authenticated* caller. Commerce's public `POST /reservations` is unauthenticated and takes
  `channel_code` from the request body, so it forwards **no channel to inventory at all**;
  only the partner route (ADR-056), whose channel comes from a credential, does. TKT-240
  forwarded the body value and was reverted: with the forward in place any caller could name a
  reseller's channel and consume its allocation with no credential — executed, not argued.
  Binding does not rescue the public forward either, because an unbound allocation admits
  anyone and every allocation in existence is unbound.
  **Residual, stated plainly:** a public sale naming a reseller channel still *prices* under
  that channel's fee rules while consuming public stock. That is a fee-attribution defect, not
  an inventory one; moving inventory now requires a credential. Tracked separately.
- **(8) The allocation set carries a REVISION (TKT-250).** `inventory_pools.allocation_revision`,
  a `bigint` starting at 0. A replace may present the revision it believes it is replacing; the
  value is read by the **same locked `SELECT`** that already reads capacity, compared **before
  any other validation**, and refused with `ErrAllocationRevisionMismatch` (coded 409
  `allocation_revision_mismatch`) when it differs. A successful replace increments it once.
    - **Why the pool row and not the allocation rows.** The write DELETEs and re-INSERTs every
      `channel_allocations` row, so nothing per-row survives a save — any version column there
      would reset to its default on every write. The pool row is the only thing that persists
      across the replace, and it is already the row being locked.
    - **Why a counter and not `updated_at` or a hash.** `inventory_pools.updated_at` moves on
      every confirm, refund, capacity adjustment and offering-state change, so a revision riding
      on it would be invalidated by ordinary trading and the editor would refuse saves during an
      on-sale. A hash needs a canonical encoding of opaque channel codes — which §Decision
      forbids normalizing — and cannot distinguish A → B → A from no change at all.
    - **Only the replace moves it.** `ReplaceChannelAllocations` is the sole writer of
      `channel_allocations`, so a revision bumped only there cannot miss a change to the set;
      and a form opened before a busy on-sale still saves afterwards.
    - **Required for the staff credential, optional for `X-Internal-Token`.** The split is
      expressible only because ADR-057 gave inventory its own staff credential, so the guard can
      tell the two apart and publish the class to the handler. The back office is where two
      humans race and is the only caller that renders a form from a read; the service-to-service
      path rebuilds its whole set per call, so a precondition would buy it nothing. The contract
      cannot express "required for one credential class", so `allocation_revision` is optional in
      both schemas and the rule lives in the handler — which is why it is asserted, not assumed.
      *(The compatibility argument originally filed for this ticket — "every service-to-service
      caller uses this route" — was checked and is false: there is no service-to-service caller.
      The optional arm exists for the smoke suite and for future internal callers, not for an
      existing one.)*

## Consequences

- **Positive:**
    - No-oversell extends to per-channel caps with the same single-lock proof shape as
      ADR-010; the contention smoke asserts exact grant counts.
    - Nothing can drift: every number is derived from claims; expiry and give-back are
      predicates, not jobs.
- **Negative:**
    - The serialized write path gains per-channel aggregation queries (indexed by
      `claims(pool_id, channel_code, status, expires_at)`); revisit only with US-019 data.
    - ~~Full-set PUT has no stale-write protection (`If-Match`); acceptable while allocation
      editing is single-operator.~~ **Superseded (2026-08-15, TKT-250) — the premise was
      falsified by TKT-244, which shipped the first UI for this endpoint.** Allocation
      editing stopped being a deliberate one-person `curl` and became a screen any admin
      can open, so two operators on one slot is ordinary rather than hypothetical. The set
      now carries a revision; see §Decision (8) below.
    - **A channel with no active allocation is refused outright once its channel reaches
      inventory (TKT-246).** For the partner route this means a reseller configured for *fee
      rules* but not for *inventory* cannot sell at all. Every channel a partner credential
      names needs an allocation before its credential is issued. The blast radius is one
      reseller rather than the platform, precisely because the public route forwards nothing.

### The adversary this binds, and the one it does not (ADR-021)

`sold_by` is **honest-writer authorization, not tamper-evidence.** It constrains a caller
arriving through the hold path, and nobody else:

- **Bound:** an external partner, and any commerce caller, reaching inventory through
  `POST /holds`. It cannot consume an allocation bound to a different reseller.
- **Not bound:** anyone who can write inventory's database, or call `/internal/` directly, can
  set, clear or impersonate `sold_by` at will. Inventory does not authenticate the reseller —
  it *trusts its internal caller*, and commerce is what verifies the credential (ADR-056).

So this decision stops a reseller from selling another reseller's stock. It is not evidence of
who sold anything, and a settlement process (TKT-23) must not treat it as proof.

**`allocation_revision` (TKT-250) binds a narrower adversary still — none at all, in the
security sense.** It is **lost-update protection between two honest operators**, and nothing
more:

- **Bound:** two back-office admins editing one slot's allocations from pages rendered at
  different moments. The second save is refused rather than silently overwriting the first.
- **Not bound:** anyone at all, in the adversarial sense. A caller who can write inventory's
  database can set the revision to whatever they like; a caller holding `X-Internal-Token` may
  omit it entirely and replace unconditionally, by design. State inside the database cannot
  constrain an adversary who writes to the database (ADR-021).

The original framing of TKT-250 called this an authorization fix — a stale save was said to
overwrite `sold_by` and return a reseller's bound stock to the public pool. **Shaping checked
that against the code and it does not hold**: the back office re-reads the current set on the
POST itself and sources `sold_by`, `requires_code` and the window from that read (TKT-244's
ai-review passes 2–4), so those fields are not carried by the stale form at all. Do not restate
the authorization claim; it was checked and it is false.

## References

- TKT-78 (US-015), parent epic TKT-4; PRD `docs/product/prd-v1.md` §TKT-4
- [ADR-010](ADR-010-postgres-claim-transaction.md) — lock order, lazy DB-time transitions
- [ADR-009](ADR-009-contract-first-apis.md) — fingerprint/replay exactness
- [ADR-023](ADR-023-operational-holds-and-conversion.md) — operational holds interplay
- [ADR-004](ADR-004-cache-first-read-path.md) — availability cache tier
