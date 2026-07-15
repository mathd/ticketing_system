# Quality practices — plan, review, hand-off, course-correction

Four practices applied at specific points of the pipeline. Each is cheap; skipping them is how plans bounce, PRs bloat, and reviews stall. (Comment-marker formats are in `SKILL.md` § Memory & metrics.)

## 1. Pre-mortem pass on plans (in `agent:plan-review`, on the draft)

Who drafts and who critiques is `config.models.plan` / `config.models.planReview` (`SKILL.md` § Config) — **always two different models**, so the critic is always reviewing someone else's draft. That is what this pass is for: a draft is a proposal, not a plan.

**Read the draft's own Cons/Risks against its Decision — first, before any code check.** It is the cheapest possible reject and needs no verification at all: if a Con negates the Decision, the draft is refuted by its own evidence. The known drafter failure is a **hallucinated seam** (facts wrong); this is the *opposite* one — **every fact right, the synthesis wrong**. A drafter is unattached to an approach only until it has drafted one, and it will carry a fatal Con in its own §Cons and adopt the approach anyway. TKT-62 is the worked example: the draft wrote *"concurrency preserves database writes for another replica or client, not HTTP availability by itself"* and *"removing the fixed migration deadline accepts potentially unbounded startup"*, then recommended `CREATE INDEX CONCURRENTLY` with ~250 lines of machinery. Both sentences refute the plan they appear in. Quote the self-refuting line in `plan-final` — a critic who verifies every seam but never reads the Cons will accept a well-researched wrong answer.

**Verify the test seam the draft picked — or pick one if it didn't.** The plan must say *where* this feature is exercised: prefer an **existing** seam over a new one, the **highest** seam that still isolates the change, and the **fewest** seams possible (ideal: one). **Confirm the named seam actually exists** (`git grep -n` at HEAD) — a hallucinated seam is a drafter's most likely failure, and a drafter that couldn't run the gate has no way to catch it itself. Record the chosen seam in the final plan and, if it's a new one, why no existing seam fit — the implementer tests through it, so a wrong seam is a rewrite. (Seam = a place you can alter behaviour without editing there — Feathers.)

**A seam that exists is not yet a test that can fail — check the fixtures too.** Verifying the seam is a spelling check: it proves the drafter didn't hallucinate, nothing more. The next question is **"what input shape can this test not represent?"**, and for **compatibility, format, schema, versioning, or parser** work it has a specific deadly answer: **a fixture built from the same type the code under test decodes with cannot express incompatibility.** It encodes the very compatibility the test claims to prove, so the test passes while the bug ships — and it looks thorough doing it. TKT-61 is the worked example: the plan's "unknown schema is parked" test built its schema-4 payload from the current Go struct, so it only ever exercised future variants shaped like the present one — the subset that needed protecting least. The bug (real future variants still terminated, service still reporting healthy) survived the plan gate, an implementation, a mutation-check that killed three mutants, and a full adversarial review pass; it took a *second* pass to catch. **Mutation testing does not save you here** — it proves the test detects a change in the *code*, and says nothing about whether the *input space* is right. Every mutant died correctly, downstream of a fixture that could not represent the failure. So: for this class of work, require **hand-written fixtures** (literal JSON/bytes with renamed keys, changed types, unexpected shapes) and reject "we'll construct it with the existing type" in the plan, not at review.

**Grill the human on the open decisions (complex tickets, when interactive) — this is the main agent's job, always.** A delegated critic cannot hold a conversation; it gets one brief and returns one answer. So whatever `config.models.planReview` says, *you* put the plan's unresolved *decisions* to the user **one question at a time**, each with your recommended answer, and wait for each. Look **facts** up in the code yourself — only genuine decisions go to the human. This extracts human context that neither the pre-mortem (author-internal) nor a fresh-context critic (no ticket history) can reach. Skip it for `risk:low` or when the user chose *gates only* drive style.

After drafting the plan, re-examine it through one **named** reasoning lens — a generic "double-check it" re-confirms; a named lens forces an angle of attack.

Default lens — **pre-mortem**: *assume this ticket bounced — the plan was rejected at Gate 2, or the PR at Gate 3. Work backward: what went wrong?* List the 3–5 most plausible failure causes (wrong integration point, missing migration, untestable AC, hidden coupling, scope too big…), then patch the plan where a cause is credible.

If pre-mortem fits poorly: **inversion** (what would guarantee failure? avoid that), **first principles** (strip assumptions, rebuild), **stakeholder mapping** (re-read as the reviewer/user/on-call). One lens, one pass — a second look, not a committee.

Record one line in the `plan-final` comment: lens applied + what it changed (or "no changes").

Why the two roles are always different models: **a drafter is unattached to a favoured approach right up until it has drafted one** — after that it wants its plan accepted. The critic has no such stake, which is the entire reason the split exists. The two sides bring different things, so assign them deliberately: the **grounded** side (repo, ticket thread, ability to run the gate) is the one that can *falsify* a draft; the **fresh** side is the one that brings an angle unattached to the ticket's history. Drafting is where a fresh angle pays; critiquing is where grounding pays. `config.models` decides which model sits where — this pass is the same either way, and the critic is never handed the author's justification.

## 2. Findings triage (in `agent:ai-review` and at Gate 3 feedback)

Every finding — from the Codex adversarial review or the human reviewer — gets classified before acting:

- **Blocking** — correctness, security, or AC violation *in this diff* → fix now, in the PR.
- **Incidental** — real but out of scope (pre-existing issue, adjacent improvement, nice-to-have) → **create a Backlog issue** (`createJiraIssue`, status Backlog), link the finding in its body, note the deferral in the review reply.
- **Rejected** — false positive or nitpick → say so explicitly, with the reason. Adversarial reviewers are instructed to find problems, so some won't exist; filtering is part of the job.

Invariant: **no finding is silently dropped.** Each one changes the diff, produces a ticket, or gets a stated rejection. Fixing incidental findings in the PR is scope creep — it bloats the diff, slows review, and the SHA churn invalidates earlier review effort.

### 2.b Running the local gate honestly (`agent:coding`)

The local gate is the source of truth for "green" — but only when run against a **committed** tree. Two failure modes seen in practice (TKT-49):

- **Commit before the gate, generated files included.** If the project's gate has a codegen-drift check that diffs generated output against `HEAD` (this repo: `check-generate` compares `openapi_gen.go` / `api-types.gen.ts` to `HEAD`), it **cannot pass on a dirty branch** — regenerated-but-uncommitted files read as drift. So the honest sequence in Building is: implement → `make generate` → **commit (generated files included)** → run the gate. This also mirrors how CI runs it (against the committed PR tree).
- **Never stage-then-unstage to make a gate pass.** A sub-agent (or you) satisfying a `HEAD`-diffing check by transiently staging generated files produces a **false green** — the working tree as it sits does not match a clean `make generate`. When delegating implementation, the prompt must forbid this and require committing the regenerated output. **Verify, don't trust:** re-run the gate yourself against the committed tree before advancing to `agent:ai-review` — a sub-agent's "gate passed" is a claim, not evidence.

## 3. Ready-for-review walkthrough (the Gate 3 hand-off, on the PR)

The "ready for review" comment **on the PR** (code repo, not Jira) is a guided walkthrough, not a pointer at the diff. The reviewer's time is the pipeline's scarcest resource; structure it (marker on line 1 — see `SKILL.md`):

```markdown
<!-- sdlc:stage=ai-review kind=review-guide -->
### 🔎 Review guide — <ISSUE-KEY>

**Scope:** <n> files, +<x>/−<y> — <one sentence: what this PR does>.

**Walkthrough** (by design concern, not by file):
1. <concern> — `src/foo.ts:42` — what to check and why it's shaped this way
2. <concern> — `db/…` — …

**High-risk spots:** <each tagged `[security]` `[schema]` `[data]` `[concurrency]` `[perf]` + location — or "none">
**Manual checks:** <1–3 observable verifications the reviewer can run>
**Deferred (triage):** <Backlog tickets from incidental findings, or "none">
```

Order the walkthrough by risk: the thing most likely to be wrong goes first.

## 4. Correct-course: regenerate from the failing layer

When a human pushes back (plan rejected at Gate 2, changes requested at Gate 3) or a stage keeps failing, **diagnose which layer broke before touching anything**:

| Layer | Symptom | Action |
|---|---|---|
| **Intent** | the issue/AC itself is wrong, stale, or ambiguous | Back to scope: fix the issue with the user (Backlog work), then re-plan |
| **Plan** | AC fine, approach wrong | Swap to `agent:planning`, regenerate the plan — don't patch code toward a broken plan |
| **Implementation** | approach fine, code wrong | Swap to `agent:coding`, fix in place |

Corrections regenerate from the source layer — never patched locally at a lower one (patching code to satisfy feedback that's really a plan problem produces code that fights its own design). The label/transition mechanics for moving backward are in `SKILL.md`; this is the decision rule for *how far back*. State the diagnosis in the comment when resuming: "treating this as a plan-layer issue: <why>".
