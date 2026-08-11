# Security review 2026-08-10 — the six decided findings

Companion to [the review](2026-08-10-security.md). These six needed a decision
before they could be fixed. **All six are now decided and shipped**; this file is
kept as the record of what was decided and why, because in three of them the
review's proposed fix does not work as written and the reason is worth not
rediscovering.

Every claim below was checked against the code first. None of them is
speculative.

| | Decision | Where it landed |
|---|---|---|
| S1 | Per-device enrolment, revocable per device | `access enrol-scanner`, migration 0009, `X-Scanner-Token` |
| S2 | Bearer link, time-bounded QR images | `ACCESS_TICKET_LINK_KEY`, 10-minute signed image URLs |
| S7 | Build the bounded version, name what it is not | commerce checkout limiter, [ADR-055](../adr/ADR-055-on-sale-write-rate-limiting.md) |
| S8 | Split payments' credential off the shared one | `PAYMENTS_INTERNAL_TOKEN` |
| S11 | Generated DB passwords; publish only the gateway | `scripts/env-bootstrap.sh`, `compose.direct-ports.yaml` |
| S13 | Pin the assumption at the journal boundary | `store.SupportedCurrencies`, positive-amount rule |

## S1 (critical) — scanner admission writes are unauthenticated

**Confirmed.** `POST /api/access/scans` and `/api/access/scans/reconciliations`
are registered with no credential check at all
(`services/access/internal/api/server.go:48-49`); neither handler reads a header
before deciding an admission. They are `security:`-free in the contract, so the
gateway proxies them from the public edge.

S5 is fixed, which changes the severity but not the finding: minting a QR that
passes `ed25519.Verify` now needs the deployment's generated seed rather than the
one in this repository. Replay of a captured QR still burns a real ticket, and
`reconciliations` still rewrites scan history in bulk, both with no credential.

**Decided: device enrolment (option 1 below). Shipped.** The review proposes an
`X-Scanner-Token` shared by every gate. That does not work as written, and the
reason is worth stating before anyone "simplifies" it back: the scanner
is a **static SPA** (`web/scanner/`, served by nginx from `dist/`), so any token
it holds ships inside a public JavaScript bundle to every phone that loads
`/scanner/`. That is a credential in the same sense a doormat key is.

Real options, in ascending cost:

1. **Device enrolment.** An operator enrols a gate once; the device keeps its own
   token in browser storage and sends it on every scan. Revocable per device,
   which is what actually matters when a phone is lost. Needs a device table and a
   way for an operator to mint and revoke.
2. **An authenticated SSR bridge.** Give the scanner a server side (as the
   back office has), hold a service credential there, and move `/scans` behind
   `/internal/`. Closes the edge completely; costs the scanner its
   static-hosting simplicity and its offline-first posture is now split across
   two tiers.
3. **Staff session.** Reuse the back-office staff login for gate operators. Fewest
   new concepts, but it puts a password prompt in front of a turnstile at doors
   open, which is a real operational cost the owner is better placed to price
   than the reviewer.

**What shipped.** Option 1. `scanner_devices` (migration 0009) holds one hashed
token per device; `access enrol-scanner` / `list-scanners` / `revoke-scanner` are
the operator surface; the contract declares a `ScannerDeviceToken` scheme that the
request validator enforces before a body is decoded; and the scanner shows a
pairing screen until it holds a token. A `401` is its own answer, never the
ticket-rejection shape — the person at the turnstile has a good ticket.

Not shipped, and the next increment: an HTTP enrolment endpoint and a back-office
screen. That wants the staff-credential question in §S1 option 3 answered first —
access has no staff identity of its own, and handing the internet-facing back
office the shared token is what TKT-191 deliberately refused.

## S2 (critical) — ticket bundles and QR PNGs on a bare link-shared UUID

**Confirmed.** `GET /orders/{ref}/tickets` and `…/{ticket}/qr.png` check nothing
beyond parsing the UUID (`server.go:95-147`). The ref has 122 bits of entropy, so
this is not enumerable — the exposure is that the system **treats the ref as
shareable**: it is returned from checkout, from wallet reads and from claim
redirects, and it travels over plain HTTP today. Anyone who ends up holding the
URL holds the tickets, indefinitely, with no expiry and no per-view check.

**Decided: bearer link, time-bounded (option 1 below). Shipped.** Which of two
models the bundle belongs to, and they are not the same product:

1. **Bearer link.** The ref stays the credential, but it stops being permanent:
   short-lived signed URLs for the QR images, and stop returning the raw ref from
   public responses. Keeps "forward the email to your friend" working, which for
   a real ticketing product is a feature, not an oversight.
2. **Account-bound.** Require the HMAC customer assertion the wallet already uses
   (`services/commerce/internal/api/assertion.go`). Strictly stronger; breaks
   guest checkout's delivery story unless guests get an assertion too.

**What shipped.** Option 1. The bundle mints a fresh, HMAC-signed, 10-minute URL
per ticket on every load (`ACCESS_TICKET_LINK_KEY`, distinct from the QR signing
seed); an expired or forged link answers `404`, identical to not-found, so a dead
link says nothing about whether the ticket behind it is real.

The residual, stated: this bounds someone who obtains an IMAGE URL — from a log, a
referrer, a screenshot. It does not bound someone who obtains the order ref, who
holds the bundle and can mint their own links exactly as the buyer does. Closing
that is account-bound delivery, and TKT-222's authenticated read is its shape.

## S7 (important) — no rate limiting on holds, reservations, checkout or scans

**Confirmed.** After the S4 fix, the platform has two limiters: commerce's
customer-credential surface and catalog's staff login. `POST /holds`,
`POST /reservations`, `POST /orders` and `POST /scans` have none. An
unauthenticated caller scripting the gateway can churn holds at line rate, each
reserving stock for `HOLD_TTL` (10 minutes) — inventory hoarding / denial of
sale, the canonical on-sale attack.

The review allowed that this may be a deliberate testbed gap. Partly: the honest
control for on-sale abuse is a queue plus per-buyer concurrency, not a token
bucket, and that is a feature this repo has not built. What must not happen is the
third option — leaving it silent and letting comments imply a control that does
not exist, which is what happened to TKT-195 and became S4.

**Decided: build the bounded version, and write the ADR for what it is not.
Shipped, with two deviations.**

`POST /reservations` and `POST /orders` now share a per-source budget, separate
from the customer-credential one so on-sale traffic cannot lock buyers out of
signing in, and declared as `429` on both operations.

The two halves that could NOT be built as written, and why —
[ADR-055](../adr/ADR-055-on-sale-write-rate-limiting.md) §2 and §3 carry the full
argument:

- **`POST /holds` gets no limiter.** Commerce is that route's dominant caller
  (`reserve` and the exchange path both POST to it), so a source-keyed bucket
  there keys every real checkout to the commerce container and starts refusing
  holds — a self-inflicted denial of sale, added by the control meant to prevent
  one. It needs per-caller internal identity first, which is S8's next increment.
- **Per-buyer hold concurrency has no buyer.** Checkout is guest-first: `reserve`
  mints `uuid.NewSHA1(NameSpaceOID, "buyer:"+reservationID)`, so every reservation
  already has a distinct buyer id and a per-buyer cap would bound every attacker
  to one reservation each — which is to say, nothing. It needs a session identity
  issued BEFORE the reserve, which is what a waiting room hands out.

## S8 (suggestion) — one shared token opens every service's money surface

**Confirmed.** All five services receive the same `INTERNAL_SERVICE_TOKEN` from
the shared `&go-env` anchor (`compose.yaml:32`), and every payments mutation —
charge, void, refund, partial refund — authorizes on it with no per-organizer
scoping. Compromise of any one service, a smoke-suite config, or a shell history
yields the whole platform's money surface.

**Decided: the first increment. Shipped.** `PAYMENTS_INTERNAL_TOKEN` is payments'
own credential; commerce is its only caller and holds both, choosing which to send
from the destination URL rather than at each of seventeen call sites. Payments
refuses to start when its token equals the shared one, and commerce's
pairwise-distinct check now covers four credentials rather than three.

What this buys, precisely: compromising catalog, inventory, access or the gateway
no longer reaches charge, void or refund. Compromising commerce still does. That
is a reduction, not a solution — per-caller credentials or mTLS is the finish, and
it is also the prerequisite for bounding `/holds` (see S7).

## S11 (suggestion) — weak committed DB credentials, broad loopback publishing

**Confirmed.** `deploy/postgres-init` creates roles whose password equals the role
name, `POSTGRES_PASSWORD: postgres` is committed, and postgres, NATS, Grafana and
all five services publish ports on `127.0.0.1`. Any process on the dev machine
gets superuser and can reach every `/internal/` surface directly — bypassing the
gateway's edge-deny and forging `X-Forwarded-For` for the limiters. The
per-database `REVOKE CONNECT … FROM PUBLIC` hardening is real and correct; the
residual is the loopback trust assumption itself.

**Decided: both halves. Shipped.**

Passwords: six generated values (`POSTGRES_PASSWORD` plus one per service role),
required with `${VAR:?}`, generated per clone by `scripts/env-bootstrap.sh` and
per run by `scripts/stack-env.sh`. `deploy/postgres-init/01-databases.sql` became
a `.sh`, because the postgres entrypoint runs shell files with the environment
available and SQL files without it.

Ports: `compose.yaml` publishes only the gateway. The rest moved to
`compose.direct-ports.yaml`, opted into by `make up` and both gate scripts — the
smoke suite genuinely needs them, and that is testbed ergonomics rather than a
security property. What changed is that a deployment built from `compose.yaml`
does not inherit the arrangement by default.

The residual, stated: with the overlay on, a process on the developer's machine
still reaches every internal surface, and the `.env` passwords (mode 600) are what
stand between it and the data.

## S13 (suggestion) — currency exponent assumption is unpinned

**Confirmed.** The charge boundary hard-codes EUR
(`services/payments/internal/api/server.go:218`) while the journal accepts any
three-letter code (`store.go:115`), and integer minor units silently assume
exponent 2. JPY is 0 and KWD is 3, so the first non-EUR currency is a
wrong-by-100x bug, not a validation error.

**Decided: stay single-currency and enforce it where it becomes permanent.
Shipped.** `store.SupportedCurrencies` is the one set, shared by the journal and
the charge boundary. It is a set rather than a bare `== "EUR"` so that adding a
currency is a decision someone states, next to the reason: every code in it has
exponent 2, and admitting JPY (0) or KWD (3) without carrying a per-currency
exponent through pricing, fees, splits and settlement is a 100x error in an
append-only ledger.

The placement matters as much as the rule. The charge boundary already hard-coded
EUR while the journal accepted any three-letter code — the assumption was checked
where it could be bypassed and unchecked where it becomes permanent.

The second suggestion shipped too: money-MOVING fact types must carry a positive
amount. Deliberately not all of them — `order.created`/`completed`/`failed` are
legitimately zero (a comp ticket is a real order that moves no money), and
`payment.declined`/`timeout` carry the amount that was attempted.
