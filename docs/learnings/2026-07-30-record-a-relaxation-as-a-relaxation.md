# Record a relaxation as a relaxation

**TKT-119, PR #131.** An ADR clause said alarm payloads carry *"bounded identifiers and enums only"*.
No shipped payload had ever satisfied it. Correcting the clause took three review passes — not
because the fix was hard, but because each attempt described the change less honestly than the change
deserved.

## What happened

ADR-025 §D9 is the PII contract a future consumer implements from. It claimed identifiers and enums
only. Six fields across the three shipped alarm classes fall outside that: `alarmData.occurred_at`
and `alarmData.reason`; `conflictAlarmData.device_occurred_at` and `.skew_flagged`;
`policyConflictAlarmData.version` and `.revisable`. §D5 of the same ADR *requires* the device-claimed
time. The clause contradicted its own document, and a strict reading rejected every alarm in
production.

The ticket framed this as the two newer classes outgrowing the clause. It was worse: the **integrity**
payload — the original class the clause was written for — has carried a timestamp *and* a free-text
`reason` since it shipped. §D9 never described anything.

Three passes, three different errors, all in the same paragraph:

| Pass | Error | Why it survived the previous fix |
|---|---|---|
| 1 | The amendment prohibited "free text" — which `alarmData.reason` (`cause.Error()`) emits | The bound was added to stop "operational scalars" licensing arbitrary data. It made the clause wrong in the *other* direction. |
| 2 | The rationale said "keeps every prohibition", and miscounted three contradictions | Only visible once the clause itself was right |
| 3 | "Added: no free text, no nested objects" — but `only` already entailed those | Only visible once the delta was itemised at all |

## The rules

**1. A clause that widens a constraint must say it widens it.** The original said identifiers and
enums **only**. That word prohibited exactly the operational scalars and the free-text `reason` the
amendment admits. Writing "this keeps every prohibition" was false, and the falsehood is the kind
that compounds: a future reviewer reads the carve-out as pre-existing policy rather than as an
accepted risk, and the next widening measures itself against the already-widened baseline. **The
honest form is a delta** — what is preserved, what is relaxed, what was implicit and is now explicit:

- *Preserved* — the three identity exclusions (buyer, guest reference, raw operator identity).
- *Relaxed* — the word "only", deliberately, because those fields already ship and §D5 mandates one.
- *Preserved and made explicit* — no device/user-supplied free text, no nested objects. `only`
  already entailed these; the original never enumerated them.

That third category is the one pass 3 caught. Calling those "Added" asserted the original permitted
them, which reads as *"we tightened it here"* when the truth is *"this is what survived"*.

**2. Fixing a decision record is not a transcription task.** There was no test to catch any of this.
Half 1 of the same ticket — a code change to boot diagnostics — was approved in pass 1 and never
came back. Every finding landed on the half with no executable check, which is the half that looks
cheap. Budget review attention accordingly: **prose that governs future implementations deserves
more adversarial passes than code that has tests**, not fewer.

**3. When you amend a clause, sweep every restatement — and know which hits are quotes.** The rule
was restated in six places (code comments, tests, operator docs) plus cited in two more. Citations
(*"§D9 constrains these payloads"*) stay correct after an amendment and must **not** be edited —
that is churn. Restatements (*"identifiers and enums only"*) must move or the contradiction simply
relocates. After the sweep, `git grep "identifiers and enums"` returns only the amendment quoting
the old wording to explain itself, which is the state you want: the false claim survives exactly once,
inside the sentence explaining why it was false.

## The trap underneath all three

Every individual sentence in each rejected version was defensible. The clause was accurate about
fields; the rationale was accurate about §D5; the categories were accurate about what the amendment
did. What was wrong each time was **the shape of the claim** — a relaxation dressed as a correction,
a count stated as exhaustive, a survival labelled an addition.

That is the failure a test cannot catch and a careful author reliably misses, because the author is
checking whether each sentence is true rather than whether the paragraph *as a whole* would mislead
the next reader. Related: [a fingerprint of a symmetric secret is an
oracle](./2026-07-30-a-fingerprint-of-a-symmetric-secret-is-an-oracle.md), where every careful
property proven about the design was true and none of them was the risk.
