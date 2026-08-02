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
implements schema 5 *and* catalog emits it → **TKT-182** the rule itself.

**Amended during TKT-180 (2026-08-02), by its ai-review.** The original ordering said
"consumers first" and meant *both* consumers. That is right for **access**, which reads only
`re_entry` and genuinely ignores everything else, and wrong for **inventory** — and the
difference is worth stating because it is not obvious.

Inventory's schema-5 handler provisions a pool and records the event in `consumed_events`.
A binary that accepts schema 5 *without* building the adjacency projection therefore creates
a rule-enabled pool that has no rule, marks the event consumed, and acks it. A later, capable
binary cannot repair that through redelivery: it short-circuits on the consumed event. The
pool is permanently rule-less and nothing reports it.

So for inventory, **"accept and ignore the field we cannot use" is strictly worse than
parking.** Parking is loud (readiness latches false), reversible, and resolves itself the
moment a capable binary deploys. Consuming is silent and final. The general rule this ADR
therefore adds: *a consumer may accept a new schema early only if handling it is idempotent
with respect to what a later binary would need to do* — which holds when the consumer
ignores the new field by construction, and fails when the consumer records completion.

Inventory's arm consequently lands in TKT-181, together with the projection that makes it
honest, and catalog's producer change deploys after it.

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
