# ADR-042: Staff identity lives in catalog; the back office holds the session

Date: 2026-08-03

## Status

Accepted

**Amended by TKT-197 (2026-08-03)** — the deputy now has a rule. See § *TKT-197 amendment*.

**Amended by TKT-191 (2026-08-03)** — *Decision 1 only*. Everything else stands.

`POST /staff/authenticate` is **no longer public**: it is guarded like every other unsafe catalog
operation, by a dedicated `CatalogStaffWriteCredential`. The **conclusion** changed; the
**reasoning** did not. Decision 1's objection was to placing the *shared*
`INTERNAL_SERVICE_TOKEN` — one value opening commerce's refunds and inventory's operational holds —
inside a public-facing SSR process. A catalog-only credential removes that objection entirely, and
leaving one operation public would have meant an exception list inside a fail-closed scheme. The
earlier decision was not wrong; its constraint was lifted.

What this does **not** change: the anonymous login form is still the public deputy, so credential
**guessing volume is unaffected** and TKT-195 is still required. See § *TKT-191 amendment* below.

## Context

Until TKT-190 the back office had no authentication at all. `/admin/` was served to any caller who
could reach the gateway, and `web/backoffice/astro.config.mjs` justified disabling Astro's CSRF
guard with the words *"the gateway is the trust boundary for this internal, single-organizer staff
tool (no auth yet)"* — with a standing note to revisit *"if the back-office … gains per-user auth"*.
TKT-190 is that revisit.

Three things constrain where staff identity can go.

**ADR-002 cuts the system at five services plus a gateway**, and assigns *organizers/tenants* to
`catalog`. The gateway is the single public entry point and its route table is the security
boundary: `/api/<svc>/internal/` is registered to an edge-deny handler, so internal routes are
unreachable from outside by construction. The gateway owns no database and its `go.mod` has no
requires at all.

**`INTERNAL_SERVICE_TOKEN` is one shared value across every Go service** (`compose.yaml`, the
`&go-env` anchor). Whatever holds it can call `commerce`'s `/internal/orders/{id}/refunds`,
`inventory`'s `/internal/operational-holds` and `/internal/slots/{id}/capacity-adjustments`, and
`access`'s refund surface — not just the endpoint it was given the token for.

**Astro's `security.checkOrigin` cannot simply be switched back on.** The Node adapter builds its
request URL from the container's own `Host`, which behind the gateway is a service name, so the
comparison against the browser's `Origin` never matches — it 403s every POST. That is what TKT-105
found, and it would still be wrong behind TLS termination.

## Possible Solutions

- **Option 1 — a sixth `identity` service:**
    - Pros: the cleanest boundary, and the natural home if authentication ever grows (SSO, MFA,
      customer accounts sharing the machinery).
    - Cons: a direct ADR-002 deviation. Costs a new module, image, Compose service, migrate job,
      smoke override, `go.work` entry and gateway route before a single line of domain logic. Every
      caller becomes coupled to it, so it is the hardest option to walk back.
- **Option 2 — the gateway owns a staff store and enforces at the edge:**
    - Pros: one enforcement point, ahead of every service; nothing reaches an upstream unauthenticated.
    - Cons: gives a database, migrations, a KDF and session state to the one component deliberately
      built as a stateless route table with zero dependencies. Domain state at the edge is cheap to
      add and expensive to remove: undoing it later is a data migration, not a deletion.
- **Option 3 — catalog owns the accounts; the back office owns the session (chosen):**
    - Pros: no ADR-002 deviation — catalog already owns organizers and tenant-scoped configuration,
      and staff administer an organizer. One migration, endpoints in an existing contract, no new
      deploy unit. Enforcement sits in the app that actually serves `/admin/*`. The gateway stays
      stateless.
    - Cons: sessions are process-local (below). Enforcement is per-app rather than central, so the
      *next* staff-facing surface must opt in rather than inherit.

## Decision

**Staff accounts live in `catalog`** (`staff_accounts`, migration `0015`), and **the back office
owns the session** and enforces it in Astro middleware.

Four sub-decisions follow, each of which is the part most likely to be misread later:

**1. The authenticate operation is PUBLIC, not internal-token guarded.**
> ⚠️ **Superseded by TKT-191** — the operation is now guarded by a dedicated catalog-only
> credential. The reasoning below still holds and is why the *shared* token was refused; read it as
> the record of that argument, not as current behaviour. See § *TKT-191 amendment*.

`POST /staff/authenticate` is declared in catalog's public contract and requires no
`X-Internal-Token`. Its only credential is the staff password.

The tempting alternative — an `/internal/` endpoint the back office calls with the shared token —
buys nothing and costs a great deal. It buys nothing because the login *form* must be anonymous by
construction (a staff member cannot sign in through a page that requires a session), so an
unauthenticated caller already has an unlimited credential-submission channel; making the endpoint
internal moves the front door without locking it. It costs a great deal because it would put the
shared internal credential inside a public-facing Node SSR process that renders operator input,
turning any SSRF or injection defect there into access to every service's internal surface.

Submission volume is a real problem, and the control for it is rate limiting — **TKT-195**, covering
the endpoint *and* the form. It is not addressed here.

**2. Sessions are in-process and are not persisted.** A back-office restart signs everyone out, and
a second replica would not share them. Both are true, and both are acceptable for a single-replica
Compose staff tool. The alternative costs a table, a migration and an expiry sweep to buy durability
nobody has asked for. When someone does, the session module is replaced without moving the
enforcement point — which is why this is the reversible choice, not merely the cheap one.

Expired entries are swept **on sign-in**, not only when their own token is presented again. Reading
is not enough on its own: the sessions that are never presented again — the tab someone closed — are
the common case, so expiry-on-read alone would let the map grow for the life of the process.

Sweeping is still not a bound, and the second review pass caught the claim that it was. Every
sign-in mints a new token while the old ones live out their full eight hours, so **one** valid
credential can inflate the map without limit — and the sweep is what degrades with it. The bound is
a **cap of five concurrent sessions per staff member**, oldest evicted: signing in on a sixth device
ends the first. The cap is per principal and never global, because a global one would let one busy
account evict a colleague's live session, turning a safety limit into a denial of service.

**3. CSRF is answered by two controls, and they do different things.**
- `SameSite=Lax` on the session cookie means the cookie is **not transmitted** on a cross-site POST,
  so a forged request arrives with no session. This depends on the browser honouring it.
- A **proxy-aware origin check** (`web/backoffice/src/lib/gate.ts`, run by the middleware on every
  unsafe method) compares `Origin` against the **public** origin the gateway reports through
  `X-Forwarded-Proto`/`X-Forwarded-Host`. Go's `httputil.ReverseProxy.SetXForwarded()` overwrites
  both from the real inbound request, so a client cannot forge them *through the gateway*. This does
  not depend on browser cooperation, and fails closed for callers that send no `Origin` at all.

Astro's own `checkOrigin` stays disabled, for the reason in Context. A synchronizer token was
rejected: it would add hidden fields and server state to every existing authoring form to defend
against the same thing, using an origin the trusted gateway already supplies.

The session cookie is scoped `Path=/admin`, **not** `Path=/`. The gateway serves the storefront at
`/`, the scanner at `/scanner/` and every service API at `/api/*` — all on the **same origin**. An
origin-wide cookie would therefore be attached by the browser to every storefront page view, every
scanner request and every API call, so any access log, error report or diagnostic echo in those
unrelated surfaces would capture a live, reusable back-office credential. `HttpOnly` does not help
here: it stops scripts reading the cookie, not servers receiving it.

**A password longer than 72 bytes is refused before the identifier is looked up**, and is never
handed to bcrypt. This is not input hygiene, it is the same timing property as above: bcrypt returns
`ErrPasswordTooLong` past 72 bytes *without doing the work*, and the KDF is the only thing masking
the cost difference between a row that was found and `sql.ErrNoRows`. The contract's `maxLength: 72`
cannot catch it — OpenAPI counts **characters**, bcrypt counts **bytes**, so 72 multibyte characters
validate cleanly at three times the limit. Both findings came from the TKT-190 adversarial review.

**4. Sign-in is by identifier alone, so `identifier_key` is unique across the whole table**, not per
organizer. Two organizers holding the same address would make the lookup ambiguous. v1 has a single
organizer; multi-organizer sign-in needs an organizer selector on the form and is out of scope.

### The adversary this constrains — and the ones it does not

Per ADR-021's rule, the claim is stated for the adversary it actually holds against.

**Closed:** an unauthenticated caller reaching the gateway can no longer read any `/admin/` page;
a captured session cookie stops working the moment its holder signs out or the eight-hour absolute
lifetime elapses; a cross-site page cannot drive a staff member's browser into a state-changing
back-office request; and an account enumerator learns nothing from either the response bytes or the
number of KDF comparisons, which are equal for an unknown identifier and a wrong password.

**Open, and deliberately so:**
- ~~**`/api/catalog/*` write endpoints remain unauthenticated.**~~ **Closed by TKT-191.** Every
  unsafe catalog operation now requires the staff-write credential; an unauthenticated caller
  reaching the gateway is refused. What replaces this gap is a narrower one, stated below: catalog
  authenticates the **deputy**, not the staff member.
- **Anything already inside the Compose network** can address the back-office container directly and
  forge `X-Forwarded-*`, or address catalog directly and skip the gateway entirely.
- **Anyone holding `INTERNAL_SERVICE_TOKEN`** already has every service's internal surface; nothing
  here changes that.
- **Anyone who can write to catalog's database** can insert a staff account. State inside the
  database cannot constrain an adversary who writes to the database (ADR-021). This is
  authentication, not tamper-evidence, and no part of it is claimed to be.
- **Login attempt volume is unbounded** until TKT-195.

## Consequences

- **Positive:**
    - No new service, no new database, no new deploy unit; the gateway keeps its zero-dependency
      stateless-proxy property.
    - Every reversal is a deletion rather than a data migration: sessions are a module, accounts are
      one table in a service that already owns the tenant.
    - The back office stops using the hard-coded `DEFAULT_ORGANIZER_ID` and reads the organizer from
      the authenticated principal, which is the wiring the code comment had been waiting for.
- **Negative:**
    - Enforcement is per-app. A future staff-facing surface outside this Astro app inherits nothing
      and must gate itself — TKT-191's route matrix is where that stops being a per-app decision.
    - A back-office restart signs every staff member out, with no warning.
    - `role` is stored but interpreted nowhere. A reader of the schema will see a column that does
      nothing until TKT-191, and the migration says so.

## References

- TKT-190 (US-B1), TKT-22 (epic), TKT-191 (the role matrix), TKT-195 (login rate limiting)
- [ADR-002](./ADR-002-services-from-day-one.md) — the five-service cut; catalog owns organizers/tenants
- [ADR-004](./ADR-004-cache-first-read-path.md) — the "never" tier that auth responses take
- [ADR-006](./ADR-006-astro-storefront-shell.md) — the Astro SSR shell these pages live in
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary before claiming a guarantee
- [ADR-022](./ADR-022-out-of-band-service-migrations.md) — migration `0015` runs in the existing `catalog-migrate` job
- [browser-submit is the only checkOrigin catch](../learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md) — TKT-105


## TKT-191 amendment — catalog authenticates the deputy

Catalog requires `X-Catalog-Staff-Write-Token` on **every unsafe operation in its contract**. Public
reads opt out explicitly with `security: []`, so a newly added operation is closed by construction.

**The credential is catalog-only.** It is generated independently of `INTERNAL_SERVICE_TOKEN`, held
by catalog and the back office and nothing else, and deliberately absent from the shared `&go-env`
Compose anchor. A smoke test presents it to inventory's internal surface and asserts it does not
open, so the separation is tested rather than asserted.

**What this authenticates, precisely.** Catalog learns that a write came from **the back office**.
It learns nothing about *which staff member*, and enforces nothing about what that person may do.
The back office is therefore a **deputy** holding a credential that can perform every catalog
write, and today its rule is "any signed-in staff member". Per-role authorization is **TKT-197**.

Say it that way. "Catalog writes are authenticated" is true; "catalog enforces who may write what"
is not, and will not be until TKT-197.

**Consequences, honestly:**
- Compromise of the back-office SSR process now has **catalog-write** impact — it did not before,
  because it held no credential at all. It still has no access to any other service's internal
  surface, which is the trade the dedicated credential buys.
- Two secrets exist where one did. `make up` generates both, separately; one leaking does not imply
  the other.
- Catalog refuses to start without it (`runtimecfg.RequiredCredential`), following TKT-83: no
  working default ships in the repo.

**Adversaries closed:** an unauthenticated caller reaching the gateway can no longer create,
publish, archive or edit anything in catalog.

**Adversaries still open, unchanged by this ticket:** anyone inside the Compose network addressing
catalog directly; anyone holding either credential; anyone who can write to catalog's database
(ADR-021 — state inside the database cannot constrain an adversary who writes to it). This is
authentication, not tamper-evidence, and none of it is claimed to be.


## TKT-197 amendment — the deputy's rule is the role matrix

TKT-191's amendment said catalog authenticates the **deputy**, not the staff member, and that the
deputy's rule was *"any signed-in staff member"*. **That is no longer true.**

The principal now carries a **role** (`StaffRole`: `admin`, `box_office`, `finance`), and the back
office holds one declared **route→role matrix** (`web/backoffice/src/lib/authorization.ts`) read by
both the gate and the navigation. Two readers, one source — because the failure mode this closes is
a hidden link over a URL that still works, and that can only happen if the two disagree.

**Why the matrix lives here and not in catalog.** Since TKT-191 the browser holds no catalog
credential, so a staff member cannot reach a catalog write except through this app. That makes the
back office the only place a role can gate one. Putting role claims in catalog remains the rejected
option it was in TKT-191: it needs a signing key, a token format, and the vocabulary duplicated into
a second service.

**Fail-closed, at every layer that handles a role.** An unrecognised stored role does not
authenticate (catalog answers 500 — a data problem, not a credential one, so an operator is not sent
to reset a password that was never wrong). A response carrying an unknown role does not mint a
session. An unclassified route refuses. A role not on a route's list refuses. `provision-staff`
rejects a role outside the vocabulary at the point an operator can still fix the typo.

**The vocabulary has one home**: the `StaffRole` enum in catalog's contract, generating the Go
constants catalog validates against and the TypeScript union the matrix is typed on. Deliberately
**not** also a SQL `CHECK` — that would be a second hand-written vocabulary, and per ADR-021 it
constrains nobody who can write the database, who can equally drop the constraint or grant
themselves `admin`.

**Route selection follows Astro's own precedence — static, then dynamic, then rest.** Not
declaration order. A matcher that disagrees with the router about which route a URL *is* cannot be
trusted to say who may reach it: with first-match, adding a static finance-only
`/admin/venues/settlement` beside the admin-only `/admin/venues/[id]` would have handed the new page
the `[id]` rule, admitting admin and refusing finance — exactly inverted, and invisible to the
enumeration test because both templates exist in both sets.

**Anonymous access has one declaration.** The gate derives it from the matrix rather than from a
separate predicate. Two lists of what is public is one list too many, and the two had already
drifted apart on a bare `/admin/_astro` before anyone noticed.

**Role is snapshotted at sign-in.** There is no role-change surface today, so nothing can go stale.
When one is added it **must invalidate or refresh live sessions**; otherwise a demoted staff member
keeps their old role until sign-out, eviction, restart, or the eight-hour expiry.

**Adversary, precisely.** This constrains a **signed-in staff member driving a browser** — they
cannot reach a surface their role does not list, whether or not it was linked. It constrains nobody
holding `CATALOG_STAFF_WRITE_TOKEN`, nobody who controls the back-office process, and nobody who can
write catalog's database (who can simply set their own role). Authentication and authorization, not
tamper-evidence.

**What is not yet proven end to end.** "Box office cannot touch settlement" is half-proven: the
matrix can express and enforce a role-exclusive route, demonstrated on the catalog authoring surface.
The settlement surface itself does not exist — **TKT-23** owns it, and must add its page to this
matrix as `admin` + `finance` and drive the box-office refusal for real.
