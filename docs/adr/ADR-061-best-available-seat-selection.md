# ADR-061: How best-available seats are selected

Date: 2026-08-18

## Status

Accepted (TKT-81)

## Context

ADR-041 built a per-pool projection of seat adjacency so the orphan rule could be arbitrated
inside the deciding transaction, and closed by naming what it had *not* decided:

> **Bounded.** This ADR decides *arbitration*, not selection. Best-available seating (TKT-81)
> will need adjacency too and can read the same projection, but its relaxation rules are its
> own decision.

This is that decision. A buyer asks for *N seats together* and inventory must answer with
*which* N — choosing and claiming them in one transaction, because a selection made outside
the lock is advisory: two claimants would pick the same free run, or pick overlapping runs
and jointly strand what lay between them. That is the same failure ADR-010 exists to prevent
and ADR-041 restated for the orphan rule.

Three facts about the system constrain the answer before any design is considered.

**There is no seat rank data. Anywhere.** The story's acceptance criteria cite "the map's
adjacency/**rank** data (TKT-3 provides both)". Adjacency exists; rank does not, in either
catalog or inventory, and TKT-3 remains an unshaped Backlog epic. Every `rank` in the
codebase belongs to pricing- and fee-rule resolution (ADR-036, ADR-046, ADR-047). So
"best" cannot mean "most desirable" — there is no datum that would order seats by desirability
— and any design pretending otherwise would be inventing a product model inside an
engineering ticket.

**Adjacency is a linked list, and a linked list cannot be searched.** `seat_claim_adjacency`
stores, per seat, the identity of its left and right neighbour. That answers *"given these
seats, would anything be stranded?"* — arbitration's question — in a bounded number of index
lookups. It cannot answer *"find me four seats together"*: there is no head to start from, no
order to sort by, and migration 0011 deliberately added no second index, because at the time
nothing queried adjacency any other way.

**The claim path is the platform's most contended transaction.** Whatever selection costs, it
is paid while holding the pool row lock that serialises every claimant on that performance.
ADR-041 already rewrote `orphanedSeatsQuery` once for precisely this reason: its first version
scanned the whole pool under that lock.

## Possible Solutions

- **Option 1 — walk the linked list at claim time.** A recursive CTE from each
  `left_identity IS NULL` head, chasing pointers to reconstruct runs. Needs no new data, and
  puts a recursive query over an unindexed access pattern inside the lock — reintroducing
  exactly the O(map)-under-contention cost ADR-041 removed. It also needs a new index on
  `(pool_id, left_identity)` merely to *find* the heads, so it does not even avoid a schema
  change.

- **Option 2 — order by seat identity.** Free, and wrong. Identities are free text
  (`section/row/seat`), so lexical order puts `A/1/10` before `A/1/2`: a "contiguous run" of
  seats 1, 10, 11, 12 in a wide row. It would also silently join rows whose labels happen to
  sort adjacently.

- **Option 3 — call catalog for geometry at claim time.** ADR-041 rejected this for
  arbitration and every word of that rejection applies here: a synchronous cross-service call
  inside the transaction holding the pool lock queues every claimant for that performance
  behind catalog's latency and availability.

- **Option 4 — project the ordering catalog already sends (chosen).** Inventory's geometry
  fetch *already* reads each row's `position`, sorts by it, and derives neighbour edges — then
  discards the row and the position. Keep them: two columns on `seat_claim_adjacency`
  (`row_key`, `position`) and an index on `(pool_id, row_key, position)`. Selection becomes an
  ordered, bounded, index-served range scan, and contiguity becomes "consecutive positions
  within one row" — a statement SQL can group without recursion.

## Decision

**Option 4.** Best-available selects over projected `(row_key, position)` ordering, inside the
same transaction and pool lock as the claim.

### What "best available" means here, stated so it cannot be over-read

**The first legal run in projected order, within a bounded scan.** Precisely:

1. **Contiguous means consecutive positions in one row.** Rows and sections never connect —
   ADR-041's rule, unchanged and load-bearing: a run spanning two rows is a party seated
   across a gangway.
2. **Ordering is catalog's `position`, re-based to 1..n per row.** Catalog positions must
   ascend and be unique within a row but need *not* be contiguous — a row authored 10, 20, 30
   is legal spacing, not missing seats. Re-basing at derivation is what makes "adjacent in the
   row" and "consecutive positions" the same statement; carrying raw values through would read
   three neighbouring seats as three one-seat runs.
3. **`row_key` is the row's catalog UUID, not its label.** Labels repeat across sections —
   "row A" exists in every one — and a label-keyed projection would merge rows that do not
   touch.
4. **This is an ordering, not a ranking.** A lower `row_key` is not a better seat. The system
   holds no seat-desirability data, and this ADR does not invent any. When real rank arrives
   (TKT-3, TKT-5) it becomes a new comparator ahead of this one; the shape of the query is
   where it would go.
5. **The scan is capped at `MaxBestAvailableScan` (400) seats in projected order.** So the
   answer is the first legal run *within that window*, and a refusal means "not within the
   window", never "nowhere in the house". This is stated in the OpenAPI description as well as
   here, because a caller who believes the stronger claim will read a refusal as a sellout.

### The relaxation rules (the story's AC2), explicit and deterministic

- **No party splitting.** If no single run of N exists, the request is **refused**, not split
  across rows. Splitting is a product decision with real revenue and real UX consequences
  (which party gets separated, by how much, with what consent), and nothing in this ticket
  establishes what the right answer is. Refusing is the reversible choice: a later ticket can
  add splitting behind an explicit request field, whereas silently splitting becomes the
  contract the moment a buyer receives it.
- **A run that would strand a seat is skipped, not refused.** On a pool with orphan prevention
  enabled, selection excludes any window whose placement would newly isolate a free seat, and
  moves on to the next candidate. The request is refused only when *every* candidate would
  strand one. Making the rule part of *eligibility* rather than a check *after* choosing is
  what stops best-available reporting "unavailable" about a map that still has legal runs in
  it.
- **Only NEWLY stranded seats count**, and a row end has one neighbour while a one-seat row has
  none. Both are ADR-041's rules, reused rather than restated — a seat already isolated by an
  earlier refund must not poison every later claim in its row.
- **Rule-off pools apply no orphan filter at all.** The venue turned it off deliberately.

### Idempotency covers the request, not the outcome

This is the one place best-available cannot copy the named-seat path. A seat hold fingerprints
the *seats*; a best-available request has none at the moment the key must be resolved. So the
fingerprint covers `(mode, organizer, slot, ticket type, seat_count, unit amount, currency)`,
and **a replay re-reads the seats from `claim_seats` rather than re-running selection**.
Re-selecting on replay would hand a retrying client a second run under a key it believes names
one hold — a silent double-sale, and the failure mode most specific to this endpoint.

The fingerprint carries a mode discriminator so a key spent on a named-seat hold conflicts here
rather than replaying.

### Two refusal codes, and they must not be collapsed

- `best_available_unavailable` — this slot cannot seat a party of that size right now.
  **Retryable**; a smaller party may succeed. Offer fewer seats; do not report a sellout.
- `best_available_unsupported` — this slot has **no ordering projection**, so best-available
  will never succeed there for any size until the performance is re-provisioned.

One is a property of the request, the other is an operational defect with an operator remedy.
Answering both the same way makes a broken pool indistinguishable from a sold-out show to the
very people who could fix it — the same distinction ADR-041 drew when it kept
`orphan_prevention_enabled` separate from the projection so that "rule off" and "projection
missing" could never look alike. Neither code carries `seat_identities`: no seats were chosen,
and that field already carries two opposite meanings (requested-and-lost for `seat_taken`,
free-and-unrequested for `orphaned_seats`).

### Existing pools are upgraded by re-provisioning, not by a migration

The new columns are **nullable**, and NULL means "provisioned before ADR-061" — a
distinguishable state, which is what `best_available_unsupported` reports.

**There is no backfill, and that is a decision rather than an omission.** `position` could be
recovered by walking each list from its row-end head. `row_key` could not: the row's identity
was never projected, and any value synthesised in a migration (the head's identity, a hash, an
ordinal) would fail to match what the consumer derives on the next re-provision — leaving two
incompatible row namings in one projection, with nothing to report it. A recursive walk across
every rule-enabled pool inside ADR-008's 30-second migration bound is the smaller objection.

Repair therefore runs through ADR-041's existing correction-wave machinery: re-emit, and
inventory's schema-5 handler upgrades the pool. **That required changing the adjacency write,
and the change is deliberately asymmetric.** `ProvisionSeated` inserted adjacency with
`ON CONFLICT DO NOTHING`, which is right for a replay and wrong for an upgrade — the identical
trap the same function already documents one table up for the rule flag, where "the difference
is invisible from here, because both arrive as 'the pool already exists'". Under DO NOTHING a
correction wave is a silent no-op on every adjacency row and the pool stays unselectable for
ever while the wave reports success.

So the conflict clause now updates **`row_key` and `position` only**, and deliberately leaves
`left_identity` and `right_identity` untouched. The edges are the substrate arbitration runs
on: a live claim was decided against them as they stood, and a later publication that rewrote
them would retroactively change what that decision meant. The ordering columns are additive,
read only by selection, and carry no such history. Changing that clause to update the edges as
well is not a cleanup; it is a different decision about immutability and belongs in its own ADR.

### What is bounded, and what is only probable

Two different guarantees, and conflating them would overstate the second.

**Hard:** `MaxBestAvailableScan` bounds the read to 400 projected rows *of one pool*,
regardless of plan. The pool scoping is structural; the cap is a literal `LIMIT` in the
statement.

**Planner-dependent:** that the read is an *ordered index scan* which terminates at the cap,
rather than a scan of the pool's whole projection followed by a sort. The index makes the good
plan available and the `LIMIT` makes it terminating, and with realistic statistics PostgreSQL
chooses it — but it is the planner's choice, and a statistics shift could still pick the sort.
The pinned plan test asserts the good plan under bound parameters at house scale. Under a
*generic* plan the same statement gets a bitmap scan plus a sort, because with no bound value
the row estimate is small enough that sorting looks cheaper; that is recorded here rather than
hidden, and it is why the cap exists as an independent second bound. Worst case is a sort of
one pool's projection, never of the map.

There is **no retry loop**, and its absence is the design rather than an oversight (the story's
"bounded retries"). Serialisation happens in the lock queue; by the time this transaction reads
the projection there is nothing left to race with, so a retry could only fire on a conflict that
cannot occur.

## Consequences

- **Positive.** Selection and claim cannot race — they are one statement sequence under one
  lock. Contiguity is real geometry, not string order. No cross-service call on the claim path.
  The orphan rule is honoured by construction rather than bolted on. Work under the lock is
  bounded by a constant, not by the size of the house.
- **Negative — a real operational cost, stated plainly.** Best-available works **only on pools
  that carry an ordering projection**, which today means pools provisioned with orphan
  prevention enabled. A venue that deliberately turned the orphan rule off cannot use
  best-available. Decoupling the two means changing what catalog emits and what
  `ProvisionSeated` requires — an ordered, multi-ticket, three-service rollout of the kind
  ADR-041's own delivery section documents — and bundling it into a ticket about selection
  would have joined two independent risks. `best_available_unsupported` exists so the
  limitation is visible at runtime instead of presenting as a sellout. **This is the follow-up
  work this ADR most expects to be superseded by.**
- **Negative.** Immutable ordering is duplicated per pool, alongside the adjacency ADR-041
  already duplicates, for the same reason and with the same trade.
- **Not covered.** Party splitting. Seat quality or price-tier preference in selection
  (needs TKT-3/TKT-5 data that does not exist). Accessible-seat pairing. Holding a run across
  several performances. Any notion of "best" that is not "first in projected order".
- **A caveat worth naming (ADR-021's discipline).** This is an honest-writer guarantee. The
  projection lives in inventory's database; anyone who can write there can make selection
  return whatever they like. It constrains buyers, not an adversary with database access.
