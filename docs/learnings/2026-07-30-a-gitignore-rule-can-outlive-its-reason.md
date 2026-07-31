# A gitignore rule can outlive its reason — and predate its victim

**TKT-139, PR #136.** A finished architecture review sat on disk for five days, cited by an ADR,
invisible to the repository. The question "was this ignored deliberately?" looked like a judgement
call the owner had to make. `git log` narrowed it to one that could be answered.

## What happened

`docs/reviews/2026-07-25-architecture.md` — 147 lines, a full review at the 75-ticket mark whose
R-recommendations became TKT-126 and TKT-128 — was excluded by `.gitignore`. `AGENTS.md` says
*"Documentation is 100% in-repo"*, so the repository contradicted its own rule.

TKT-136 had already met this and **got it right**: it identified the file as present in the working
tree but excluded by `.gitignore:15`, reworded the citation to *"The review is not an in-repo
document"* — accurate about tracking — and opened TKT-139 because changing the policy was a separate
decision. What it left behind was a true sentence about a situation nobody had chosen.

The apparent options were: commit it, or record why it is ignored. The second needs a reason, and no
one knew one. That is where the ticket said *"only the repo owner can decide this."*

Two dates ended the discussion:

| | |
|---|---|
| `docs/reviews/` added to `.gitignore` | **2026-07-12**, in a commit titled *"untrack build artifacts/node_modules…"* that also deleted a 69 MB `bin/golangci-lint` |
| the review written | **2026-07-25** |

**The rule predates the file by thirteen days.** It could not have been a decision about this
document. A build-artifact sweep claimed a directory name, and a document later walked into it.

## The rule

**Before preserving a decision, check that a decision was made.** An ignore rule, a config default, a
flag — the fact that something is *configured* a certain way is not evidence that anyone *chose* it.
`git log -S` on the line costs one command. It cannot read anyone's mind — a directory rule can be
added prospectively — but it establishes *when* the line arrived and *in what company*, which is
usually enough to tell:

- **intent** — the entry arrived alone, or in a commit about that subject, usually with a comment; or
- **collateral** — it arrived inside a sweep, in a block of unrelated entries, undocumented, and, as
  here, before the thing it now affects existed at all.

The last of those is the strongest form: a rule that predates its subject cannot encode a judgement
about it.

The tell here was visible without history and I under-read it: `.gitignore:15` sat in the
build-artifacts block with **no comment**, while every deliberate neighbour explains itself
(`spike/` says *"TKT-39 Astro spike — throwaway prototype, not committed"*). Undocumented entries
among documented ones are a signal about how the line arrived, not about how important it is.

The cost of getting this wrong is asymmetric and quiet. Recording an invented rationale produces a
comment that *looks* like institutional memory and is fiction — and the next person to question the
rule finds a reason and stops, exactly where they should have kept pulling.

## The corollary: a link checker must resolve against `git ls-files`

The ticket also asked whether to build a docs-link checker. Note what one would have done here:

> A checker resolving paths with `os.Stat` **passes on this defect** on the author's machine — the
> file is present — and fails only in CI or a fresh clone.

The check must enumerate **tracked** files (`git ls-files`), never the filesystem, or it validates the
developer's working directory rather than the repository. Not hypothetical: this file is exactly the
shape that defeats a filesystem check. Whoever builds that checker should include an
ignored-but-present file as a regression case.

## And one thing that is now true forever

The review is in git history. **If this repository is ever made public, that history is public**,
including a candid internal assessment of the codebase's weaknesses. Nothing in it is a secret today
and it is appropriate in a private repo whose collaborators read the code — but the decision to open
the repo is a different decision, made later by someone who will not remember this file. Recorded
here because that is where it will be found, and not in a pull request nobody will re-read.

## One consequence to be deliberate about

Removing a **directory** rule un-ignores the whole namespace, not just the file that prompted it.
Everything future authors put in `docs/reviews/` is now tracked by default. That is the intent — it
is a documentation directory in a repo whose rule is that documentation is in-repo — but it is a
broader change than "commit one file", and anyone adding a scratch draft there should know it lands
in history.
