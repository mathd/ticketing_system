# ADR-051: The public customer surface is rate limited in commerce, per subject and per source

Date: 2026-08-07

## Status

Accepted

Discharges the control ADR-049 §2 named and ADR-050 § Consequences widened. Amends neither:
the 409 membership oracle stays exactly what ADR-049 says it is, and this ADR explains why.
TKT-195 (the back-office equivalent) is still open and inherits the mechanism, not the wiring.

## Context

ADR-049 §2, on `POST /api/commerce/customers`:

> This is **an unauthenticated, unrate-limited membership oracle over the entire customer base**.
> […] The control that actually addresses submission volume is **rate limiting: TKT-224**.

TKT-224 was written against **two** operations. By the time it ran there were **five**, and
ADR-050 had already said so:

> Rate limiting is still needed for credential grinding and for reset-mail volume, which this
> ticket *adds* as an abuse surface: an unauthenticated caller can now make commerce enqueue a
> message per request.

What unbounded volume actually buys, per operation:

| Operation | Cost of a request | What volume buys |
|---|---|---|
| `POST /customers` | bcrypt | Walking the customer base through the 409 |
| `POST /customers/authenticate` | bcrypt on **both** paths (ADR-049 §3) | Credential grinding; CPU exhaustion |
| `POST /customers/password-reset` | **an enqueued message** | Mail-bombing one address |
| `POST /customers/password-reset/complete` | bcrypt | Token grinding (see §4) |
| `POST /orders/claim` | a lookup | Guessing order references (ADR-049 § TKT-223) |

## Decision

### 1. The limiter lives in commerce, and the mechanism is shared

Not the storefront: `POST /api/commerce/customers` is public through the gateway, so a caller
skips the form entirely and a storefront-side limiter protects nothing.

Not the gateway: it owns no database and its `go.mod` has no requires at all (ADR-002).
Counters there are a deviation to argue, not a default.

The mechanism is `shared/go/ratelimit`, because **TKT-195 needs the same thing in a different
process** — its surface is `/admin/login` in *catalog*. One package, two wirings; the policy,
the key derivation and the oracle-safety property are tested once. Two independently grown
limiters was the outcome the ticket's readiness note called the bad one.

### 2. Counters are in-process, and this is a real limitation, not a detail

Per-replica, and empty after a restart.

Postgres was rejected on a specific ground: every **refused** request would cost a write, which
hands an attacker write amplification against the money-path database. The limiter would become
the vector it was added to remove. The repo already took the same trade for storefront sessions
(ADR-049 §4).

**The adversary, named (ADR-021).** This bounds *a single scripted client against one replica*.
It does not bound a distributed caller, it does not bound anyone willing to wait, and it bounds
nothing after a restart. It **slows** enumeration; it does not close it. Anyone writing "rate
limited" in a doc, a ticket or an AC has to mean that sentence and no larger one.

### 3. Two keys, because neither is sufficient

- **Per subject** (`customerSubjectBurst` per `customerSubjectWindow`) — the address for the three
  address-keyed operations, the caller's customer id for `claim`. Protects one account.
  *Residual:* nothing against a walk across many subjects.
- **Per source** (`customerSourceBurst` per `customerSourceWindow`) — the client IP. *Residual:*
  trivially evaded by rotation, and a shared NAT egress carries many genuine buyers.

**The per-source key is worth much less than it looks, and the reason is structural.** The
storefront calls commerce *server-side* through the gateway, and the gateway replaces
`X-Forwarded-For` with its own peer — which on that hop is the **storefront container**. So every
buyer who uses the forms shares one source key, and this budget is a cap on the site's aggregate
form traffic rather than on any one client's.

It cannot be repaired by having the storefront forward the buyer's address. commerce would then be
trusting a header that any caller reaching the gateway can set — a total bypass — and the
storefront deliberately holds **no credential** that would let commerce tell its claim from an
attacker's (ADR-043, ADR-049 §1). That posture is worth more than this key.

So the honest division of labour is:

- a caller scripting the **gateway directly** gets their own source key, and this budget bounds them;
- a caller working through the **forms** is bounded by the **per-subject** limiter, not this one.

The budget is therefore sized for aggregate traffic, and
`TestTheSourceBudgetIsSizedForTheWholeSiteNotOnePerson` fails if anyone tightens it toward a
per-person figure — a change that looks obviously right in isolation and would throttle every
buyer at once during an on-sale.

**The key is what stays fixed across an attempt sequence, not what is being attempted.** Keying
`claim` on the order reference, or reset-completion on the token, would hand every guess a fresh
budget — no bound at all — while filling the key map. That is why `claim` keys on the caller and
why reset-completion has no subject key (§4).

**Credential probing and account recovery are separately scoped**, and this is the one decision
here that was wrong first and corrected by evidence. One bucket per address across both looked
obviously right — same subject, and separate budgets let a prober probe twice as much. It is a
lockout. The buyer who mistypes their password until the budget is gone is precisely the buyer who
then clicks "forgot password", and a shared bucket refuses them the only path back in.
`test/browser/rate-limit.mjs` caught it on its first green run: every reset was throttled and
nothing was ever enqueued. The defensive cost is a constant factor on a walk that is already
hopeless; the availability cost of getting it wrong falls on the one user who most needs the
feature. `TestSpendingTheSignInBudgetDoesNotRefusePasswordRecovery` pins it, and
`TestTheRecoveryBudgetStillLimitsOnItsOwn` pins that the split did not quietly remove the
mail-bombing bound ADR-050 asked for.

Both maps are **capped**, for the reason the storefront's session map is (ADR-049 §4): they are
fed by unauthenticated input. At the cap an already-tracked key still works and only a *new* key
is refused. The other direction — evicting to make room — would let an attacker rotate keys to
flush the bucket holding them back, which is a bypass. Reaching a cap is a symptom to escalate.

### 4. Reset-completion has no subject key, deliberately

The only candidate is the submitted token, and keying on it is worse than nothing twice over: a
grinder varies it, so the budget never binds, and each guess inserts a map entry. The per-source
limiter is the real bound here. Grinding the token is not what the limiter answers — it is 32
random bytes (ADR-050 §4), so the search space is.

### 5. The refusal is uniform by construction, not by discipline

`429`, one `Retry-After`, one body constant shared by all five operations — and the check runs
**before the store call**.

**429 is declared in the OpenAPI document for exactly the five limited operations.** Commerce
validates its own responses against the contract (ADR-009), and an undeclared status is rewritten
to **500**. So the first working build had a limiter that fired correctly and a buyer who saw an
outage — the storefront mapped the 500 to `unavailable` and nothing looked wrong from the outside.
No Go handler test could see it: those call the handler directly and never cross the contract
middleware. `make browser` is what caught it, which is the case AGENTS.md's browser-submit rule
exists for.

That ordering is the whole design. A bucket that only filled for addresses that *exist* would
make a 429 mean "this address is real" — **a sharper oracle than the 409 it was added to blunt**.
Refusing before any lookup makes the answers identical by construction rather than by remembering
it at five call sites. `TestARateLimitedKnownAddressIsIndistinguishableFromAnUnknownOne` compares
whole responses, and a mutation that moves the check after the store call fails it.

`claim` is the one exception to "before the assertion check", and it is safe: its refusal is
about *who is asking*, not about whether the order exists.

### 6. The client IP is trusted only because of how it arrives

The per-source key reads `X-Forwarded-For`. That is safe here and would not be in general: the
gateway's proxy uses httputil's **`Rewrite`** hook, which strips inbound `X-Forwarded-*` before
the hook runs; `SetXForwarded` then writes the connecting peer. A forged chain is **discarded**,
not appended to. `TestAForgedXForwardedForDoesNotReachTheUpstream` asserts that against the real
proxy rather than trusting the documentation — the older `Director` hook *appends*, and the
difference between replace and append is the difference between a limiter and a bypass.

commerce takes the **last** element regardless, so a future ingress that appends degrades to
correct instead of forgeable. Taking the first is the classic bypass.

**Residual, stated:** this is only as good as the gateway being the sole ingress. Commerce's port
is published in the Compose profiles; anyone who reaches it directly sets the header freely. That
is a deployment property, not something the code can enforce.

### 7. The 409 stays

ADR-049 §2's trade is unchanged. "Answer 201 and mail the owner" became *available* with ADR-050,
but adopting it forces registration to stop returning a principal, which breaks the
register→signed-in flow ADR-049's TKT-221 amendment attaches to. That is a product change and
belongs to its own ticket. Rate limiting slows the walk; it does not remove the disclosure, and
this ADR does not claim otherwise.

### 8. The storefront renders a 429 as a wait, never as a verdict or an outage

`throttled` is its own reason through `customer-api.ts` and its own branch on all four pages.
Folded into a credential verdict it is false — no password was checked. Folded into `unavailable`
it sends a buyer to support for something a clock fixes. On forgot-password, folded into success
it is a lie: nothing was enqueued, so the buyer waits on mail that will never arrive.

The copy is identical whatever the address, which is what stops the UI reopening the oracle §5
closes in the handler.

## Consequences

- **TKT-195 gets a package, not a design.** It wires `shared/go/ratelimit` into catalog with its
  own thresholds. Its AC-4 (a 429 must not appear only for real accounts) is the same property §5
  establishes here, and the same test shape proves it.
- **Thresholds are tunable; the shape is not.** The budgets are named constants and the tests
  assert against *them*. A test carrying its own literal would keep passing when the production
  value moved, leaving the limit untested exactly when it changed.
- **A restart is an amnesty.** Deploys empty every bucket. Acceptable at this scale and dishonest
  to leave unsaid.
- **Nothing here is a defence against a botnet.** §2 says what this is. If distributed abuse ever
  becomes the threat, the answer is upstream of this repo, and this ADR should be superseded
  rather than quietly stretched.
- **`POST /orders/claim` moved into the limited route group.** It is still a static segment under
  the same prefix as `GET /orders/{id}`, chi still prefers static over parameter, and the routing
  test that asserts so still covers it.
