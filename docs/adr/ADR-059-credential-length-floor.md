# ADR-059: Every credential has a length floor, and the journal's is not the same number

Date: 2026-08-16

## Status

Accepted (TKT-252; decision taken under the autonomous gates configured for this repo, recorded on
the ticket — the three shaping questions were answered by the owner at Gate 1).

Amends **ADR-032** by citation, not by contradiction: that ADR's 16-byte journal floor stands
unchanged, and this ADR exists partly to record *why* it stands while everything else moves to 32.

Read alongside **ADR-056**, which is where the number 32 comes from, and **ADR-021**, whose rule
about naming the adversary is what keeps the claim below honest.

## Context

`runtimecfg.RequiredCredential` is the single startup gate for every credential in the system —
sixteen load instances across six binaries. Until this decision it validated **transport
characters** and nothing else: emptiness, the retired checked-in literal it replaced, leading and
trailing whitespace (which HTTP strips in transit), and `httpguts.ValidHeaderFieldValue`.

It never validated **length**. A deployment configured with `x` started cleanly.

That is not a catalog problem or a commerce problem; it is one function's problem, inherited
identically by:

| Value | Consequence of a weak value |
|---|---|
| `CATALOG_ORGANIZER_ASSERTION_KEY` | forge an organizer assertion → write into any tenant (ADR-058) |
| `COMMERCE_CUSTOMER_ASSERTION_KEY` | forge a customer assertion → attribute orders to any buyer |
| `ACCESS_TICKET_LINK_KEY` | forge ticket links |
| `INTERNAL_SERVICE_TOKEN`, the three staff-write tokens, `PAYMENTS_INTERNAL_TOKEN` | bearer credentials, brute-forceable online |

The HMAC keys are the sharper half. A bearer token must be guessed *against a running service*,
which is online, observable and rate-limitable. A signing key is different: **a single captured
token is an offline oracle**, so an attacker tests candidate keys locally, at whatever rate their
hardware allows, and the service never sees an attempt.

The surfaced-by-review history matters for the scope of this ADR. TKT-245's ai-review raised this as
a `[high]` against catalog and it was **deliberately not fixed there**, on the ground that a
catalog-only floor would leave the identical hole one service over *while looking like the class had
been addressed*. That reasoning is the reason this decision covers every raw-string credential at
once rather than arriving one service at a time.

What already held, and was never enforcement: `make up`, `scripts/env-bootstrap.sh` and
`scripts/stack-env.sh` all generate 32 bytes from `/dev/urandom` (64 hex characters), and
`compose.yaml` ships `${VAR:?}` with no defaults. Every one of those is a **convention a production
deployment can decline to follow**.

## Possible Solutions

- **Option 1: Do nothing; rely on the generators.**
    - Pros: zero code change; every documented deployment path already produces 64 hex characters.
    - Cons: the gap is exactly the deployment that *didn't* use the documented path. A convention
      that is never checked is indistinguishable, at runtime, from no convention.
- **Option 2: A package-wide constant floor inside `RequiredCredential`.**
    - Pros: simplest possible change; one number, one place; no call sites touched.
    - Cons: `JOURNAL_SIGNING_KEY` also flows through this function, so a blanket 32 silently raises
      its floor and contradicts ADR-032. Making it an exception then means hiding a conditional
      *inside* the shared function, where no call site can show which policy applies to it.
- **Option 3: A separate exported function for the exception (e.g. `RequiredSigningKey`).**
    - Pros: the two policies are named and separate.
    - Cons: the ordinary floor becomes invisible — it lives inside an implementation rather than at
      the call site — and a new credential added by a future ticket inherits whichever function its
      author happened to copy. The failure is silent and looks correct.
- **Option 4: A variadic/optional override (`RequiredCredential(env, retired, opts...)`).**
    - Pros: no existing call site changes; the exception is expressible.
    - Cons: an omitted option compiles. A security policy that can be left out by *not typing
      anything* is the one shape guaranteed to be left out eventually.
- **Option 5: A required `minBytes` parameter on `RequiredCredential`.**
    - Pros: the policy is stated at every call site, where it can be read; an omitted floor is a
      **compile error**, not a silent default; the journal's divergence becomes a visible, commented
      argument rather than a hidden branch.
    - Cons: a breaking signature change touching all sixteen loads in one commit.

## Decision

We adopt **Option 5**. `RequiredCredential(envVar, retiredDefault string, minBytes int)` takes a
required floor, and `runtimecfg.CredentialMinBytes = 32` is what every ordinary credential passes.

**`JOURNAL_SIGNING_KEY` passes `journalKeyMinBytes = 16` and keeps the contract ADR-032 already
states.** Raising it was considered and **ruled out of scope** rather than left unexamined: it has
its own blast radius (the smoke journal literals and their drift guard, `JOURNAL_HISTORICAL_KEYS`,
and a documented claim in `docs/development.md` §Journal signing that rests on the key not being
required to be high-entropy). Changing a shipped decision as a side effect of a different ticket is
how a decision gets reversed without anyone arguing for the reversal. If 16 is wrong, it earns its
own ticket.

The new check is the **last** arm of the switch. Every case before it says something specific and
actionable — the value is absent, it is the retired literal, it is padded, it carries a byte HTTP
will not transmit — and those diagnostics are what tell an operator which mistake they made. Most of
the fixtures proving those messages are deliberately short, so a length check placed ahead of them
would answer *"too short"* to a value whose real problem is a stray newline.

**Why 32:** ADR-056 §2 already generates 32-byte partner tokens and states the reasoning ("this
token is 256 bits from `crypto/rand`"). The number is taken from the existing decision rather than
invented here.

### What this is not

**It is a LENGTH floor. It is not an entropy floor, and it must never be described as one.**
Thirty-two `a` characters pass it, deliberately, and a test named
`TestCredentialFloorIsLengthNotEntropy` asserts exactly that so a future change cannot quietly add a
character-class or repetition heuristic without reading this section first.

Naming the adversary, per ADR-021:

- **Closed:** an attacker guessing a *trivially short* credential — `x`, `dev`, `test` — whether
  online against a running service or offline against a captured token. The floor makes the search
  space of conformant-length values large enough that length alone is no longer the weak link.
- **NOT closed:** an attacker guessing a **low-entropy value that happens to be long enough**. A
  32-character human-chosen passphrase, a repeated string, or a value copied from a public example
  passes this check completely. Nothing in this system verifies that a credential was randomly
  generated, and this ADR does not change that.

The honest summary: this removes the *floor* failure, not the *entropy* failure. Closing the second
would require either generating credentials on the service's behalf or rejecting values that fail a
statistical test — both larger decisions, neither taken here.

## Consequences

- **Positive:**
    - A deployment can no longer boot on a one-character signing key or bearer token — across every
      credential at once, which is what makes this different from the catalog-only fix rejected in
      TKT-245.
    - The floor is readable at each of the sixteen call sites instead of being a property of a
      shared function, so "which policy governs this credential?" is answered by looking at it.
    - A future credential cannot inherit the wrong floor by omission: the parameter is required, so
      forgetting it does not compile.
    - The journal's divergence is now *stated* — a named constant with a comment and a test — where
      before it was simply the absence of any floor anywhere.
- **Negative:**
    - **A conforming-today deployment with a hand-written short credential will fail to start after
      this lands, and it will discover that at RESTART, not at deploy time.** This is the real cost
      and it is accepted deliberately: no generator in this repository produces a short value, so a
      deprecation window would protect only the hand-written case this decision exists to stop, and
      a warning that still boots is the "configured-looking and wrong" failure ADR-042's guard
      exists to prevent. The remedy is one command (`openssl rand -hex 32`), but an operator meets
      it during a restart they may not have planned.
    - A breaking signature change to a function in `shared/go`, so all five services and the gateway
      rebuild. This is a one-time cost and is what buys the compile-time guarantee.
    - Two floors now coexist in the system (32 and 16). That is a genuine wart. It is recorded here
      rather than smoothed over, because the alternative — one number achieved by quietly moving the
      journal — is worse.
    - No rotation mechanics, key overlap, or per-caller credentials are introduced. Those remain
      open and are explicitly out of scope (ADR-058 § consequences records the same for catalog).

## References

- TKT-252 — this decision. Surfaced by TKT-245's ai-review as a confirmed `[high]`, filed rather
  than fixed there.
- [ADR-032](ADR-032-stripe-behind-the-psp-port.md) — the journal keyring's 16-byte floor and the
  effective-HMAC-key reasoning behind it. Unchanged by this ADR.
- [ADR-056](ADR-056-partner-credential-identity.md) §2 — the in-repo precedent for 32 bytes.
- [ADR-058](ADR-058-catalog-organizer-assertion.md) — the organizer assertion key, which stated no
  minimum length; this closes that gap.
- [ADR-042](ADR-042-staff-identity-and-backoffice-sessions.md),
  [ADR-057](ADR-057-inventory-staff-write-credential.md) — the staff-write credentials covered here.
- [ADR-043](ADR-043-where-a-service-auth-guard-lives.md) — why the guard belongs in the shared
  package every entrypoint already calls.
- [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary before making a
  strength claim.
- `docs/development.md` §Journal signing — the documented position on journal key entropy, left
  intact because the journal floor is left intact.
