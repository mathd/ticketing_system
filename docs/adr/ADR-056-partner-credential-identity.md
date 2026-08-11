# ADR-056: A reseller is a third identity class, and its scope lives in the credential

Date: 2026-08-10

## Status

Accepted

Ticket: TKT-240 (epic TKT-17, US-CH6). Extends ADR-043 (where a service auth guard lives) and
ADR-051/ADR-055 (rate limiting) to a new surface; amends neither. Positions the new class against
ADR-042 (staff), ADR-043 (internal service token) and ADR-049 (customer).

**Number note:** this is 056 and not 055 because `ADR-055` is currently used *twice* —
`ADR-055-on-sale-write-rate-limiting.md` and `ADR-055-presale-unlock-codes.md`, both Accepted on
2026-08-10, from the security review and TKT-239 respectively, and both cited from code. That
collision is a registry defect this ticket did not create and did not fix.

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
label, created_at, revoked_at)`, with a partial unique index giving one *live* credential per
`(organizer, channel, reseller)` so that rotation is enrol-then-revoke.

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

The confirm path goes further and resolves the reservation with a **SQL predicate binding all four
terms at once** (`id`, `organizer_id`, `channel_code`, `reseller_id`). A load-then-compare in Go
would be equivalent only if nobody ever forgot a term; the predicate cannot be partially applied. A
reservation that is not the partner's answers **404**, not 403 — "forbidden" would confirm it exists.

### 4. The surface is a contract operation, so the guard is declared and validator-enforced

ADR-043's line, applied without exception: these operations declare
`security: [{PartnerCredential: []}]` and the OpenAPI validator enforces it **before routing**. Not a
header comparison in a handler.

The mechanical reason, not a stylistic one: a handler check is a thing the *next* partner-shaped
operation can be added without. A declaration is checked by the validator for whatever the document
says, and `TestEveryPartnerOperationDeclaresTheCredential` derives the requirement from the document
rather than from a list someone must remember to update.

Unlike `/internal/`, these paths **are** reachable from the edge. That is the point of the class.
They live under `/api/commerce/` and the gateway's existing `internal/` denial is unaffected; no new
route-table entry was added, because the table strips the whole matched prefix and a
`/api/commerce/partners/` entry would deliver `/reservations` to commerce instead of
`/partners/reservations` — verified by executing the stripping logic, not by reading it.

### 5. Rate limiting is per reseller

ADR-051's per-subject half is *fully available here and nowhere else on a write path*: the credential
resolves in the validator **before the body is decoded**, so a partner has a stable identity at
limiting time. The on-sale write path is source-keyed precisely because it does not.

Per-source alone would be near useless for a partner — a server with a fixed address that
legitimately sends volume. Keyed on `reseller_id` rather than `credential_id`, so rotating a leaked
credential does not hand the partner a fresh budget.

### 6. Attribution is copied from the reservation, and it is historical

`reservations.reseller_id` and `orders.(channel_code, reseller_id)`, nullable, not backfilled — NULL
means "not a partner sale", which is what every pre-existing row is. The order's values are **copied
from the reservation** in the insert, so they survive replay and recovery identically and no request
field can attribute a sale to a reseller that did not make it.

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
- Closing the commerce→inventory channel seam (same ticket) means a partner sale consumes its own
  channel's allocation. That change affects **all** channelled sales, not only partners', and is the
  ticket's intended breaking change.
- The **seated** half of that seam stays open and stays TKT-176's. A seated claim carries no channel
  at all, so the partner surface refuses a seated pool (`seated_pool_unsupported`) rather than
  half-fixing it. A test pins the seated hold body as channel-free; if it fails, TKT-176 was closed
  by accident.
