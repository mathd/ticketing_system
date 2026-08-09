# Shaping — Backlog → Ready readiness (DoR)

Read this when a ticket is in **Backlog** and needs preparing for Gate 1. Problem it solves:
tickets entering the pipeline with unresolved decisions stall in Planning — an LLM can't plan
around a pending product call. **Prefer not starting over blocking mid-flight**: shaping happens
in Backlog; `Ready` stays sacred (= claimable *now*).

Named practices this implements: Shape Up-style shaping (small prep effort ahead of delivery),
XP spikes (timeboxed investigation tickets), Example Mapping (surface rules/examples/questions),
SPIDR slicing (first sprint-sized slice).

## The DoR — 8 items on the ticket (`readiness` field)

| Key | Ready when |
|---|---|
| `objective` | Problem + why it matters is understood |
| `scope` | First delivery scope defined — what's in AND out |
| `uncertainties` | Key unknowns/assumptions/risks identified and acknowledged |
| `dependencies` | Major dependencies and blockers visible (as `blocked-by` links) |
| `approach` | Enough technical direction to start safely — **any remedy the ticket text already proposes is verified against the code before it is inherited** |
| `first_slice` | A meaningful increment that fits one sprint is identified |
| `success` | COS defined — we know what "done" looks like |
| `context_memo` | Context-mémo baked onto the ticket, **`governingAdrs` included** (`decomposition.md` § context-mémo) |

**Why `context_memo` is a gate item and not a nicety.** `SKILL.md` already says *every* pipeline ticket gets the bake — but nothing enforced it, so tickets reached Planning without one and the planner re-derived the context by hand, or didn't. The planner is the stage that can least afford to improvise it: whoever drafts reads **code, not decision history**, so an ADR that makes the obvious solution wrong is invisible to it — it recommends the wrong thing with every fact correct (TKT-62: ADR-008 is what made `CREATE INDEX CONCURRENTLY` pointless; TKT-61: ADR-017 *was* the whole ticket and arrived only because the orchestrator went looking). Resolve `governingAdrs` from `registry.bindingPath` at shaping, when there is time to read, rather than at Planning under a drafting deadline. `deferred` is available if the area genuinely has no governing decisions — but say so in `note`, so "none" is a finding rather than an omission. **Every cited ADR must resolve to a real file at HEAD** — `git -C <repo> cat-file -e HEAD:<registry.bindingPath>/<ADR>` or an `ls`. A mémo that names an ADR which does not exist (a planned-but-unwritten decision, or a slot reused for a different topic) sends the drafter looking for a spec that isn't there and costs a Planning-time reconciliation (TKT-56 cited `ADR-030-stripe-behind-psp-port.md`, but ADR-030 was the catalog-coverage gate and the Stripe ADR had never been written — it had to be authored mid-run as ADR-032). If the governing decision is not yet recorded, the honest mémo entry is a `deferred` item whose `note` says "ADR to be authored", not a citation of a file that does not exist.

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

**Exception: `approach: deferred` does *not* pass when the ticket touches auth, money paths, data
migrations, or CI/deploy config.** In that combination the escape hatch is doing the opposite of its
job: the decision rule behind `deferred` is "no major redesign mid-sprint", and on those four
surfaces the approach *is* the redesign — it decides a trust boundary, who may spend, or what runs
against production data. Deferring it moves that call from shaping, where there is time to read and
a human to ask, into Planning under a drafting deadline. Set `owner: "human"` and hold at Gate 1.
TKT-22 hit this twice in one epic: TKT-193's approach assumed an order read that returns fields the
contract forbids, and TKT-194's refund action turned out to be unreachable from the back office at
all — both discovered at claim time, both money- or auth-adjacent, both costing a rescope.

**A remedy proposed in the ticket text is a hypothesis, not a finding — verify it against the code
before `approach` inherits it.** Tickets are usually filed by whoever hit the problem, often citing a
precedent ("fix it the way TKT-N did"). That citation is the most trusted sentence in the ticket and
the least checked: it arrives with a ticket number attached, so shaping copies it into `approach` and
the plan brief carries it to the drafter as settled direction. The drafter reads code, not ticket
history, and cannot tell a verified remedy from a filed guess. TKT-233 was filed as "make expiry
deterministic (inject the clock, as TKT-229 did)" — but that test's expiry is decided entirely in
SQL (`expires_at` written as `now()+$interval`, swept by `expires_at<=now()`), so there is no Go-side
clock to inject and the precedent could not transfer. Reading the two call sites took a minute;
inheriting the suggestion would have spent a drafting run designing an interface that cannot exist.
If the proposed remedy does not survive the read, say so in `note` and record what replaces it — the
correction is worth more to the drafter than the original suggestion was.

## The shaping pass (agent, in Backlog)

1. **Read the real code first** — most items resolve by reading, not asking (integration point,
   existing patterns, feasibility). Fill what you can yourself: draft objective, scope, COS,
   first slice (SPIDR: split by Spike/Path/Interface/Data/Rules until one slice fits a sprint).
   This read is also where a **remedy the ticket proposes** gets checked rather than inherited
   (above) — including, and especially, one that cites a precedent ticket.
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
