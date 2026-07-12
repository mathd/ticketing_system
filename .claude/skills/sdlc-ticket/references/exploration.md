# Upstream exploration — idea → brief (Jira)

Read this when the user brings an **idea or problem, not a spec** ("explore this", "I'm thinking about feature X"). Output: a brief in the epic, ready for `decomposition.md`. Exploration, PRD authoring, and decomposition run as **one flow ending at Gate 1** — no separate approval gate, but the drive style applies: the user can stop, steer, or restart at any point.

A vague brief produces a vague PRD produces vague tickets — spend the effort here, where it's cheapest.

**Fast path — already discussed.** If this session *already* holds the problem, the direction, and enough constraints (e.g. we just finished designing it together), **skip the interview**: synthesize the brief straight from the conversation and continue into `decomposition.md`. Don't re-elicit what's already on the table. Drop into a mode below only when the context is genuinely thin.

## Mode selection (ask first)

| Idea state | Mode |
|---|---|
| Fuzzy — problem known, solution isn't | **A. Coached brainstorm** (then B or C) |
| Confident — direction clear, needs structuring | **B. Product brief** |
| High-stakes or uncertain value | **C. PRFAQ (working backwards)** |

Modes compose: A → B is the natural chain; C replaces B when skepticism is warranted.

## Sharpen the language (every mode)

While eliciting, actively sharpen the domain language — a fuzzy term now is an ambiguous COS later:

- **Challenge conflicts.** A term that clashes with the decision registry / existing glossary gets called out on the spot: "you're using 'account' — Customer or User? Those are different things."
- **Cross-reference the code.** When the user asserts how something works, check the code agrees; surface contradictions rather than transcribing the claim.
- **Offer a decision record sparingly** — only when the choice is **hard to reverse**, **surprising without context**, and **the result of a real trade-off**. If any is missing, skip it. When all three hold, it's registry material (per the 3-houses rule in `SKILL.md`), not a throwaway line in the brief.

## Mode A — coached brainstorm

**Stance (non-negotiable): every idea comes from the human.** The agent facilitates — it creates the conditions for the user's thinking, it does not pitch solutions ("here are 10 features you could build" is banned).

1. **Setup** — agree on topic, desired outcome, constraints (one sentence each).
2. **Technique** — offer a menu, let the user pick: *role-play the persona*, *inversion* (design the worst version, flip it), *constraint removal* (no limits, then add back), *quantity first* (10 raw ideas, refine after).
3. **Facilitate** — probing questions, one technique at a time. Push with "why?" and "what would have to be true?", never with proposals.
4. **Organize** — group into themes; the user prioritizes.
5. **Close** — top 1–2 concepts get a next-step + success signal. Feed into Mode B or C.

Raw session notes worth keeping go in a collapsed section at the bottom of the brief.

## Mode B — product brief

Guided Q&A (problem → who has it → desired outcome → why now), then draft a 1–2 page brief:

```markdown
## Brief
**Problem** — <who hurts, how, evidenced by what>
**Target user** — <specific, not "users">
**Outcome** — <observable change when this ships>
**Non-goals** — <what this explicitly does not do>
**Assumptions & risks** — <each marked validated / unvalidated>
**Success metric** — <one number, measurable post-ship>
**Feasibility** — <from technical recon, below>
```

## Mode C — PRFAQ (working backwards)

1. Help the user write a **launch press release** (customer language, dated launch day: headline, the problem, what shipped, a customer quote) + a short **FAQ** (hardest questions first: who is this for, why now, what does it cost, what breaks).
2. Then switch roles: **hostile interviewer**. Attack every claim — "who exactly asked for this?", "what evidence supports that quote?", "what's the cheapest way this fails?". The user defends.
3. Surviving claims stay; the rest are **cut or demoted to flagged assumptions**. Output = the Mode B brief format, with the press release in a collapsed section.

## Technical recon (always, regardless of mode)

Before the brief is final, read the **actual repo**: where would this integrate, which existing patterns apply, what's the feasibility red flag (missing infra, schema implications, conflicting WIP). Findings go in **Feasibility**, distinguishing *read in the code* from *inferred*. Market/domain research is out of scope unless the user asks.

## Elicitation close

One **pre-mortem** pass on the finished brief (`quality-practices.md` §1): *this shipped and flopped / never shipped — why?* Patch or flag accordingly. Record the one-line lens note in the brief.

## Output & hand-off (Jira)

1. Create the **epic**: `createJiraIssue` type Epic, summary `Epic: <feature>`, description = the brief (`## Brief`). Status starts at **Backlog**. (For a heavier feature, put the brief on a **Confluence page** and link it from the epic instead.)
2. Continue directly into `decomposition.md` in the same run: the PRD is appended under `## PRD` (epic description or Confluence page), children are created with dependency links and baked context-mémo, and the flow ends with the **readiness verdict** on the epic.
3. ⛔ **Gate 1 covers the whole lineage:** by transitioning the children **Backlog → Ready** (prioritizing), the human approves brief, PRD, and decomposition in one decision — everything they need to judge is on the epic.
