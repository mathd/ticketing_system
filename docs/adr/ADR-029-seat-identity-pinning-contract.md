# ADR-029: Seat-identity pinning contract for safe published-seat-map edits

Date: 2026-07-20

## Status

Accepted (TKT-104 / US-021)

## Context

A published seat map is immutable (TKT-103: the `status='draft'` write gate refuses
authoring on a published version). But organizers must be able to **edit** a published map —
rename/add/remove sections, rows, seats — and get a new version, **without ever orphaning or
duplicating a seat that a sale or hold already references.**

The stable handle a sale/hold references is the **seat identity** — the string
`"section/row/seat"` composed server-side when a seat is authored (TKT-102;
`seat_map_seats.seat_identity`, `UNIQUE (seat_map_id, seat_identity)`). Reserved-seat claims
(TKT-80) do not exist yet, so today no cross-service record references a seat identity. This
ticket must nonetheless define and prove the contract TKT-80 will consume, so that TKT-80 can
plug real holds in **with no catalog change** (COS-5).

Two hazards must be closed:

- **Orphaning.** An edit whose new geometry drops (or renames the identity components of) a
  seat that is currently referenced would leave a sale/hold pointing at a seat that no longer
  resolves in the current published version.
- **The edit-vs-sale race.** The decision "is this edit safe?" depends on *which seats are
  currently referenced*. A naïve check-then-act — read the referenced set, then write the new
  version — races a concurrent reference being taken between the read and the write. This is
  precisely the check-then-act shape ADR-018 rule 1 addresses for slot transitions.

A discipline constraint applies (ADR-021): "safe against sold inventory" here is a
**concurrency/consistency guarantee under honest writers**, not a tamper-evidence claim. It
must not be overclaimed.

## Possible Solutions

- **Option 1: hard-reject an orphaning edit.**
    - Pros: simplest to implement and to prove; a pinned identity *always* resolves directly
      in the current published version, so a consumer never dereferences; strongest alignment
      with ADR-026's never-strand-a-confirmed-claim spirit. Remap can be added later as its
      own ADR without invalidating the pinning invariant.
    - Cons: an organizer cannot rename/remove a section while it contains referenced seats —
      they must edit around the pinned seats or wait for the references to clear.
- **Option 2: express an orphaning edit as an explicit, recorded remap.**
    - Pros: organizer can rename freely; the remap records the old→new translation.
    - Cons: every consumer must dereference through a remap table to find the current
      identity; remap integrity (chains, merges A→B and C→B) must be separately proven; a much
      larger contract to commit to early.
- **Option 3: do nothing (published maps stay uneditable).**
    - Pros: zero work.
    - Cons: fails the epic's headline COS; organizers cannot correct a published map.

## Decision

We adopt **Option 1 (hard-reject)**, with a **two-sided row lock** closing the race.

1. **The pin is the fact-source.** A `seat_map_pins (map_family_id, seat_identity, pinned_by)`
   row records that a seat identity is referenced by a sale/hold. `pinned_by` is free-form
   text (`"sale:<order_id>"`, `"hold:<hold_id>"`) so TKT-80 writes real references without a
   catalog migration. A pin is **version-independent**: it applies to the whole map *family*
   (all versions of one edited map share a `map_family_id`), never to a specific version row —
   a version bump must not drop a pin.

2. **`EditSeatMap` is state-deriving, so it decides under a lock (ADR-018 rule 1) — a
   FAMILY-scoped advisory lock, not a row lock.** In one transaction it: takes
   `pg_advisory_xact_lock(hashtextextended(map_family_id::text, 0))`; then reads the family's
   current published version (`… WHERE map_family_id = … AND status='published' ORDER BY
   version DESC LIMIT 1`) and its pinned identity set; requires **every** pinned identity to
   appear **exactly once** in the submitted geometry (absent → `ErrSeatMapEditOrphansPinned`;
   duplicate → the new version's `UNIQUE(seat_map_id, seat_identity)` yields
   `ErrSeatMapConflict`); inserts a new published version (`version+1`, same `map_family_id`)
   with the new geometry; commits; then emits the existing `seat_map.published` event after
   commit. The predecessor is never mutated (COS-1).

   **Why an advisory lock and not `SELECT … FOR UPDATE` on the current row** (the subtle part,
   caught in adversarial review and reproduced): `EditSeatMap` makes a new version by
   **INSERT**ing a new row, which never conflicts with a `FOR UPDATE` lock on the *old* row. A
   `PinSeat` blocked on the old row would, once the edit commits, still hold the **stale** old
   row — PostgreSQL rechecks the locked row, it does **not** re-run the `ORDER BY … LIMIT` — and
   validate the seat against the old geometry, landing an orphaned pin. The same stale row lets
   two concurrent edits both derive `version+1` and collide. Serializing on the **family
   identity** (a lock immune to which row is current), then re-resolving the current version
   under that lock, is what actually closes it. `UNIQUE (map_family_id, version)` is the
   belt-and-suspenders backstop.

3. **`PinSeat`/`UnpinSeat` take the *same* family advisory lock (the two sides).** `PinSeat`
   takes the family lock, resolves the current published version freshly, validates the identity
   exists in it (else `ErrSeatIdentityNotFound`), and inserts the pin idempotently. `UnpinSeat`
   takes it too, so a release cannot be lost against an uncommitted concurrent pin. Because all
   three take the identical family lock, a concurrent edit and pin **serialize**: the loser sees
   the winner's committed result. Either the pin lands first and the edit is rejected as
   orphaning, or the edit lands first and the pin is rejected as not-found. It can **never** be
   that both succeed and leave a pin for a seat the current version lacks.

4. **No new event, no new schema.** A new version, once created, is just a published map, so it
   emits the existing `seat_map.published` (schema 1); its payload already carries id,
   organizer, venue, version (ADR-017: no consumer-semantic change → no bump).

5. **No new HTTP endpoint in this ticket.** The deliverable is the contract (this ADR + the
   store functions + the pin table); the editing UI is TKT-105. `EditSeatMap`/`PinSeat` are
   store functions, proven by store smoke tests.

## Consequences

- **Positive:**
    - A referenced seat identity always resolves, unchanged, in the current published version —
      the contract TKT-80 consumes verbatim (a hold writes a pin via `PinSeat`, releases via
      `UnpinSeat`; no catalog change).
    - The edit-vs-sale race is closed by construction and proven by a 30-iteration real-Postgres
      race test (`TestEditSeatMapDoesNotRacePin`) mirroring `TestArchiveDoesNotRacePublish`.
    - Previous versions stay immutable; existing seated performances keep referencing the exact
      version they were published against (TKT-103).
- **Negative / limits:**
    - **This is honest-writer consistency, NOT tamper-evidence (ADR-021).** State inside the
      database cannot constrain an adversary who writes to the database: anyone with catalog
      write access can insert or delete `seat_map_pins` rows, or write a `seat_maps` row
      directly, and defeat the guarantee. The contract guards against our own bugs and against
      concurrent honest writers — not an attacker. Do not describe it as "tamper-evident".
    - An organizer cannot rename a section (which would change its seats' identities) while any
      of its seats are pinned; they must keep the pinned seats' section/row/seat labels stable.
      If flexible remapping is later required, it is a **new** ADR layered on top — the pinning
      invariant ("a pinned identity resolves in the current version") holds under remap too;
      remap changes *how* it resolves, not *whether*.
    - `map_family_id` has no hard FK from `seat_map_pins` (a family id is shared by every
      version, so it is not unique); the relationship is enforced under the row lock in
      `EditSeatMap`/`PinSeat`, not by a constraint. A stray pin row is possible via direct SQL —
      see the tamper caveat above.

## References

- TKT-104 (US-021: edit a published map safely) — this ADR is COS-2's deliverable.
- [ADR-018](ADR-018-catalog-slot-transition-concurrency.md) — the row-lock-decide-emit-after-commit
  discipline this instantiates for map edits; the two-sided lock is the same reasoning applied
  to a second write path.
- [ADR-026](ADR-026-inventory-capacity-adjustment-clamp.md) — never strand a confirmed claim;
  forward-only. Hard-reject is the map-edit expression of that spirit.
- [ADR-005](ADR-005-unified-dated-slot-admission.md) — seat identity is a slot attribute the
  claim references; versioning keeps it resolvable.
- [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md) — name-the-adversary discipline; the
  reason this ADR states its guarantee as honest-writer consistency, not tamper-evidence.
- [ADR-019](ADR-019-catalog-read-path-scoping.md) / [ADR-020](ADR-020-catalog-index-build-concurrency.md)
  / [ADR-022](ADR-022-out-of-band-service-migrations.md) — index-backed family/pin reads, plain
  `CREATE INDEX`, out-of-band migration `0011`.
- TKT-80 (US-017: seat-level claims) — the downstream consumer of this contract.
