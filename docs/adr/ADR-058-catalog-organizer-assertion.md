# ADR-058: Catalog verifies which organizer a back-office write is for

Date: 2026-08-15

## Status

Accepted (TKT-245; decision taken under the owner-waived gates of that run, recorded on the ticket).
Fifth deliverable of the TKT-17 epic.

Amends nothing. **ADR-043 draws the line for where a guard lives, and this does not move it** — the
15 converted operations are contract operations, so their guard is a declared `security:` requirement
enforced by the validator, exactly where that ADR puts it.

Read alongside **ADR-053**, which recorded the gap this closes and stated the assumption it rested
on, and **ADR-049 § the TKT-221 amendment**, whose customer assertion this copies almost exactly.

## Context

ADR-042 put staff accounts in catalog and the **session in the back-office process**, and the two
never speak about a specific signed-in staff member afterwards. Catalog states the organizer once, at
sign-in, and then forgets it.

So every unsafe catalog operation took `organizer_id` **from the request body** and believed it. The
back office passed its session's organizer and never a form value — but that is a discipline in one
codebase, not a boundary catalog can enforce. Catalog's own code said so, at
`services/catalog/internal/api/server.go`:

> The whole thing rests on one assumption, which is the actual security property: **the back office
> is not compromised.** Catalog authenticates the PROCESS (ADR-021 — name the adversary), and no
> predicate in this file can change that.

TKT-236 narrowed the blast radius where it could — `UpdateChannel` gained an organizer predicate —
and ADR-053 recorded honestly that the predicate defends the back-office **form** path and nothing
else, because both the list's organizer and the update's were caller-supplied. A test
(`TestStaffCredentialCanStillEnumerateAndMutateAcrossTenants`) pinned the chain **as present**, in
the shape of ADR-021's rollback-gap test, and told its successor what to do if it ever started
refusing.

### The scope was wider than the ticket said

Counted from the contract at the time of writing: **29 unsafe operations**, of which

- **15** took `organizer_id` in the request body — the problem as filed;
- **13** are state transitions taking only a **path id** (`POST /series/{id}/publish`,
  `/festivals/{id}/archive`, `/performances/{id}/close`, the attach operations…). These reach the
  store with **no organizer at any layer** — `PublishSeries(ctx, id uuid.UUID)` — so there is no
  asserted field to distrust because there is no field at all;
- **1** is `POST /staff/authenticate` itself.

Two further sites belonged to the same problem and were not in the ticket text: `GET
/internal/channels` took a caller-supplied organizer in its **query string** (the enumeration ADR-053
recorded), and the back office's `SeatMapEditor` serialized the organizer into a **hidden form
field**, so on that one path the tenant made a round trip through the browser and came back as
client input.

## Decision

**Catalog signs a statement it can verify later without storing anything: "this is staff member S,
acting for organizer O, until T."** The staff member earns it by presenting their password; the back
office keeps it in its in-process session, server-side only, and forwards it on the writes it
proxies.

`v1.<staff id>.<organizer id>.<unix expiry>.<mac>` — HMAC-SHA256, base64url, key
`CATALOG_ORGANIZER_ASSERTION_KEY`, TTL 8h.

Five sub-decisions follow, each of which is the part most likely to be misread later.

### 1. The field is DELETED, not validated

`organizer_id` is gone from all 15 request schemas. Catalog does not compare a submitted organizer
against the verified one — there is nothing to submit. The validator answers *"property organizer_id
is unsupported"*, because those schemas are `additionalProperties: false`.

This is the whole decision, and the alternative is the tempting one. Every fix that *checks* a
submitted value keeps the trust boundary in the client and merely moves where it leaks: if *"can the
client influence this field?"* still answers *"yes, but only if it lies in a way I now check for"*,
it is not fixed. TKT-244 took three review passes to learn this on a different surface; this ticket
inherited the lesson rather than repeating it.

### 2. Both schemes are required TOGETHER, in one requirement object

```yaml
security:
  - CatalogStaffWriteCredential: []
    CatalogOrganizerAssertion: []
```

One object means AND. **Separate objects would mean OR** — one bracket's difference — and the OR form
means *either credential alone suffices*, so an assertion with no write credential would satisfy the
operation. That is an auth downgrade in the exact property this ADR adds.

Nothing local catches it. `securityRequires` in the invariant test asks whether **any** requirement
object names the scheme, so the OR form passes the pre-existing spec guard, passes a naive
"the assertion is declared on these operations" coverage test, and passes mutation testing — whose
mutants flip the mechanism, not the declaration. It is pinned by a dedicated test
(`TestCatalogOrganizerAssertionIsRequiredTogetherWithTheStaffCredential`), and that test was verified
by flipping all 15 to the OR form and confirming **every other invariant stayed green**.

### 3. `POST /staff/authenticate` must NOT require an assertion

It is the operation that **issues** one. The requirement is therefore per-operation on the 15, never
at document level: a document-level assertion requirement would make sign-in require the thing
sign-in produces — an unreachable endpoint, and a 401 nobody could ever satisfy.

### 4. The payload carries staff id and organizer, and deliberately NOT the role

Both signed fields are **immutable per staff row** (`staff_accounts`, migration 0015), so neither can
go stale inside a live token.

The role is different, and excluding it is load-bearing. ADR-042 snapshots `role` at sign-in and
warns it goes stale the day a role-change surface lands; signing it would make catalog authoritative
about a fact it cannot refresh, so a demoted staff member would carry their old role until the token
expired. Role enforcement stays in the back office (`web/backoffice/src/lib/authorization.ts`), and
**catalog still enforces no roles at all** — see § what this does not close.

The staff id is not load-bearing today. It is 36 signed bytes that save a canonical-format migration
the first time anything needs the principal, and the version prefix makes that migration survivable
rather than mysterious.

### 5. `GET /internal/channels` requires the assertion whatever credential opened the door

Including the shared `INTERNAL_SERVICE_TOKEN`, which **reverses** what a test previously asserted.

That token authenticates a *service*. It names no tenant, and the organizer used to arrive as a query
parameter — precisely the shape being removed. Keeping a query-parameter path "for services" would
leave the enumeration capability open to the **widest** credential in the system while closing it for
the narrowest.

Verified before removing it: this route has exactly **one** caller repo-wide, the back office
(`web/backoffice/src/lib/catalog.ts`). No Go service reads it, so nothing lost a capability it was
using. The sibling `/internal/channels/{id}` is untouched and still takes the internal token alone —
it names a specific row rather than enumerating a tenant's configuration. If a service ever needs the
list, it needs an organizer-bound credential of its own (ADR-056's shape), not a query parameter
restored.

The check there is **inline**, not declared, because the route is hand-mounted and outside the
contract — ADR-043's line, applied as written.

### Why not the alternatives

**A per-organizer credential** (ADR-056's shape) authenticates a *machine* confined to one tenant.
That is right for a reseller and wrong for an interactive staff tool, where the tenant is a property
of the signed-in human, not of the process: catalog's credential is one env scalar shared by one back
office serving all staff, and per-tenant provisioning and rotation would buy a boundary the signed-in
human already carries.

**Catalog owning the session** is the cleaner end state, and ADR-042 § 2 already promises the session
module can be replaced *"without moving the enforcement point"*. It costs a table, a migration, an
expiry sweep and moving enforcement out of the back office. This decision is a strict step toward it,
not a detour: the mint/verify seam is the one a catalog-owned session would use.

## Consequences

- **The session is clamped** to `min(SESSION_TTL_MS, assertion lifetime)`. Equal TTLs are not enough:
  catalog mints at T1 on its clock and the back office stamps its session at T2, so an 8h session
  holding an 8h assertion outlives it by the round trip plus skew — a 401 from catalog on a session
  the back office still believes is live. The storefront learned this for the customer assertion; the
  coupling is written down in both places because nothing enforces it.
- **Deployment is a coordinated cutover.** New catalog with an old back office means writes with no
  assertion (401); new back office with an old catalog means sign-in responses with no assertion.
  There is deliberately **no dual-acceptance shim** — a window that accepts a missing assertion is
  this gap, kept open on purpose. Live sessions do not survive: the back office is single-replica
  Compose and a deploy restarts it, which clears the in-process session map by construction.
- **Key rotation invalidates every live assertion**, and there is no overlap mechanism in this slice.
  Rotate by restarting catalog; staff sign in again.
- Catalog refuses to start when the signing key equals either credential, for a reason sharper than
  the usual separation argument: a key equal to the **write credential** would let anyone who can
  write mint their own tenancy.

## What this does NOT close — name the adversary (ADR-021)

The claim is stated for the adversary it actually holds against, because the previous two attempts at
this claim in `server.go` were both wrong in good faith.

**Closed.** A caller holding *only* `CATALOG_STAFF_WRITE_TOKEN` can no longer choose an organizer on
any of the 15 converted writes, and can no longer enumerate a tenant's channel registry at all. The
organizer is unsubmittable rather than validated.

**Since TKT-251 this extends to the 13 path-id transitions** (§ item 5 below), by a different
mechanism: they had no field to delete, so they take the verified organizer into a store-tier SQL
predicate instead. All 29 unsafe operations now answer the same way.

**Not closed, and each of these is a real residual:**

1. **A compromised back office.** It holds live assertions in memory for every signed-in staff member
   and can replay any of them. This is the same adversary ADR-053 named. The assertion **narrows** it
   — to organizers with a live session, for eight hours — without eliminating it. Overclaiming here
   is exactly the failure ADR-021 exists to stop.
2. **Anyone holding the signing key.** They mint for any organizer, forever. The key is catalog-only
   and is never sent to the back office, which is what makes (1) narrower than it would otherwise be.
3. **A writer with catalog database access.** Untouched, and untouchable by this mechanism: state
   inside the database cannot constrain an adversary who writes to the database.
4. **Replay of a live stolen assertion** until it expires. It is a bearer token, exactly as commerce
   says of its own.
5. **The 13 path-id transition writes — CLOSED for the staff-token holder by TKT-251.**

   As written, this item said a caller could still publish, archive, close, reopen or re-wire
   another tenant's resources by id, because those operations carried no organizer at any layer, and
   that *"catalog can verify the organizer"* was true of the 15 writes and the channel read and
   **false of the transitions**. That sentence was correct on 2026-08-15 and is no longer.

   TKT-251 gave all 13 the remedy this item named — a **store-tier organizer predicate** — and the
   claim now reads the same way for all 29 unsafe operations: a caller holding only
   `CATALOG_STAFF_WRITE_TOKEN` cannot choose an organizer on any of them. The shape, because it is
   the part most likely to be misread later:

   - The predicate is **in the SQL of the locked lookup**, not a check beside it (ADR-018 — the
     decision happens under the row lock, so a check outside it is a TOCTOU on the row the
     transition is about to move). `transitionFestival` did not even select `organizer_id`; the
     column was added as a predicate, never as a value to compare afterwards.
   - **The attach operations' two-row comparison was never authorization.**
     `AttachPerformanceToSeries`, `AttachDayToFestival` and `attachSeasonMember` compared the two
     rows' `organizer_id` to *each other* — which two resources of the same victim tenant satisfy.
     Both lookups now carry the caller's organizer. The same-organizer comparison remains, because
     it still catches the legitimate case (your own two resources belonging to different events)
     and still answers `ErrOrganizerMismatch`.
   - **A cross-tenant miss is `ErrNotFound`**, indistinguishable from an unknown id, so the refusal
     never confirms a guessed id is real — and catalog's ids are exposed by public reads.
   - `PublishPerformance` needed the predicate in **two** places: the atomic `UPDATE`, and the
     fallback read that classifies *why* nothing flipped. Scoping only the update refused the write
     while still answering `ErrGroupedSlotLifecycle` / `ErrNotSellable` / `ErrIllegalTransition`
     about a row the caller does not own — the write was closed and the information channel was not.
   - `PublishSeatMap` is deliberately **not** the ADR-029 family-lock shape. That lock serves
     `EditSeatMap`/`PinSeat`, which resolve the family's current published version; publish flips one
     specific draft row by id and resolves no version, so it takes the predicate on its conditional
     `UPDATE` and on the canonical re-read, and nothing else.

   **Still open, and unchanged by TKT-251:** items 1–4 and 6–7 below apply to the transitions exactly
   as they apply to the 15. A compromised back office replays live assertions; the signing-key holder
   mints at will; **a writer with catalog database access is untouched** — state inside the database
   cannot constrain an adversary who writes to the database (ADR-021).
6. **Roles.** Catalog enforces none. The assertion names a staff member but authorizes nothing on
   their behalf beyond naming their tenant.
7. **Multi-organizer staff.** Out of scope by ADR-042 § 4; one organizer per staff row, so an
   assertion cannot express "acting for a different tenant I also administer".

## Tests that pin this

- `TestCatalogOrganizerAssertionIsRequiredTogetherWithTheStaffCredential` — the AND-ness (§ 2).
- `internal/store/organizer_predicate_smoke_test.go` (TKT-251) — the transitions' cross-tenant
  refusal, asserted at the **store tier against real PostgreSQL**, because that is where the
  mechanism is: the API-tier fake scopes in a Go map and stays green with the SQL predicate deleted.
  Each case asserts three things — the attacker is refused with `ErrNotFound`, the victim's row did
  not move, and **the owner can still perform the operation** (a `WHERE false` satisfies the first
  two). Both mutants were run: deleting `transitionSeries`' predicate, and deleting the scoped
  existence check in `PublishPerformance`'s classification path — the second leaked
  *"performance has no ticket type"* about a victim's row, and the test caught it.
- `TestCatalogWritesTakeTheOrganizerFromTheAssertionAndNotTheBody` — derived from the spec in both
  directions: no converted write still declares the field, and no unsafe write still takes one.
- `TestConvertedHandlerRefusesWhenNoScopeWasVerified` — a handler reached with no verified scope
  refuses rather than writing for `uuid.Nil`. It exists because a mutation check found that deleting
  the refusal left the entire package green: every other test drives the router, where the validator
  always fills the slot, so nothing could reach the unfilled state.
- `TestStaffCredentialAloneCanNoLongerEnumerateOrMutateAcrossTenants` — the inverted TKT-236 test.
  **If it ever fails, this ADR is wrong; do not delete it.**
- `assertion_test.go` — mint/verify, tamper on every field, expiry boundary, nil-uuid, empty key,
  indistinguishable refusals, and the payload shape (so a future field is a deliberate
  canonical-format change).
- `session.test.ts` § the clamp — the session cannot outlive its assertion.
