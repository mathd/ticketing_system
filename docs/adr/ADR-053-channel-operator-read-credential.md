# ADR-053: The back office reads the channel registry with its own catalog credential

Date: 2026-08-10

## Status

Accepted (TKT-236; decision taken under the owner-waived gates of that run, recorded on the ticket).
Second deliverable of the TKT-17 epic. Amends nothing; **ADR-043 draws the line for where a guard
lives, and this does not move it** — the guard stays exactly where it was, on catalog's `/internal/`
prefix, and gains a second accepted credential on one route.

## Context

TKT-235 shipped the sales-channel registry. Its operator read — `GET /internal/channels`, which
returns **disabled** channels — is hand-mounted and undeclared, because catalog's contract cannot
express a staff-authenticated GET: TKT-191's derived invariant requires every *safe* operation to opt
out of the staff-write credential with `security: []`, and that credential is a *write* credential.

TKT-236 gives the back office the admin page for that registry. The page must show disabled channels
— disabling is the only retirement mechanism (there is no DELETE, because a code removed from the
registry is still recorded on live claims, fee rules and split schedules that do not reference it),
so a page that could not display or re-enable them would make retirement a one-way door.

The back office **does not hold `INTERNAL_SERVICE_TOKEN`** and is deliberately denied it:

> that one value opens every service's internal surface, and this is a public-facing SSR process
> — `compose.yaml`

That refusal is enforced by a test (`web/backoffice/test/api.test.ts`: the client must never send
`X-Internal-Token`) and is the same trap TKT-190 walked into and plan-review caught.

## Possible Solutions

- **Give the back office `INTERNAL_SERVICE_TOKEN`.**
    - Pros: no new code.
    - Cons: hands a public-facing SSR process every service's internal surface — commerce's refunds,
      inventory's operational holds. Explicitly refused in compose and pinned by a test.
- **Publish a public read that returns disabled channels.**
    - Pros: no credential question at all.
    - Cons: publishes operator configuration to the internet. Disabled channels are the organizer's
      retired configuration; nothing about them is a buyer's business.
- **Ship against the public read only** (enabled channels; disabling one-way from the UI).
    - Pros: zero wiring.
    - Cons: an operator cannot see or re-enable what they disabled — a screen that creates dead
      configuration.
- **Accept the catalog staff-write credential on that one route** (chosen).
    - Pros: no new credential, no new secret to generate or rotate, and the smallest reversal —
      one guard branch and one environment variable.
    - Cons: widens what one credential opens; see § What this costs, which is the part that took two
      review passes to state correctly.

## Decision

`X-Catalog-Staff-Write-Token` is accepted on **`GET /internal/channels`** and nothing else. The back
office reaches it **directly, in-network** (`CATALOG_URL`), because the gateway edge-denies every
`/api/<svc>/internal/` route by construction (ADR-002) — the same shape as the staff refund reaching
commerce (TKT-194).

Specifics:

- **Method+path exact, not a prefix.** `/internal/channels/{id}` sits one character away and stays
  shared-token-only: the page does not need it, and an allowance is only narrow if something refuses
  what is next to it. A mutation check found that widening this to a prefix killed **no test** —
  the hand-mounted handlers carry their own credential check, so the guard could stop being narrow
  while every route-level test stayed green. The allowance is therefore asserted against the
  predicate directly.
- **Additional, not replacing.** `X-Internal-Token` still works on that route; access and other
  services use catalog's internal surface.
- **Fail-closed when unconfigured**, and constant-time compared, like `authenticateStaffWrite`.

## What this costs

**This section was wrong twice before it was right, and both errors are recorded because the
correction is the useful part.**

**First claim — "the added blast radius is nil."** False. The argument was that a credential which
can already `createChannel` and `updateChannel` can already learn which channels exist. It cannot:
a create against a guessed code returns 409 if taken — one code per request, and it never yields an
id. The list returns **every channel for a caller-supplied organizer in one call**: ids, codes,
kinds, and disabled rows that appear nowhere public.

**Second claim — "the organizer predicate breaks the enumeration→mutation chain."** Also false.
TKT-236 added `AND organizer_id = $2` to `UpdateChannel`, which is a real fix for a real
cross-tenant write bug. But **both** the list's `organizer_id` and the update's are *caller-supplied*,
so a stolen credential can list a victim's channels and then update the ids it just learned, naming
the victim in both calls. **The sequence was executed against the code**: list returns 200 with the
victim's channels, update returns 200 and mutates them. It is pinned by
`TestStaffCredentialCanStillEnumerateAndMutateAcrossTenants`.

**What is actually true:**

- This read **adds bulk enumeration** of any organizer's channel configuration to a credential that
  previously had to probe one guessed code at a time.
- It does **not add cross-tenant write capability** — the same credential already had it, because
  `createChannel` and `updateChannel` take the organizer from the request body and catalog cannot
  check it. Enumeration makes that existing capability far easier to aim, which is amplification
  rather than a new power.
- The organizer predicate **does** close the back-office **form** path, where the page supplies its
  session's organizer against a form-supplied channel id. That was a genuine bug and is genuinely
  fixed.
- Everything else rests on one assumption, which is the actual security property:
  **the back office is not compromised.**

**Name the adversary (ADR-021).** Catalog authenticates the calling **process**, not the staff member
behind it. Against an honest back office, tenancy holds because the page passes its session's
organizer and never one from the request. Against a compromised one, it does not hold and no
predicate in catalog can make it — that needs an organizer identity catalog can verify independently
of the request body, which is **TKT-245**.

## Consequences

- **Positive:**
    - The registry becomes operable by a human without a new secret, and retirement stops being a
      one-way door.
    - One cross-tenant write bug closed for the form path, with the SQL predicate asserted at the tier
      it lives in — an earlier version of that test passed against the in-memory fake while the
      predicate was deleted.
    - The gap that remains is written down, pinned by a test, and owned by a ticket rather than
      implied to be absent.
- **Negative:**
    - One credential opens one more thing, and the trust assumption behind it is now explicit rather
      than comfortable. TKT-245 is the ticket that would let this ADR say something stronger.
    - `/internal/channels` has no generated client types and no ADR-028 response validation, being
      hand-mounted — the cost catalog's whole internal surface already pays (`api/cache_control.go`).
- **Revisit when:** TKT-245 lands, or a second consumer needs an operator read — two callers on a
  route-specific allowlist is the point at which the allowlist should become a mechanism.
