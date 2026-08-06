# A fix can be correct and still lie about itself

**TKT-221 (PR #179) — 2026-08-06**

## What happened

Two `[high]` findings in the second adversarial review pass. Neither was a defect in behaviour.
Both fixes did the right thing. What was wrong was what each one **said about itself** — in a code
comment and in a line of UI copy.

**The checkout bridge.** A transport failure returned `503` with:

```ts
// 503 and NOT a payment verdict: nothing was submitted, so a retry is safe.
return new Response(JSON.stringify({ error: 'checkout is temporarily unavailable' }), …)
```

`503` was the right status. "Nothing was submitted" was false: a timeout or a disconnect can land
*after* the gateway accepted the request and payments accepted the charge — only the response was
lost. The outcome is **unknown**, and telling a buyer that is safe to retry is telling them to pay
twice.

**The 401 handler.** A refused customer assertion had been falling through to "payment status is
being checked", which was a lie in the frightening direction — no order existed and no money had
moved. The fix added an explicit branch:

> *Your sign-in has expired. Sign in again in another tab, then press pay — your seats are still
> held.*

Better. Also not something the client can know: commerce verifies the assertion **before** it
resolves an existing order, so a retry of an already-successful checkout whose assertion has since
expired receives the same 401 — with the order completed and the seats confirmed.

## Why it is easy to miss

Neither claim is testable, and that is the whole problem. A comment is not type-checked, copy is not
asserted, and both were written *while fixing a finding*, when attention is on the mechanism rather
than on the sentence describing it. Every test passed. A reviewer reading only the logic would find
nothing.

The claims are also **load-bearing in a way the code is not**:

- The copy is what the buyer acts on. "Safe to retry" and "your seats are still held" are
  instructions.
- The comment is what the next author trusts. Someone optimizing this route in six months reads
  "nothing was submitted" and reasonably concludes the retry path needs no care.

On a money path, the claim *is* part of the product.

## What to do instead

- **Read the prose adversarially, with the same question as the code: what is the worst case in
  which this sentence is false?** "Nothing was submitted" fails whenever the failure is a lost
  response rather than a refused connection. "The seats are still held" fails whenever the refusal
  can arrive after success.
- **A claim about what did *not* happen needs the same evidence as a claim about what did.** "No
  order exists" is a statement about a remote system's state, made by a client that has been told
  only "401".
- **Prefer saying less.** The corrected 503 says the outcome is *unknown*, which is true in every
  case and needs no case analysis to stay true. The corrected copy points at the tickets page rather
  than asserting a state.
- **Write down what actually protects the user, separately from what this code does.** Here it is
  commerce, not the bridge: a retry carries a fresh idempotency key while an order already exists for
  that reservation, which the claim path answers with a conflict rather than a second charge. That
  sentence is worth more than either of the two that were wrong.

## The shape to look for

A fix that *improves* behaviour and, in the same diff, adds a confident sentence explaining why the
improvement is safe. The improvement is usually real. The sentence is where the unexamined assumption
went.

Related: [two correct fixes can compose into a new defect](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md)
— same ticket-shape, one level down: there the *code* interacted badly, here the *claims* did.
