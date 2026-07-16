# Name what a control reaches, not what it is for

Date: 2026-07-15 · From TKT-67 (PR #51), TKT-57 (ADR-021) · Status: practice, not enforced by the gate

## What happened

A control gets named for the job it was *meant* to do. The name then gets read as a guarantee, by
everyone downstream — including its author, a week later. Four instances in two tickets, none of
which any test could catch, because in every case **the code did exactly what it said and the
sentence describing it was still false.**

ADR-021 §The trust boundary catalogues three from TKT-57: quarantine rows, retained epoch
signatures and a checkpoint chain, each described as constraining an adversary whose defining
ability is to delete them. TKT-67 added the fourth, and two more of the same shape in code:

- **"Refuse to boot unmonitored."** The check proved a NATS durable existed and filtered the alarm
  subject. Nothing in the repo consumed that durable — so the default stack passed the check and
  was unmonitored. `docs/development.md` already said "attaching the durable to a pager is the
  deployment's job", two paragraphs from the claim it falsified. The check was real; it reached
  *retention*, and got named for *monitoring*.
- **A verifier that recomputed a signed root from the same unauthenticated rows the signer used.**
  Signer and verifier agreed — on whatever an attacker had inserted, since nothing blocked an
  INSERT into the queue. Two components agreeing about attacker-supplied input is not verification,
  but "the verifier recomputes and compares the root" *sounds* like it is.
- **`canonical_version` stored on five tables and read by nothing.** The version was genuinely
  covered by the signed domain prefix, so the crypto was sound. The loose column was a second copy
  nothing enforced — a discriminator that could lie while every hash still verified.

## The tell

The failure never looks like a bug. Every individual fact is correct, the tests are green, and the
sentence is still an overclaim. So the usual detectors are all blind to it:

- **Tests can't see it.** There is no failing case; the code does what it does. TKT-67's own
  adversarial tests were written *from* the mistaken belief, so they confirmed it.
- **The author can't see it.** Both TKT-67 instances survived the author's own probe list, which
  had *named the category* and then checked the wrong member of it. Both were caught by review.
- **Consistency can't see it.** The contradiction was already sitting in the repo — the docs and the
  code comment disagreed for a full review round before anyone noticed.

## What to do instead

Write the reach, then check the name against it:

1. **Say what the control touches, mechanically.** "This proves a durable exists and filters the
   subject." Not "this makes us monitored."
2. **Ask what it does *not* reach**, and write that down in the same breath. If the answer is
   uncomfortable ("nobody reads this durable"), that is the finding.
3. **If the gap cannot be closed, say so where the claim lives — do not defer it.** Some gaps are
   not implementation debt: no boot-time check can prove a human will act on a page. ADR-021 §D6 is
   amended to say monitoring is a *deployment obligation*, because pretending it was a code
   obligation is what produced the false claim.
4. **Ship the honest signal instead of the flattering one.** TKT-67 could not prove anyone reads the
   alarms, so it exposes `access.lifecycle.alarm.durable_pending` — a durable nobody drains
   accumulates. That is a real, if partial, answer to a question the boot check only pretended to
   answer.
5. **When a verifier recomputes, ask what authenticates its inputs.** Recomputation over
   attacker-writable state proves agreement, not integrity.

The related trap — a fix whose test cannot fail — is
[`2026-07-15-prove-tests-fail.md`](./2026-07-15-prove-tests-fail.md). TKT-67 hit it again: a review
fix was mutation-checked, *no test failed*, and the fix had been landed with nothing able to catch
its absence. Running the check beat assuming it, twice in one review round.

## Where this is enforced

Nowhere automatic, and it probably cannot be. `AGENTS.md` carries the rule for the lifecycle trail
("say which adversary you mean before writing tamper-evident"); ADR-021 §The trust boundary carries
the reasoning. This file is the general form: **the ADR is about one trail, the habit is about every
control you name.**
