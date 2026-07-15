# ADR-015: Series and season grouping with slot-derived lifecycle

Date: 2026-07-14

## Status

Accepted at the TKT-52 plan gate.

*Extended by [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md): this decision stands; §4/§6
(lock the row, emit after commit) are generalized there to the **direct per-slot** endpoints, and to
festival grouping.*

## Context

US-010 groups the dated slots introduced by ADR-005 into an ordered show run
(`series`) and groups runs or standalone events into a program (`season`). The
existing slot lifecycle is authoritative and already carries deterministic,
at-least-once publication/archive events. Adding a second lifecycle state to a
series would allow the group and its slots to disagree, especially after a
direct per-slot archive.

## Decision

1. A series has identity, localized name, one event, and ordered membership,
   but no lifecycle column. Its state is derived from its member slots.
2. A slot belongs to at most one series. Membership freezes once any member
   leaves draft; attaching a non-draft slot or extending a launched run is a
   conflict. Thus an idempotent lifecycle call always operates on a stable set.
3. A season has separate series and event relations. Public expansion
   deduplicates by event ID, retains series framing for member slots, and shows
   every other public slot once.
4. Attach and lifecycle operations lock the series row first. A lifecycle
   operation then locks member slots in UUID order. Direct slot transitions
   lock only their one slot row, so they can wait but cannot form a lock cycle
   with the multi-row series path.
5. Series publish/archive preflight every member and mutate all members in one
   transaction. A conflict aborts the transaction and identifies the blocking
   slot. An owed closure event intentionally blocks a run archive until that
   slot transition is retried.
6. Events are emitted only after commit. Each member is emitted and marked
   immediately, in stable order; publication precedes archive for that member.
   A retry therefore resumes owed work without a group-level marker.
7. Public season detail uses the five-minute event-detail tier. Program
   membership changes on the same editorial cadence as event detail, and a
   300-second bound avoids inventing a longer tier before usage evidence.
   Localized series and season names use the catalog's existing locale
   validation and fallback resolver, so the storefront adds no new i18n path.

## Consequences

- Direct slot transitions may create legitimate partial series states; public
  reads filter archived members and retain the visible run.
- A single bad or event-pending member blocks the bulk transition, but the 409
  response names it so operators can repair the exact slot.
- Membership cannot be edited after launch. A future rescheduling workflow
  must model a new run or explicitly amend this decision.

## References

- TKT-52 / US-010
- [ADR-004](./ADR-004-cache-first-read-path.md)
- [ADR-005](./ADR-005-unified-dated-slot-admission.md)
- [ADR-009](./ADR-009-contract-first-apis.md)
- [ADR-014](./ADR-014-typed-dated-slot-implementation.md)
