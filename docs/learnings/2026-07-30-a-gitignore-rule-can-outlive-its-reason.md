# A gitignore rule can outlive its reason — and predate its victim

**TKT-139, PR #136.** A finished architecture review sat on disk for weeks, cited by an ADR, invisible
to the repository. The question "was this ignored deliberately?" looked like a judgement call the
owner had to make. It was answerable from `git log` in one command.

## What happened

`docs/reviews/2026-07-25-architecture.md` — 147 lines, a full review at the 75-ticket mark whose
R-recommendations became TKT-126 and TKT-128 — was excluded by `.gitignore`. `AGENTS.md` says
*"Documentation is 100% in-repo"*, so the repository contradicted its own rule.

TKT-136 had already tripped over this: an ADR cited the review, the path did not resolve, and the
ticket concluded *"that file does not exist"*. It does exist; it is ignored. TKT-136 fixed the
citation by rewording it to *"The review is not an in-repo document"* — a true sentence about a
situation nobody had chosen.

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
`git log -S` on the line costs one command and distinguishes:

- **intent** — the entry arrived alone, or in a commit about that subject, usually with a comment; or
- **collateral** — it arrived inside a sweep, in a block of unrelated entries, undocumented, and
  possibly before the thing it now affects existed.

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
developer's working directory rather than the repository. This is not hypothetical; it is what made
the defect survive long enough for TKT-136 to misdiagnose it. Whoever builds that checker should
include an ignored-but-present file as a regression case.

## And one thing that is now true forever

The review is in git history. **If this repository is ever made public, that history is public**,
including a candid internal assessment of the codebase's weaknesses. Nothing in it is a secret today
and it is appropriate in a private repo whose collaborators read the code — but the decision to open
the repo is a different decision, made later by someone who will not remember this file. Recorded
here because that is where it will be found, and not in a pull request nobody will re-read.
