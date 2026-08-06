# A cap inherited across a trust boundary is not a bound

**TKT-220 (PR #178) — 2026-08-06**

## What happened

The storefront's customer session module was written by mirroring the back office's staff session
module (`web/backoffice/src/lib/session.ts`, ADR-042). That was the right thing to copy: its design
had been argued in an ADR and hardened by three adversarial review passes.

Copied with it was the per-principal session cap, and the comment explaining what the cap is *for*:

> This cap is what actually BOUNDS the session map. […] With it, `sessions.size` is at most
> `staff headcount × MAX_SESSIONS_PER_STAFF`.

That is true in the back office. Staff accounts are provisioned by an operator with a CLI, so the
number of principals is bounded by a human being, and a per-principal cap therefore bounds the map.

**Customer registration is public and unauthenticated.** One actor can mint unlimited distinct
accounts, therefore unlimited distinct principals, each entitled to five sessions that live out a
full eight-hour TTL. The map had no bound at all — through a cap whose stated purpose was to bound
it.

Every individual fact in the copied comment was correct. The *premise* — a bounded principal count —
was deleted in transit and nothing said so.

Caught by the first adversarial review pass, as `[high]`. It had survived planning, plan-review and
implementation, all of which read the comment and agreed with it.

## Why it is easy to miss

A cap is a number with a unit. `MAX_SESSIONS_PER_CUSTOMER = 5` looks like a complete statement, and
the arithmetic that turns it into a bound (`principals × 5`) is the part that lives in prose. Prose
travels with the code when it is copied; the *reason the prose was true* does not, because it lives
somewhere else entirely — in this case in a provisioning CLI and an ADR about a different service.

The specific trap: **the thing that changed is not visible at the copy site.** Nothing in
`session.ts` mentions how principals come into existence. You have to go and ask.

This is [ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md)'s rule — *name the adversary
before claiming a property* — applied to a resource bound rather than a security claim. "The map is
bounded" is a claim, and it needs the same question: bounded **against whom, creating principals
how?**

## What to do instead

- When you copy a control, copy the question, not the conclusion: **what makes this number a
  bound?** Then check that thing still holds on the new side.
- Say the premise out loud in the new comment. The fixed version names it: *"the per-principal cap
  bounds the map only if the number of principals is bounded, and here it is not: registration is
  public."*
- Be most suspicious when the source is **good** code. A control that has been reviewed three times
  reads as settled, which is exactly what stops the next reader re-deriving it.
- The signal is a boundary crossing: staff → customer, internal → public, provisioned → self-service.
  Any of those under a copied control deserves the derivation re-run from scratch.

## The follow-on, which is its own lesson

The first fix was a global cap that **evicted** oldest-first. Review pass 2 found that this turned
memory exhaustion into an availability attack: an attacker fills the map, and every subsequent
sign-in silently displaces a real customer's live session — possibly mid-purchase. The replacement
**refuses** at capacity instead, disturbing nobody who is already signed in.

Both behaviours are bad under a flood. The choice is only ever *which failure to prefer*, because the
actual cause — unauthenticated unlimited registration — is not something a cap can fix. See
[the fixes-compose learning](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md).
