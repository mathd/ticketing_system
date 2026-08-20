# ADR-056: A reseller is a third identity class, and its scope lives in the credential

Date: 2026-08-10

## Status

Accepted

Ticket: TKT-240 (epic TKT-17, US-CH6). Extends ADR-043 (where a service auth guard lives) and
ADR-051/ADR-055 (rate limiting) to a new surface; amends neither. Positions the new class against
ADR-042 (staff), ADR-043 (internal service token) and ADR-049 (customer).

**Number note:** this is 056 and not 055 because at the time `ADR-055` was used *twice* —
the on-sale write rate limiting ADR from the security review and TKT-239's presale unlock codes,
both Accepted on 2026-08-10 and both cited from code. The 2026-08-19 review resolved it: the
presale ADR is now [ADR-064](ADR-064-presale-unlock-codes.md), and `scripts/check-adr-numbers.sh`
fails the gate on a repeat. Bare `ADR-055` in this file means the rate limiting one.

## Context

An external reseller sells from the same inventory as everyone else. It is a machine, it
authenticates on every request, and it must be confined to one organizer and one channel.

The platform already had three identity classes and none of them fits:

| Class | ADR | What it authenticates | Why it is not this |
|---|---|---|---|
| Staff session | ADR-042 | A back-office **deputy**, per organizer | Human, session-scoped, administers rather than sells |
| Internal service token | ADR-043 | "something holding the service credential" | **One shared value** opening every service's internal surface; carries no individual |
| Customer session | ADR-049 | A **buyer** | Buys for itself; no channel, no organizer scope |

The tempting shortcut was to reuse the internal token. TKT-190 proposed exactly that and it was
caught at plan-review: `INTERNAL_SERVICE_TOKEN` opens commerce refunds and inventory operational
holds, so handing it to an external partner would hand them the platform.

## Decision

### 1. A reseller credential is a per-reseller database row, not a configured secret

`reseller_credentials` in **commerce**: `(id, reseller_id, organizer_id, channel_code, token_hash,
label, created_at, revoked_at)`.

**Several live credentials for one scope are permitted, deliberately.** The first version of this
decision put a partial UNIQUE index over live rows and called rotation "enrol-then-revoke" — which
that index makes impossible, since issuing the replacement while the original is live raises a
unique violation. The only workflow it allowed was revoke-then-enrol, taking the partner offline
between the two statements. Zero-downtime rotation needs the old and new credentials to coexist for
as long as the handover takes, so the constraint went rather than the workflow. `token_hash` is
UNIQUE, so nothing that authenticates can ever collide.

**Known gap (ai-review pass 2):** the overlap is currently *unbounded* — repeated enrolment yields
any number of live credentials, and `ListResellerCredentials` exists in the store but is **not
exposed as a CLI command**, so an operator cannot enumerate them to reconcile. Bounding the overlap
and shipping the listing command is outstanding work, recorded on the ticket rather than implied
to be done.

Commerce and not catalog, despite catalog owning the channel *registry* (ADR-002, ADR-024). The
credential authorises **selling**, and orders, reservations and attribution are commerce's. Issuance
validates registry membership against catalog; the column records what was issued. No cross-database
foreign key, per ADR-024's rule that historical attribution does not FK the registry.

### 2. The stored form is SHA-256 over a generated 32-byte token, deliberately not bcrypt

Catalog hashes staff passwords with bcrypt (cost 10). That is right there and wrong here, and the
difference is **how often the credential is checked and who chose it**:

- A staff password is verified **once per session**; a partner credential is verified on **every
  request**, on the on-sale hot path. A deliberately slow KDF there is a self-inflicted denial of
  service.
- A bcrypt hash **cannot be looked up**. It salts per row, so authenticating would mean scanning
  every credential and comparing each. A SHA-256 `token_hash` with a UNIQUE index is one indexed
  lookup.
- bcrypt's cost exists to compensate for **low-entropy human-chosen** secrets. This token is 256 bits
  from `crypto/rand`. There is nothing to compensate for.

**This reasoning does not transfer to anything a person types.** The precedent followed is access's
`scanner_devices` (a machine credential presented on every scan), not catalog's staff store.

### 3. The credential carries its scope, and the scope is never read from the request

`AuthenticateResellerCredential` returns the organizer, channel and reseller the credential was
**issued for**. Handlers take them from there and from nowhere else.

This is the load-bearing clause, and it is written this way because the alternative has already
failed twice in this codebase:

- **ADR-053**: catalog's staff credential can enumerate *and mutate* across tenants, because both the
  list's `organizer_id` and the update's are **caller-supplied**. Every statement is individually
  correct; the composition is not. Pinned by `TestStaffCredentialCanStillEnumerateAndMutateAcrossTenants`.
- **Access's scanner enrolment** was platform-wide while looking per-organizer, because the device was
  resolved and then what it was enrolled *for* was discarded.

Where a partner request also names an organizer — it must, because idempotency terms need it — the
handler **compares** it against the credential's and refuses with `403 partner_scope_mismatch`. The
comparison direction is the decision: the request's value is a claim to be checked, never a value to
be used.

**The shape a future write path must follow**, recorded here because it was built and removed rather
than never considered: resolve the target with a **SQL predicate binding all the scope terms at
once** (`id`, `organizer_id`, `channel_code`, `reseller_id`), not a load-then-compare in Go. A
load-then-compare is equivalent only if nobody ever forgets a term; a predicate cannot be partially
applied. A row that is not the partner's answers **404**, not 403 — "forbidden" would confirm it
exists. TKT-246 and TKT-23 inherit this.

### 4. The surface is a contract operation, so the guard is declared and validator-enforced

ADR-043's line, applied without exception: every partner operation declares
`security: [{PartnerCredential: []}]` and the OpenAPI validator enforces it **before routing**. Not a
header comparison in a handler. (This slice ships one such operation; the rule is written for the
surface, not for the count.)

The mechanical reason, not a stylistic one: a handler check is a thing the *next* partner-shaped
operation can be added without. A declaration is checked by the validator for whatever the document
says, and `TestEveryPartnerOperationDeclaresTheCredential` derives the requirement from the document
rather than from a list someone must remember to update.

Unlike `/internal/`, these paths **are** reachable from the edge. That is the point of the class.
They live under `/api/commerce/` and the gateway's existing `internal/` denial is unaffected; no new
route-table entry was added, because the table strips the whole matched prefix and a
`/api/commerce/partners/` entry would deliver `/availability` to commerce instead of
`/partners/availability` — verified by executing the stripping logic, not by reading it.

### 5. Rate limiting is per reseller

ADR-051's per-subject half is *fully available on this surface and nowhere else*: the credential
resolves in the validator **before the body is decoded**, so a partner has a stable identity at
limiting time. The customer on-sale writes are source-keyed precisely because they do not — a
distinction that survives this slice being read-only and is why the limiter is worth having now.

Per-source alone would be near useless for a partner — a server with a fixed address that
legitimately sends volume. Keyed on `reseller_id` rather than `credential_id`, so rotating a leaked
credential does not hand the partner a fresh budget.

### 6. Attribution is copied from the reservation, and it is historical

`reservations.reseller_id` and `orders.(channel_code, reseller_id)`, nullable, not backfilled — NULL
means "not a partner sale", which is what every pre-existing row is. The order's values are **copied
from the reservation** in the insert, so they survive replay and recovery identically and no request
field can attribute a sale to a reseller that did not make it.

**In this slice nothing writes a non-NULL `reseller_id`**, because there is no partner write path.
The columns and the copy-from-the-reservation mechanism ship anyway, deliberately: they are the
shape TKT-246 (the write) and TKT-23 (settlement) both need, the mechanism is the part that is easy
to get wrong, and adding a nullable column to a table is far cheaper than adding it later under a
migration that has to reason about existing partner sales. Said plainly so nobody reads the columns
as evidence that partner attribution is live: it is scaffolding with its semantics fixed, not a
working feature.

No foreign key to `reseller_credentials`: attribution is the historical fact of who sold this, and an
FK would let revoking or rotating a credential rewrite or block the record of past sales.

## What this does not protect against

Named explicitly, per ADR-021, because "secure" without an adversary is not a claim:

- **A stolen live token is replayable.** Hashing protects the credential at rest — a database dump, a
  backup, a slow-query log — and makes revocation immediate. It does nothing about a token in flight.
  TLS, idempotency keys and the per-reseller rate limit carry that, and none of them makes replay
  impossible.
- **A writer with commerce database access defeats all of it.** They can insert a credential row,
  clear `revoked_at`, or rewrite attribution. This is **honest-writer security, not tamper
  evidence.** The hash-chained lifecycle trail (ADR-021) is the shape that resists a hostile writer,
  and it protects access's domain, not commerce's.
- **Issuance-time registry validation can go stale.** The credential records the channel it was
  issued for; if catalog later disables or renames that channel, the credential still names the old
  code. Refusals then come from inventory's allocation lookup, not from the credential.

## Consequences

- A fourth machine identity would now have three precedents to choose between. The question to ask is
  the one that separated this from staff: **how often is it checked, and who chose the secret.**
- The partner surface is the first thing in commerce with a declared `securityScheme`. Commerce's
  spec previously had none, and there is deliberately **no document-level default** — adding one
  would place a requirement on twenty existing operations whose guard is an assertion header.
- **This slice is READ-ONLY, and that is the load-bearing consequence.** The partner surface reads
  availability for its own channel; it cannot hold or sell. A write was built and then removed,
  because a hold only means something if it consumes the credential's channel allocation, and
  inventory does not yet enforce that — the channel stops at catalog's fee resolution and never
  reaches the claim path.

  The seam closure that would have supplied it was **reverted in this same ticket**: forwarding the
  channel is necessary but not sufficient, because `POST /reservations` is unauthenticated and takes
  the channel from the request body, so forwarding alone let any caller consume a reseller's
  allocation (executed, not theorised). Closing the seam is therefore an authorization change —
  the allocation must say who may sell it, judged under the pool row lock — and that is **TKT-246**.

  Until then, **do not describe a partner as confined to its channel on any write path.** There is
  no write path. Adding one without TKT-246 would ship an operation whose stated security property
  is false.
- The **seated** half of that seam stays open and stays TKT-176's. A seated claim carries no channel
  at all, so the partner surface refuses a seated pool (`seated_pool_unsupported`) rather than
  half-fixing it. A test pins the seated hold body as channel-free; if it fails, TKT-176 was closed
  by accident.
