# A rising finding count indicts the mechanism, not the last patch

**2026-09-26 — TKT-271**

## What happened

TKT-271 gave the scanner a local list of voided ticket ids. The plan bound that list to the
current pairing: a new pairing or a 401 cleared it, and a pull that began under an old pairing
could not write into the new one. Each review pass then found pairing defects, and each defect came
from the previous fix to the binding:

| Pass | Findings | Where |
|---|---|---|
| 1 | 8 | 2 in the binding (re-pair during a pull, a stale tab writing into a new pairing) |
| 2 | 3 | 2 in the binding fix (a stale tab's 401 clears a newer pairing, a stalled old pull blocks the new one) |
| 3 | 5 | 3 in the binding fix (legacy metadata with no owner, two tabs sharing one token, the unpair of a newer pairing) |

Pass 3 went UP. At that point the question changed from "how do I close this edge case" to "what
does the binding protect". The answer was nothing. Refunded and exchanged are append-only
lifecycle facts, and ticket ids are globally unique, so an id learned under ANY pairing is a correct
denial forever. A foreign or stale merge can only add correct denials. Decision D5 deleted the
binding: the list is a merge-only union, and only the completion time (the one value that describes
"this device's view") is keyed per token fingerprint. The net diff was negative, and pass 4
confirmed the premise in code and found no defect in the union design.

## The rule

- **When findings rise from one pass to the next and cluster in one mechanism, stop patching it.**
  Ask what the mechanism protects, in one sentence, naming the input it changes the output for.
  If you cannot, delete it (compare [the mechanism was inert](2026-08-23-the-mechanism-was-inert-not-the-test.md)).
- **Ownership rules on a set of permanent facts are a smell.** If every element stays true forever,
  whoever learned it, the set needs no owner. Bind only the values that describe a point of view
  (here: when THIS token last completed a walk).
- **Check the premise against the code, not the ADR.** The union is correct only because nothing
  can make a voided id admissible again. Pass 4 was asked to find such a path (un-refund, id reuse,
  a feed that emits a non-voided id) and found none. If one is ever added, the union is wrong.

## A second, smaller lesson from the same ticket

A pull that outlived its React component kept running and called `fetch`. In the scanner suite
that `fetch` was the NEXT test's stub, and one unrelated test failed about 1 run in 20. The
implementer saw it once and it passed on rerun. Loop the suite (20+ runs) before calling a new
async loop done, and make every async loop stop on unmount.
