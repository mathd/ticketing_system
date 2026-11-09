# Documentation evaluation results

Review date: 2026-11-09. Application baseline: `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.
This evaluates documentation. It does not certify application correctness, security, capacity,
or venue readiness. The [fixed grid](evaluation.md) was written before either baseline review.
Its SHA-256 is `3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`.

## Ratings

Both models use M1–M10 and the same evidence requirements. Each metric must reach 8 from both
models. An average cannot satisfy this rule. Revision reviews are pending at this candidate.

| ID | Metric | Opus baseline | Sol baseline | Opus revision | Sol revision |
|---|---|---:|---:|---:|---:|
| M1 | Accuracy | 6 | 6 | Pending | Pending |
| M2 | Authority and currency | 5 | 6 | Pending | Pending |
| M3 | Findability | 4 | 5 | Pending | Pending |
| M4 | Task execution | 6 | 7 | Pending | Pending |
| M5 | Coverage | 5 | 8 | Pending | Pending |
| M6 | Contract and safety precision | 6 | 7 | Pending | Pending |
| M7 | Token efficiency | 3 | 3 | Pending | Pending |
| M8 | Clarity and structure | 5 | 6 | Pending | Pending |
| M9 | Reference integrity | 7 | 8 | Pending | Pending |
| M10 | Maintainability and provenance | 6 | 6 | Pending | Pending |

Full findings and inspection limits: [Opus baseline](reviews/opus-baseline.md),
[gpt-6.1-sol baseline](reviews/sol-baseline.md). Both found a design problem: mandatory context
mixed current rules with incident narratives, while current-state summaries lacked update triggers.
No critical or high safety finding was reported in either baseline.

## Scope and provenance

The baseline contains 274 tracked text documents, 249 Markdown and 25 text, totaling 3,062,164
bytes. [Inventory](inventory.tsv) lists each baseline path, family, byte count, SHA-256 and
revision disposition. The 561 tracked files under `docs/` include 243 of those text documents
and 318 supporting assets. [Asset inventory](assets.tsv) preserves their paths, sizes and hashes.
No historical evidence asset was changed or rerun to improve a result.

Both reviewers structurally scanned all 274 documents. Sol fully read 13; Opus fully read 17,
partly read 24, and checked 34 selected implementation claims. Structural scanning is not a
full semantic read. The remaining ADRs, learnings, workflow references and evidence received
limited inspection. Images were inventoried, not visually qualified. External links and live
board state were not checked. The reports state each reviewer's exact limits. References to
`baseline.tar` and temporary ledgers in those reports refer to the original local review packet;
use the recorded baseline commit and committed inventories to reproduce its contents.

The revision covers every document family through indexes, authority rules and provenance labels.
Detailed decisions, historical findings, logs and receipts remain available. It does not replace
every historical sentence or claim a full implementation audit of the corpus.

## Changes and finding disposition

- Shortened `AGENTS.md`; retained binding rules in task-triggered engineering guidance. Added
  decision, runbook, contract, workflow and historical-record indexes.
- Moved 22 operations sections out of development. Kept their top-level headings as redirects.
  Moved ADR-085's nine detailed sections without changing their bodies, except relative links.
  Kept original subsection anchors at the stable ADR path.
- Corrected toolchain, staff role enforcement, scanner module/outcome guidance, EUR-only sales,
  implemented test-mode PSP support, gate order and service-call boundaries against source.
- Registered all 55 base-Compose interpolation inputs and the twelve generated contract outputs.
  Documented hooks and their runtime limits. Added update triggers and a known-limits register.
- Corrected the load-summary commit and INSERT statistics against its raw JSON. The existing full
  load target still overwrites that tracked evidence path. Changing a Makefile is outside this
  documentation-only change; the warning is now explicit.
- Added amendments and historical-scope labels. ADR-050's `internal/mail` reference is a rejected
  design alternative, not a current directory claim; it was clarified rather than replaced with
  a path that would change the decision's meaning.
- Retained the repository's branch and commit policies. A reviewer's memory is not repository
  authority, and historical commit naming does not amend the written convention. ADR-073 is absent
  from this baseline; check open work and the board before allocating that number.

## Context cost

Exact UTF-8 byte counts below compare each baseline file with the first revision candidate.
A rough token estimate is bytes divided by four. It is not a model tokenizer count.

| Entry | Baseline bytes | Revision bytes | Reduction |
|---|---:|---:|---:|
| `AGENTS.md` | 35,156 | 6,777 | 80.7% |
| `docs/development.md` | 142,936 | 5,596 | 96.1% |
| `docs/LEARNINGS.md` | 56,248 | 13,860 | 75.4% |
| ADR-085 entry | 234,176 | 13,036 | 94.4% |
| `docs/README.md` | 2,054 | 3,136 | Increased by 1,082 bytes for task routing |

Always-loaded AGENTS cost falls from roughly 8,789 to 1,694 tokens. Implementation tasks also
load the engineering rules and relevant references. ADR-085 detail still exists in its linked
parts; the 94.4% figure measures its entry file, not deletion of decision content. The total
corpus can grow through indexes and review evidence while task-specific loading falls.

## Verification record

Candidate preparation verified all nine moved ADR-085 bodies against the immutable baseline,
after only relative-link relocation. The fixed grid hash is unchanged. Final model reviews,
link/anchor checks, gate result, and final corpus measurements will be recorded here after the
candidate is reviewed. This pending section grants no completion claim.

Maintain the rules and indexes using [maintenance](maintenance.md). Preserve this review's date,
baseline and limits when future changes invalidate a size or implementation claim.
