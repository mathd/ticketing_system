# ADR-018: Catalog slot transition concurrency and grouped-member lifecycle ownership

Date: 2026-07-15

## Status

Accepted (TKT-53 / TKT-58; recorded here in TKT-64, extracted from `AGENTS.md`)

## Context

Catalog slot transitions both mutate a row and owe a domain event derived from the
post-transition row. ADR-015 §4/§6 fixed the locking and emit-after-commit rules for the
*series* path; the same hazards exist on the *direct per-slot* endpoints, and were learned
the hard way rather than decided:

- **Phantom events.** A conditional `UPDATE … WHERE status = x` followed by a *separate*
  re-read to derive the event is racy. A concurrent opposite or terminal transition can
  commit in between, and the re-read then derives an event that contradicts the row —
  `performance.archived` on a still-published row, a nil `archived_at` producing a
  mismatched deterministic id.
- **Grouped members.** ADR-014 §2 shipped `capacity_group_id` as a "forward-compat seam
  only"; TKT-53 made it real for festivals. A festival day whose status can be flipped
  out-of-band by the direct endpoints lets the group aggregate and its members disagree —
  the exact failure ADR-015 §1 avoided for series by not giving the group its own column.

Both rules lived as ~35 lines of prose in `AGENTS.md`, which every agent loads on every
task. The content is real and load-bearing, but it is an architecture decision, and it was
sized like one — it dominated a file whose job is to be a short index.

## Possible Solutions

- **Option 1: leave the rules in `AGENTS.md`.**
    - Pros: agents see them without a second read; zero work.
    - Cons: `AGENTS.md` is loaded into every context window, so the cost is paid on every
      task regardless of relevance; it makes the file's other pointers harder to find; and
      an architecture decision recorded outside `docs/adr/` is invisible to anyone reading
      the decision log — against this repo's own working agreement.
- **Option 2: extend ADR-015.**
    - Pros: ADR-015 already owns lifecycle + locking, and is lean.
    - Cons: ADR-015 is scoped to series/season grouping; festivals are a different grouping
      (`capacity_group_id`, ADR-014) and the direct-endpoint rule applies to *ungrouped*
      slots too. Widening ADR-015 in place would retroactively restate a decision accepted
      at the TKT-52 gate.
- **Option 3: a new ADR, with `AGENTS.md` reduced to a pointer.**
    - Pros: the decision lands in the log where it is discoverable and can be superseded;
      `AGENTS.md` shrinks to an index; the detail stays one hop away for the tasks that
      need it.
    - Cons: an agent touching a catalog transition must follow the link.

## Decision

We adopt Option 3. Two rules, scoped narrowly:

1. **A state-deriving slot transition decides under a row lock.** When a transition mutates
   state *and* derives an owed event's identity/payload from the post-transition row, *and*
   a concurrent opposite or terminal transition can interleave (archive, close, reopen), it
   must decide from the locked current row in one transaction: `SELECT … FOR UPDATE`, apply
   the transition atomically, commit, then emit at-least-once **after** the commit (the
   owed-marker pattern — never publish while holding the transaction). This is ADR-015 §4/§6
   applied to the direct per-slot path.

   A purely **monotonic** one-way transition whose event id cannot be invalidated by a racing
   transition (draft→published) stays correct under the plain conditional `UPDATE`, and stays
   lock-free. Reference impls: `ArchivePerformance` (locked) and `PublishPerformance`
   (monotonic) in `services/catalog/internal/store/postgres.go`; the same decision pattern
   underlies access `Redeem`.

2. **A grouped member's own publish/archive is refused, on the transition's existing
   decision.** A slot with a non-null `capacity_group_id` (today: a `festival_day`, CHECK-
   constrained to that kind) must not go through direct `PublishPerformance` /
   `ArchivePerformance`; both return `ErrGroupedSlotLifecycle` so the festival stays the only
   writer of its members' status.

   Enforce this on the decision the transition **already makes**, never as a separate
   pre-check — a pre-check outside the locked read (or outside the `UPDATE` predicate)
   reopens rule 1's race, since a slot can join a group between check and write. The two
   endpoints differ on purpose, matching the split above: `ArchivePerformance` is
   state-deriving, so it reads `capacity_group_id` under the same `FOR UPDATE` lock that
   decides the transition; `PublishPerformance` is monotonic, so it folds
   `capacity_group_id IS NULL` into its conditional `UPDATE` and only diagnoses the grouped
   case on the zero-rows path.

**This is not a general "group owns its members" invariant.** Closure is orthogonal and stays
per-member: `CloseSlot`/`ReopenSlot` deliberately do not consult `capacity_group_id`. Series
transitions do not guard it either, so a festival day that also belongs to a series can still
be flipped via `PublishSeries`/`ArchiveSeries` — ADR-015 accepts partial series states by
design. Read the code before relying on group ownership beyond these two endpoints.

## Consequences

- **Positive:** the phantom-event class is closed on the direct path by construction, not by
  reviewer vigilance; the festival aggregate has exactly one writer; `AGENTS.md` drops ~35
  lines from every context window; and the rule is now in the decision log, so a future
  grouping (passes TKT-12, lodging TKT-13) inherits it or supersedes it explicitly.
- **Negative:** the rule is one hop away from the file agents always read, so a transition
  written without following the link can still get it wrong — the guards in `store` and their
  smoke tests are the real enforcement, not this document. The deliberate asymmetry between
  the two endpoints reads as an inconsistency until rule 1 is understood, and the narrow
  scope (closure and series exempt) is a live edge that must be re-read, not assumed.

## References

- TKT-53 (festival grouping), TKT-58 (the corollary), TKT-64 (extraction)
- [ADR-014](./ADR-014-typed-dated-slot-implementation.md) — `capacity_group_id` as a seam
- [ADR-015](./ADR-015-series-season-grouping-and-lifecycle.md) — §4 locking, §6 emit-after-commit
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) — when the owed event's schema must bump
- `services/catalog/internal/store/postgres.go`, `store.go` (`ErrGroupedSlotLifecycle`)
