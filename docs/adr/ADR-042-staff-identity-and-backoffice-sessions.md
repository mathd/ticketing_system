# ADR-042: Staff identity lives in catalog; the back office holds the session

Date: 2026-08-03

## Status

Accepted

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
- **`/api/catalog/*` write endpoints remain unauthenticated.** This ticket gates the back-office UI,
  not the catalog API. Anyone who can reach the gateway can still create and publish events by
  calling the API directly. TKT-191 is what closes that, and until it lands the phrase "the back
  office is authenticated" must not be read as "the catalog is".
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
