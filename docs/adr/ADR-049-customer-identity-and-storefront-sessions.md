# ADR-049: Customer identity lives in commerce; the storefront holds the session

Date: 2026-08-06

## Status

Accepted

**Amended by TKT-221 (2026-08-06)** — a signed-in purchase is attributed to its account. Nothing
above is reversed; the amendment is additive and lives in § *TKT-221 amendment* at the end.

## Context

TKT-21 needs a *customer*: an optional account a buyer can create, so that purchases hang off an
identity instead of only an order reference they have to keep. Nothing in the system has one today.

Guest checkout already works end to end — `buyer_pii` and `orders.guest_order_ref` in commerce, and
a retrieval page at `web/storefront/src/pages/[locale]/tickets/[orderRef].astro`. **That path is not
being replaced.** Guest-first is the default and this ADR must not weaken it.

Four things constrain where customer identity can go.

**ADR-002 cuts the system at five services plus a gateway.** `catalog` owns venues, events, rule
definitions and *organizers/tenants*; `commerce` owns cart, pricing, **orders** and post-purchase.
The gateway owns no database and its `go.mod` has no requires at all.

**ADR-042 already answered this question for *staff*, and answered it differently.** Staff accounts
went to catalog **because catalog owns organizers and staff administer an organizer**. That is a
domain argument, not a "catalog has bcrypt code" argument, and it does not transplant: a customer
administers nothing. ADR-042 also already rejected the two escape hatches — a sixth `identity`
service (a direct ADR-002 deviation costing a module, image, Compose service, migrate job, smoke
override, `go.work` entry and gateway route before any domain logic) and a gateway-owned store
(domain state at the edge is cheap to add and expensive to remove).

**`buyer_pii` is not an identity.** It is keyed by `buyer_id`, which is minted per reservation, and
it is written with `ON CONFLICT (buyer_id) DO UPDATE`
(`services/commerce/internal/api/server.go:1168`). It is an order-time snapshot that gets
overwritten — making it the account would make the password follow the most recent checkout.

**This repo has no mail path.** No SMTP, no queue, no provider. Every design that depends on sending
mail — email verification, password reset, "we sent the owner a note" on a duplicate registration —
is unavailable, not merely unbuilt.

## Possible Solutions

- **Option 1 — a sixth `identity` service.**
    - Pros: the cleanest boundary, and the natural home if authentication grows to cover staff and
      customers together.
    - Cons: the ADR-002 deviation ADR-042 already priced and rejected, now for a slice that has one
      table and two operations. Hardest option to walk back.
- **Option 2 — the gateway owns customer accounts.**
    - Pros: one enforcement point ahead of every service.
    - Cons: gives a database, migrations and a KDF to the component deliberately built as a
      stateless route table. Rejected once already, for reasons that have not changed.
- **Option 3 — catalog owns customer accounts, beside `staff_accounts`.**
    - Pros: the bcrypt machinery, the provisioning CLI and the contract wiring already exist there;
      one identity table shape, one place to look.
    - Cons: it is the "the code is already there" argument wearing a domain argument's clothes.
      Catalog owns organizers and their configuration; a customer belongs to no organizer. It would
      also put the account on the far side of a service boundary from the orders that TKT-221–223
      must join it to, making the core operation of this epic a cross-service call.
- **Option 4 — commerce owns customer accounts; the storefront owns the session (chosen).**
    - Pros: no ADR-002 deviation — commerce already owns orders and buyer PII, which is exactly what
      the epic's next three slices attach to an account. One migration in an existing service, two
      operations in an existing contract, no new deploy unit, and `commerce-migrate` already exists
      (ADR-022). Enforcement sits in the app that serves the pages. The gateway stays stateless.
    - Cons: commerce now owns password verification alongside money movement. Sessions are
      process-local (below). Extracting a shared identity service later means moving account data
      *and* contract ownership.

## Decision

**Customer accounts live in `commerce`** (`customer_accounts`, migration `0015`), and **the
storefront owns the session** and enforces it in Astro middleware.

Six sub-decisions follow, each of which is the part most likely to be misread later.

### 1. `registerCustomer` and `authenticateCustomer` are PUBLIC contract operations

They carry no credential. ADR-043's rule is what places them: declared operations in a service's
public contract are its public surface; an inline `X-Internal-Token` check guards `/internal/`.

Note this is *not* the same call as TKT-191 made for staff. That ticket moved
`POST /staff/authenticate` behind a dedicated catalog credential because the back office is a staff
tool with a known, provisioned user set, and the credential could be scoped to catalog alone.
Customer registration is open to the public by definition — there is no provisioned set to check
against — and the process rendering the form is the storefront, which today holds **exactly one**
environment variable (`GATEWAY_URL`) and no service token at all. Giving it one to reach an
`/internal/` endpoint would place a credential inside a process serving anonymous internet traffic,
which is the trade ADR-042 refused for the shared token and the reasoning holds here.

### 2. A duplicate registration answers 409, and that is an account-existence oracle

`POST /customers` with an already-registered address returns 409 and says so plainly.

This is **an unauthenticated, unrate-limited membership oracle over the entire customer base**. Said
in those words because the softer phrasing ("it discloses whether the address is registered")
undersells it: anyone can walk a list of addresses and learn which of them buy tickets here.

It is accepted because every alternative available in this repo is worse. The standard mitigation —
answer 201 and mail the owner — needs mail, which does not exist. Answering 201 without mailing
anyone means telling a legitimate buyer their account was created when it was not, and they discover
it at their next sign-in with no way to recover. A generic "invalid request" is still a
distinguishable answer *and* useless to the person who typed the address.

The control that actually addresses submission volume is **rate limiting: TKT-224**, covering both
operations and the storefront forms. It is not addressed here. TKT-195 is the equivalent still open
for the back office.

### 3. An unknown address and a wrong password are indistinguishable — in the answer *and* in the cost

One error (`ErrCustomerCredentialsInvalid`), one refusal body built from one constant, one status.
Two call sites constructing "the same" message are two call sites one of which eventually says
something slightly different, and the difference is what an enumerator reads.

Equal bytes are not enough. The unknown-address path performs **one bcrypt comparison against a
fixed dummy hash pinned to the production cost** before refusing, because a byte-identical response
that arrives an order of magnitude sooner is still an oracle. The dummy hash is a literal generated
out of band, never produced by the hashing function under test: a fixture derived from the code it
tests would track a regression in the cost constant instead of failing on it.

**The claim is "masked", not "identical", and the distinction is the honest one.** An adversarial
review pass correctly pointed out that the two paths are not the same work: a known address takes a
successful `QueryRow`/`Scan`, an unknown one takes `sql.ErrNoRows`. What the design asserts is that
the *remaining* difference — one indexed single-row lookup, hit versus miss — sits underneath a
bcrypt comparison two to three orders of magnitude larger, and that the tests prove the KDF is
always paid rather than proving the two paths are cycle-identical. Removing the residue entirely
would mean the database returning a dummy row on a miss, which pushes the same branch into SQL and
buys very little. Naming the adversary (ADR-021): this defends against someone timing responses
across a network. It does not defend against someone who can measure the database directly, and it
is not claimed to.

Two consequences that look like details and are not:

- An over-long password is refused **before the lookup**. Past 72 bytes bcrypt returns
  `ErrPasswordTooLong` *without doing the work*, which strips away the only thing masking the cost
  difference between a row that was found and `sql.ErrNoRows`. The contract's `maxLength: 72` cannot
  catch it: OpenAPI counts characters and bcrypt counts bytes, so 72 multibyte characters clear the
  schema and are three times over the limit.
- The sign-in schema's `minLength` on the password is **1, not 8**. Raising the floor there would
  refuse a short password before the credential check and answer differently from a wrong one.
  Registration is where the policy lives.

### 4. Sessions are in-process, unpersisted, absolutely expiring, and capped per customer

A storefront restart signs everyone out and a second replica would not share sessions. Both are
true, both are acceptable for a single-replica Compose stack, and both leave no schema to migrate
away from when they stop being.

The **per-customer cap of five** is what bounds the session map. Sweeping expired entries is not a
bound: every sign-in mints a new token while the old ones live out their full TTL, so one valid
credential could inflate the map without limit — and the sweep degrades with it. ADR-042 records
that a review pass caught exactly that claim in the back office. The cap is per principal and never
global: a global one would let a busy account evict a stranger's live session.

The TTL is **absolute, not idle** — an idle window renews on every request, so a stolen cookie stays
good as long as the thief keeps using it. Eight hours is **inherited from the back office without a
customer-specific argument**; there it was "one working shift", which is a staff argument. Recorded
as inherited rather than justified, because it is one constant and a real complaint should move it.

### 5. The session cookie is `Path=/`, and that is a deliberate departure from ADR-042

ADR-042 scoped `bo_sid` to `/admin` because the gateway serves the storefront, the scanner, `/admin/`
and `/api/*` on **one origin**, so a `Path=/` cookie is attached to all of them.

That reasoning is right and its conclusion still does not fit here. The storefront **is** the
gateway's catch-all (`/`), and its pages are locale-first. An account-subtree cookie is not sent on
the pages that need it: the checkout flow lives on `/[locale]/events/[eventId]` (TKT-221) and the
guest-order claim form on `/[locale]/tickets/[orderRef]` (TKT-223). A per-locale path would drop the
session on every language switch. Scoping it narrowly would make two of this epic's four slices
unimplementable and buy a rewrite.

**What it costs, unsoftened:** the browser attaches this token to same-origin requests to `/api/*`
— including the seat picker's and hold picker's client-side fetches — and to `/admin/` and
`/scanner/` if a customer ever visits them. That is wider travel than `bo_sid` has.

**What makes it acceptable today:** no service or gateway log records it.
`shared/go/obs/requestlog.go` logs `method`, `path`, `status` and `duration_ms` and nothing else,
and every Go service and the gateway use it. TKT-202 — the open bug where the gateway "logs
`guest_order_ref`" — is a *path* disclosure, which confirms the shape: paths are logged, cookies are
not.

That is a property of **today's logging**, not a guarantee. `docs/development.md` carries the
standing constraint that request logging must never log the `Cookie` header, and this paragraph is
the thing that stops being true if it ever does.

Naming the adversary (ADR-021): the cookie's controls bind a **browser**. They say nothing about a
caller already inside the Compose network, and nothing at all about anyone who can read or write the
commerce database — that adversary can mint, read or delete accounts directly.

### 6. Astro's `security.checkOrigin` is off; a proxy-aware origin check replaces it

`checkOrigin` defaults to **`true`** in the installed Astro 7, and `web/storefront/astro.config.mjs`
had never set it — because until this ticket the storefront had **no form at all**, so nothing had
ever exercised it. It could not be left on: the Node adapter builds its request URL from the
container's own `Host`, which behind the gateway is a service name, so the comparison never matches
and it 403s every POST. That is TKT-105's failure, sitting latent in a second app.

What replaces it is `web/storefront/src/lib/gate.ts`, which compares `Origin` against the **public**
origin the gateway reports via `X-Forwarded-Proto`/`X-Forwarded-Host` — headers Go's
`SetXForwarded()` overwrites from the real inbound request, so a client cannot forge them *through
the gateway*. A missing `Origin` is refused: every current browser sends it on POST, and the
storefront has no non-browser writers.

The check runs on **every** unsafe method, including register, sign-in and sign-out. Exempting
sign-in leaves login-CSRF open; exempting sign-out lets any site sign a buyer out.

The claim the gate makes is an **ordering** — "refused before any credential is read" — which is why
the composition lives in `gate.ts` rather than in `middleware.ts`. Nothing observable from outside
distinguishes it from a gate that ran the handler, let it mint a session, and then replaced the
response: the bytes are identical and an orphaned server-side session survives. The tests instrument
`lookup` and `next` and assert a refused request reaches neither.

## Consequences

- Commerce owns password verification. A service that moves money now also holds credentials; that
  is the price of keeping identity next to orders, and it is not free.
- **The guest path is unchanged.** No route redirects to sign-in, checkout needs no account, and
  the storefront's public pages never consult the session — which is also why the site header shows
  a plain "Account" link and never "Signed in as …". Reading the cookie on catalog pages would make
  that HTML per-customer, which ADR-004's minutes tier forbids.
- Account HTML is `no-store` and account paths are outside `page-tier.ts`'s cached set.
- A storefront restart signs every customer out. There is no way to invalidate a session from
  another process, and no cross-replica sharing.
- Registration discloses account existence (§2) until TKT-224 lands.
- Accounts are globally keyed by normalized email. There is no organizer selector, and adding one
  later is a data migration plus a contract change.
- No password reset, no email verification, no magic links, no OAuth. A customer who forgets their
  password has **no recovery path at all** — their orders remain reachable by order reference, which
  is why guest retrieval staying first-class matters more than it looks.
- `email_key` is `UNIQUE` and registration relies on the constraint rather than a pre-check: a
  SELECT-then-INSERT is a race two concurrent registrations both win.
- Rolling migration `0015` back with any account present **fails**. Credentials cannot be recovered
  and a silent drop would lock every customer out with no record of who existed.

---

## TKT-221 amendment — attributing a purchase to an account

### The problem this had to solve

This ADR put customer **accounts** in commerce and the **session** in the storefront process, and
they never speak about a specific signed-in buyer: the storefront authenticates once, gets a
principal, and remembers it locally. Commerce has no idea that session exists.

Two facts make the obvious answers unavailable. The checkout is a `fetch` from a **browser-side
React island** (`HoldPicker`, `client:load`), so the storefront's SSR process is not in the request
path at all; and `sf_sid` is `httpOnly`, so the island cannot read it. Meanwhile:

- **Putting `customer_id` in the checkout body is forgery.** `/api/commerce/orders` is public through
  the gateway; anyone could post any UUID and attribute their order to a stranger, or a stranger's to
  themselves.
- **Giving the storefront a service credential is the trust boundary §1 refused.** That process
  serves anonymous internet traffic and holds exactly one environment variable.

### Decision

**Commerce mints a signed, expiring customer assertion; the storefront proxies the checkout and
attaches it server-side.**

1. **The assertion** is `v1.<customer id>.<unix expiry>.<HMAC-SHA256>`, signed with
   `COMMERCE_CUSTOMER_ASSERTION_KEY` — a **fourth** credential, commerce-only. Commerce verifies its
   own signature; nothing is stored. It is returned from `registerCustomer` and
   `authenticateCustomer`, so the buyer earns it by presenting their password.

   It is a **bearer credential in a public contract response**, said plainly because that is what it
   is: anyone holding it can attribute a checkout to that customer until it expires. What bounds the
   damage is where it lives, not what it is.

2. **Its lifetime is exactly the storefront session's** (8h), anchored at the same instant. This is
   deliberate and it is the part most likely to be "tightened" later by someone who has not read
   this paragraph. A shorter assertion does not buy security — it lives only inside the in-process
   session, so it is already unreachable once that session is gone — and it buys a specific, cruel
   failure: a buyer who signs in, browses, takes a hold on a short TTL and reaches the payment button
   is refused *there*, with no way back except signing in again while the hold expires. The
   storefront cannot re-mint one; it holds the principal, not the password. Coupling the two makes
   "the session is alive" and "the assertion is valid" one statement rather than two that can
   disagree. Two constants in two languages enforce it, plus a test on each side.

3. **The storefront proxies checkout at `/checkout`.** Not under `/api/` — in this system
   `/api/<svc>/` means "the gateway proxies this to a service", and a storefront route inside that
   namespace reads as a service call while depending on the `/` catch-all. The bridge forwards an
   allowlist of headers, the body verbatim, and commerce's status and response **bytes** unchanged;
   it never forwards the cookie.

4. **Absent assertion means GUEST; invalid means 401.** Absent is a first-class answer — buying
   without an account is the default. Invalid is **not** silently downgraded to a guest order: that
   would hide a failed attribution from the buyer, who would then not find the purchase in their
   account with no error to point at. But an *expired storefront session* at the bridge **is** a
   guest checkout rather than a refusal: breaking a working checkout because the buyer had once been
   signed in is a worse outcome than an unattributed order they can still reach by reference.

5. **Attribution is written once, on the INSERT in `claimOrder`, and never updated.** It has to
   survive the request that established it — an order can be completed minutes later by the recovery
   runner (ADR-016), after the assertion expired. A replay finds the existing row and leaves it
   alone, which is what makes attribution immutable under idempotency: a second request with a
   different assertion cannot repoint a purchase, and cannot promote a guest order into an attributed
   one. The assertion is deliberately **not** part of the idempotency fingerprint — it carries an
   expiry, so including it would turn a legitimate retry into a 409.

6. **An exchange's replacement order inherits the source order's attribution.** An exchange is the
   same purchase in a different seat; without this a signed-in buyer's exchanged order silently
   disappears from their account.

7. **`orders.customer_id` is nullable and nothing is backfilled.** NULL means guest, for the domain
   reason and not as a migration convenience. No index: the read it exists for is TKT-222's, and
   ADR-019 says an index is justified by the scan it removes.

### The adversary, named (ADR-021)

This stops **an internet caller who can post directly to the public gateway** from attributing an
order to a customer they have not authenticated as. Tampering with either field of the token fails
the MAC; the MAC is checked before the expiry is trusted, because until the signature says otherwise
the expiry is attacker-controlled.

It stops **nothing** against: someone holding the signing key; someone who can read the storefront
process's memory; someone with commerce database access; or someone who has stolen a live assertion,
which remains usable until it expires. Commerce refuses to start when the key equals either other
credential, and all four are generated from independent `/dev/urandom` reads — but that is separation,
not protection against a holder.

### Consequences

- A fourth credential to generate, distribute and eventually rotate. **Rotation, revocation and
  multi-key verification do not exist** — changing the key invalidates every live assertion, which
  signs every customer out of checkout attribution until they sign in again.
- The checkout now traverses the storefront SSR layer, which means it passes the proxy-aware origin
  check — **for guests too**. That is a new failure mode on a previously direct path, and it is
  exactly the class `make check` cannot see, so a browser must complete a guest checkout before this
  is believed.
- `GET /orders/{id}` now reports `customer_id`, and it is **informational**: that read is public and
  answers for any order id. It is not an ownership check, and TKT-222 must not treat it as one.
