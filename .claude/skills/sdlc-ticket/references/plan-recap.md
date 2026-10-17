# Plan recap — the plan a human can read in one minute

Every ticket that is planned gets a **plan recap**: a short, plain-language page of the current plan,
for the owner and the PO. The board shows the newest one in the ticket window's **Plan** tab. It is
the readable output of Planning. The `kind=plan` draft, its amendments, the review rounds and the
decision log stay in the thread for the record; nobody should have to rebuild the plan from them.

## What it is, and what it is not

- **It is** the current, complete plan, said simply: what is decided, what changes, how it behaves,
  how we know it is done, how it will be built, and what could go wrong.
- **It is not** a history. No earlier versions, no review transcript, no rejected alternatives, no
  model names, no tool logs. Those are in the thread.
- **It adds nothing.** Every behaviour, scope item and COS in it must be in the reviewed plan. If
  writing the recap shows a gap or a choice the plan did not make, that is a plan change: it goes
  back to plan-review, never into the recap alone.

## Who writes it, who checks it

The recap is checked like the plan, so it needs the same independence: its checker must not be one
of its authors (`reviews.md` § Inputs and identity). **The plan's drafter writes the recap**
(`config.models.plan`, briefed through its runner like the draft), and **the plan reviewer checks
it** (`config.models.planReview`). The skill already refuses a config where those two are the same
model, so the recap inherits the plan's cross-model guarantee in every configuration. You, the
orchestrator, brief the drafter and post the result; you do not write or edit the recap yourself,
or you become its author. When `plan` is `main-agent`, you are the drafter, and `planReview` is
still another model. Record the recap's author and checker in `plan-final`.

## When

1. **Planning — before the last plan-review round's decision audit.** The drafter writes the recap from the
   complete plan (the draft with every amendment applied, not the delta) and give it to the auditor
   with the plan (`reviews.md` § One round step 3). The audit checks that the recap states the plan
   faithfully: no missing COS, no added behaviour, the open decisions shown. If a later round
   changes the plan, write a new recap for that round's audit.
2. **After the last round's fixes.** Fixes below the repeat threshold change the plan without
   another round (`reviews.md` § Stage policy). If they change anything the recap says, the drafter
   rewrites the recap from the complete amended plan, then the plan reviewer does a **fidelity check**
   only: one read-only call that compares the new recap with the complete amended plan and answers
   faithful / not faithful with the differences. It is not a review round and does not count
   against the cap; it checks presentation, not the plan. Record its answer in `plan-final`. A
   difference is fixed in the recap; a gap in the plan is a plan change, back to plan-review.
3. **Post it** as `<!-- sdlc:stage=planning kind=plan-recap -->` together with `kind=plan-final`,
   before Gate 2 (or before the autonomous `Planning → Building` move). `risk:low` tickets skip the
   plan gate, not the recap.
4. **Building — refresh only when the plan's meaning changed.** A material change returns to
   Planning anyway (`quality-practices.md` §4). A non-material clarification that changes what the
   recap says (a decision logged in Building) gets a new recap with
   `<!-- sdlc:stage=coding kind=plan-recap -->`, status `revised in Building`, naming the decision.
   Do not post a recap for implementation detail the recap never mentioned.
5. **PO Review** uses the recap's *Done when* list as the checklist; the validation note answers it
   item by item.

The newest `kind=plan-recap` comment is the current one. Never edit an old one (comments are
append-only); post a new one.

## Format

Use only the markdown the board renders: `#` headings, `**bold**`, `` `code` ``, `-` bullets,
`1.` numbered lists, blank-line paragraphs, and fenced code blocks. No tables, no links, no nested
lists. Keep it under about 60 lines. Write for a PO: short sentences, domain words, no internal
jargon. Name files and tests only where a COS needs its proof.

```markdown
<!-- sdlc:stage=planning kind=plan-recap -->
# <TICKET-KEY> plan — <the outcome, in one line>

**Status:** awaiting approval · **Plan:** `<first 12 hex of the plan digest>` · **Decisions:** D2, D4

## Decide now
- <a question only a human can answer> — <what changes with each answer>
- (or) Nothing to decide.

## What changes
<Two to four sentences: the problem, who has it, the outcome after this ticket.>

## Scope
- **In:** <…>
- **Out:** <…>

## How it behaves
- <observable behaviour, including the important failure cases>

## Done when
1. **COS-1** <the criterion> — shown by `<test or check>`
2. **COS-2** <…> — shown by <…>

## How it is built
- <the approach in a few steps, in build order, with the boundaries it respects>

## Risks
- <risk> — <what limits it>
- (or) None identified.
```

`Status` is one of `awaiting approval`, `approved` (after Gate 2), `revised in Building (D<n>)`.
`Plan` is the first 12 hex characters of the SHA-256 of the complete plan text the recap describes:
the plan the last round saw, or the amended plan when step 2 applied. The recap names the plan it
describes. `Decisions` lists
the decision IDs that shaped what the recap says, or `none`.

**Decide now comes first.** A reader who must decide something sees it before any detail. In
`gates: "autonomous"` there is nothing for the human to decide before Building: list the decisions
the agent took instead, as `Decided by the agent: D<n> — <choice>`, so the owner can see them.
