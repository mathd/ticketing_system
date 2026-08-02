# ADR-041: Where an orphan-seat rule is arbitrated

Date: 2026-08-02

## Status

Accepted (TKT-175, delivered by TKT-179 → TKT-182)

## Context

An **orphan** is a free seat left with no free neighbour in its row after a selection is
taken. Preventing buyers from creating them is standard practice in reserved seating and
is worth real revenue; making it configurable per map matters because the rule is wrong
for some venues (general-admission-style rows, boxes, accessible pairs).

Deciding whether a selection would strand a seat needs three things **at the same
moment**, and no service has all three:

| | Has | Lacks |
|---|---|---|
| **inventory** | the deciding transaction (ADR-010), live occupancy (`claim_seats`) | any geometry — seat identities are opaque `section/row/seat` strings; there is no row ordering and no seat list |
| **catalog** | the geometry, and the natural home for the rule | any part in deciding a claim |

Two further constraints bound the answer:

- **The claim transaction is the platform's most contended path.** ADR-010 exists to keep
  it short and lock-ordered. Whatever this costs, it cannot cost a network round trip
  while holding the pool lock.
- **A rule that does not run inside the deciding transaction is decoration.** Two
  claimants can each take a legal seat and *jointly* strand a third. Any check that
  happens before the transaction — in the browser, in commerce — passes both of them.

At the time of writing, inventory consumes exactly four subjects
(`platform.catalog.performance.{published,archived,closed,reopened}`) and holds no
geometry projection of any kind.

## Possible Solutions

- **Option 1 — inventory calls catalog on the claim path.** Simplest to describe: when a
  seat claim arrives on a rule-enabled pool, fetch the row's geometry and decide. It puts
  a synchronous cross-service call **inside the transaction holding the pool lock**, which
  is the specific thing ADR-010 was written to prevent: every claimant for that
  performance now queues behind catalog's latency and availability.

- **Option 2 — inventory subscribes to `seat_map.published` and projects geometry.** No
  hot-path call. But it is a new durable consumer, a new projection, replay and
  reconciliation, and ordering coordination against `performance.published` — plus a
  genuinely new failure state: a pool provisioned while its geometry projection is
  missing, stale, or arrived out of order.

- **Option 3 — commerce checks between price resolution and the claim.** Cheap and
  outside the hot path, and **cannot satisfy the contention requirement**: it is advisory
  by construction, so two concurrent claimants both pass it and strand the seat anyway.

- **Option 4 — project the exact published version's geometry at provisioning time
  (chosen).** A published seat-map version is **immutable** (ADR-029): an edit mints a new
  version and never mutates an existing one, and a seated pool is bound to one specific
  version. So the geometry that pool will ever need is fixed the moment it is provisioned.
  Fetch it **once**, at provisioning, from the public read for that exact version, convert
  each row's `position` order into left/right adjacency, and commit the projection
  atomically with the pool. At claim time the rule is a local, pool-scoped query inside
  the transaction that already holds the lock.

## Decision

**Option 4.** Geometry is projected once at provisioning for the exact immutable version;
the rule is arbitrated inside the deciding transaction against that local projection.

What makes this different from Option 2 is not the projection — it is **immutability**.
Option 2's costs are all consequences of a projection that can drift: replay,
reconciliation, staleness, ordering. Projecting something that cannot change has none of
them. The version binding is what buys that, and it is why this ADR depends on ADR-029
rather than merely citing it.

Concretely:

1. Catalog carries `orphan_prevention_enabled` **per seat-map version**. An edit that
   omits it inherits the predecessor's value; an explicit value applies only to the newly
   minted version.
2. A seated performance whose bound map has the rule on publishes at
   `performance.published` **schema 5**, carrying the exact `seat_map_id` and the flag.
   Rule-off maps keep emitting **schema 4, byte-identical**.
3. Handling schema 5, inventory fetches that exact version's geometry **before opening its
   transaction**, derives adjacency from each row's `position` order (null = row end), and
   commits pool, flag, projection and `consumed_events` atomically. Resolution failure
   fails closed: delayed NAK, no pool provisioned, nothing partial stored.
4. At claim time, and **only when the flag is on**, `CreateSeatHold` runs the rule after
   `sweepExpired` and after `contendedSeats` — under the pool lock, before any write.
   A refusal returns the newly stranded identities.

### Rules this ADR fixes

- **Adjacency is `position` order within a row.** Not label arithmetic, which breaks on
  non-numeric labels and on position gaps; not visual coordinates, which the model does
  not carry. Sections and rows never connect.
- **Only *newly* orphaned seats are refused.** A seat stranded earlier — by a refund, by
  an administrative action — must not poison unrelated claims for ever. The rule
  constrains what a selection *creates*, not what it inherits.
- **A row end has one neighbour; a one-seat row has none** and is always selectable.
- **Liveness is `claim_seats.released_at IS NULL`, evaluated after `sweepExpired`**
  (ADR-031). Running before the sweep would refuse legal selections against seats whose
  holds have already expired.
- **The refusal code is `orphaned_seats`, distinct from `seat_taken`.** The identities are
  **free seats the buyer never requested**, so commerce's `seat_taken` validation — which
  requires the identities to be a subset of the request (TKT-173) — would reject every
  valid orphan refusal as malformed. The two codes carry opposite relationships to the
  request and must not share a path.
- **The projection must be complete and reciprocal, and that is established in two
  places, because it is two different claims.** The bounded claim-time query reads adjacency
  rows to find candidates, so a missing row finds no orphans and lets a stranding claim
  commit, and a one-way edge can blame an unrelated claim for a seat that was already
  isolated. Both are *unsound*, not merely incomplete, so the claim path fails closed
  (`ErrSeatProjectionIncomplete`) on any defect it can reach: the requested seats' rows,
  their neighbours' rows, and whether those edges point back.
  **What it cannot reach is stated rather than implied.** An edge pointing *in* from a row
  the request never touches is invisible to any query bounded by the request, and finding
  it means scanning the pool — the cost the bounded form exists to avoid. That case is
  closed where the projection is built and where it is the only thing that changes:
  `ProvisionSeated` validates the whole set before it writes anything.
  **And `ProvisionSeated` proves internal consistency, not fidelity.** A projection whose
  seats all name no neighbours is perfectly reciprocal, and nothing in inventory can tell
  it apart from a map of genuine one-seat rows — the geometry that would settle it lives in
  catalog. Fidelity is established exactly once, where the adjacency is *derived* from that
  geometry (`SeatMapAdjacency`), and is pinned there by tests that assert the middle seat of
  a row names both its neighbours. Re-checking it in the store would mean re-deriving it
  from data the store does not have. Per ADR-021, name the adversary: all of this is
  corruption-detection for an honest writer, and a writer with inventory DB access can
  still make the rule say whatever they like.
- **Rule off means no work, and it is proven by absence:** dropping the projection table
  and observing a rule-off claim still succeed. "No extra latency" is read as *no
  additional network call, SQL statement, lock or projection access*; literal timing
  equality is unmeasurable and, read that way, unachievable.

### Delivery order is part of the decision

ADR-017 §5b′ requires a consumer to dispatch on `schema` before decoding `data`. A
producer emitting schema 5 before its consumers accept it makes them park the message and
readiness fall. The safe order is therefore **consumers first, producer second**, and that
is only expressible as separate merges:

**TKT-179** setting + this ADR → **TKT-180** access accepts schema 5 → **TKT-181** inventory
implements schema 5 and its projection → **TKT-182** the rule itself → **TKT-183** catalog
emits schema 5, and re-emits for performances already published against an enabled map.

**Amended twice during TKT-180 (2026-08-02), both times by its ai-review.** The original
ordering said "consumers first" and meant *both* consumers, and the first amendment gave the
wrong reason for the distinction. Both corrections are recorded because the wrong versions
are more intuitive than the right one.

**Inventory cannot accept schema 5 early; access can — and receipt-recording is not why.**

Inventory's schema-5 handler provisions a pool and records the event in `consumed_events`. A
binary that accepts schema 5 *without* building the adjacency projection creates a
rule-enabled pool that has no rule, marks the event consumed, and acks it. A later, capable
binary redelivered that event short-circuits on the consumed row: the pool is permanently
rule-less and nothing reports it.

Access records `consumed_events` too (`store/policy.go`). It is nonetheless safe, because
schema 5 changes nothing access owns: it projects the unchanged identifiers and `re_entry`,
so **no future access binary will ever need to redo that event**.

So the discriminating property is **semantic completeness, not idempotency and not whether a
receipt is written**:

> A consumer may accept a new schema variant early only if its current handler performs
> **every effect this consumer will ever require from that variant** — or if the
> later-required effects remain replayable despite any completion marker.

Stated as "safe when handling is idempotent", the rule would both condemn access, which is
correct today, and authorise irreversible partial handling merely for being repeatable. That
is the wrong test, and it was the first amendment's.

**The producer gets its own revision, because a shared merge does not order a deployment.**
Catalog and inventory start independently, so a single merge carrying both inventory's arm
and catalog's emission does not guarantee inventory is running first. If catalog emits while
an old inventory replica is still up, that replica **quarantines the event, acks it, and
latches unready** — and deploying the capable binary does **not** repair it: quarantined
originals were acked, so recovery is `reprocess-quarantine` **plus a restart**, never
automatic. Newly published performances would have no inventory until an operator acts.

Hence TKT-183: catalog's emission is a separate revision, deployed only once TKT-181's
inventory *and* TKT-182's rule are fully rolled out.

**The enforcer ships before the producer, not after.** An earlier draft of this section
ordered TKT-183 before TKT-182, which opens a window where organizers have enabled the rule,
inventory has recorded the flag and the projection, and buyers can still strand seats —
silently, for the length of a deployment, on a revenue rule someone deliberately turned on.
TKT-182 has no reason to wait: it is dormant until a schema-5 pool exists, so shipping it
first costs nothing and closes the window by construction.

**Performances already published against an enabled map must be re-emitted.** This is the
non-obvious consequence of shipping the setting (TKT-179) before the transport (TKT-183): a
performance can be published against a rule-enabled map *today*, and catalog emits it at
schema 4, sets `event_emitted_at`, and never emits it again — re-POSTing publish is
idempotent and will not re-fire. Inventory provisions an ordinary seated pool with no flag
and no adjacency, and **nothing later in this sequence repairs it**. Those pools would be
permanently rule-less while the back office insists the rule is on.

So TKT-183 owns a correction wave as well as the forward path, and it needs a **fresh event
identity** rather than the existing re-emit machinery: the `reemit-policies` path uses fixed
correction ids that may already sit in `consumed_events`, so replaying under them is a no-op.
Inventory's schema-5 handler must therefore also **upgrade an existing seated pool** —
attaching the flag and projection to a pool that already exists — rather than assuming it
only ever provisions new ones. Prove it is replay-safe.

**And the wave must not race catalog's own rollout.** "Run it after TKT-181 and TKT-182 are
deployed" is not sufficient: during TKT-183's *own* rolling deployment an old catalog replica
can still publish a rule-enabled performance at schema 4 **after** the scan has already passed
it. That publication sets `event_emitted_at`, so it can never be re-emitted, and the pool is
permanently rule-less — the same deployment-order hazard this section identifies for
inventory, occurring one layer in, inside the producer's own rollout.

So the wave starts only once **every schema-4-producing catalog replica has drained**, or it
is designed to converge without that assumption: a watermark, or a reconciliation repeated
until it finds nothing, rather than a single pass. Its correction identity must be
**deterministic per performance** so repeats converge instead of multiplying events.

## Consequences

- **Positive.** No cross-service call on the claim path. No stale projection, by
  construction. The rule is decided where contention is resolved, so two claimants cannot
  jointly strand a seat. Rule-off pools are untouched, including their event bytes.
- **Negative.** Immutable geometry is duplicated per pool — deliberately: it makes
  provisioning completeness, version binding and claim scoping local and atomic, at the
  cost of storing the same rows once per seated performance. A `performance.published`
  schema fork must be rolled out in a fixed order across three services.
- **Bounded.** This ADR decides *arbitration*, not selection. Best-available seating
  (TKT-81) will need adjacency too and can read the same projection, but its relaxation
  rules are its own decision.
- **Not covered.** Rules other than single-seat orphans — price-tier adjacency,
  accessible-seat pairing, party splitting across rows. Each would need its own data and
  most would need more than left/right neighbours.
- **A caveat worth naming (ADR-021's discipline).** This is an honest-writer guarantee.
  The projection lives in inventory's database; anyone who can write there can make the
  rule permit or refuse anything. It constrains buyers, not an adversary with database
  access.
