# Shaping — Backlog → Ready readiness (DoR)

Read this when a ticket is in **Backlog** and needs preparing for Gate 1. Problem it solves:
tickets entering the pipeline with unresolved decisions stall in Planning — an LLM can't plan
around a pending product call. **Prefer not starting over blocking mid-flight**: shaping happens
in Backlog; `Ready` stays sacred (= claimable *now*).

Named practices this implements: Shape Up-style shaping (small prep effort ahead of delivery),
XP spikes (timeboxed investigation tickets), Example Mapping (surface rules/examples/questions),
SPIDR slicing (first sprint-sized slice).

## The DoR — 7 items on the ticket (`readiness` field)

| Key | Ready when |
|---|---|
| `objective` | Problem + why it matters is understood |
| `scope` | First delivery scope defined — what's in AND out |
| `uncertainties` | Key unknowns/assumptions/risks identified and acknowledged |
| `dependencies` | Major dependencies and blockers visible (as `blocked-by` links) |
| `approach` | Enough technical direction to start safely |
| `first_slice` | A meaningful increment that fits one sprint is identified |
| `success` | COS defined — we know what "done" looks like |

```json
"readiness": {
  "objective":  {"state": "met",  "note": "one-liner"},
  "approach":   {"state": "open", "note": "SSO in v1?", "owner": "human"},
  "uncertainties": {"state": "open", "note": "API rate limits unknown", "spike": "KEY-19"}
}
```

- `state`: `met | open | deferred`. Missing item = `open`.
- `deferred` = the explicit escape hatch: acknowledged as safely resolvable during Planning
  (decision rule: Ready = "no *major* redesign mid-sprint", not "zero unknowns"). Say why in `note`.
- `owner: "human"` on an `open` item = a **decision** only a human can make — it shows up in the
  board's "your move" strip. No ticket for it; the answer lands in the thread, the agent records it.
- `spike: "<KEY>"` links the item to the spike investigating it.

**Gate 1 is hard-blocked** (board-enforced in local mode; discipline in Jira mode) while any item
is `open` or an open blocker exists. `deferred` passes.

## The shaping pass (agent, in Backlog)

1. **Read the real code first** — most items resolve by reading, not asking (integration point,
   existing patterns, feasibility). Fill what you can yourself: draft objective, scope, COS,
   first slice (SPIDR: split by Spike/Path/Interface/Data/Rules until one slice fits a sprint).
2. **Surface the rest, Example-Mapping style** — for each COS/rule, try a concrete example; what
   you can't exemplify is an unknown. Sort unknowns into:
   - **investigations** (answerable by work) → spawn a **spike** (below);
   - **decisions** (answerable only by a human) → `open` + `owner: "human"`, grill concisely.
3. **Record** — write `readiness` on the ticket + a `kind=readiness` verdict comment
   (`<!-- sdlc:stage=backlog kind=readiness -->`): item states + what's left and who owns it.
4. Re-run the pass when a spike closes or a human answers — flip items, update the comment.

## Spikes — preparation work as first-class tickets

A spike is a ticket `type: "Spike"` whose deliverable is a **decision, not code**.

- **Create**: summary = the question; body/comment carries the **timebox** (default: 1 day) and
  expected output; link parent `blocked-by` spike; usually `risk:low` (plan gate skipped).
- **Flow**: same pipeline, same labels. Building = investigate (read code, prototype, measure) —
  **no PR expected**; a throwaway branch is fine, delete it. **Spikes skip PO Review**: they close
  `Building → Done` directly (the answer is judged by the humans at the parent's Gate 1, so a
  separate acceptance gate would be ceremony). Remove the pipeline label at Done as usual.
- **Close**: at Done, post the answer on the **parent** as
  `<!-- sdlc:stage=backlog kind=spike-result -->` (recommendation + evidence, ≤ 10 lines), flip the
  parent's readiness item (`open` → `met`, drop the `spike` ref into the note), and update the
  parent's `kind=readiness` comment.
- **Timebox is real**: expiring without an answer IS an answer ("too uncertain — descope or
  re-slice"); post that as the spike-result.
- Multiple spikes per parent are normal; they're the "multiple blockers during shaping" case and
  the existing never-claim-with-open-blockers rule orders everything.
