# Documentation evaluation results

Review date: 2026-11-09. Application baseline: `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.
This evaluates documentation. It does not certify application correctness, security, capacity,
or venue readiness. The [fixed grid](evaluation.md) was written before either baseline review.
Its SHA-256 is `3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`.

## Ratings

Both models use M1–M10 and the same evidence requirements. Each metric must reach 8 from both
models. An average cannot satisfy this rule. Revisions 1 and 2 failed. Their scores remain below. Revision 3 is pending review.

| ID | Metric | Opus baseline | Sol baseline | Opus revision 1 | Sol revision 1 |
|---|---|---:|---:|---:|---:|
| M1 | Accuracy | 6 | 6 | 7 | 7 |
| M2 | Authority and currency | 5 | 6 | 8 | 8 |
| M3 | Findability | 4 | 5 | 8 | 9 |
| M4 | Task execution | 6 | 7 | 7 | 7 |
| M5 | Coverage | 5 | 8 | 9 | 9 |
| M6 | Contract and safety precision | 6 | 7 | 8 | 7 |
| M7 | Token efficiency | 3 | 3 | 8 | 9 |
| M8 | Clarity and structure | 5 | 6 | 8 | 8 |
| M9 | Reference integrity | 7 | 8 | 7 | 7 |
| M10 | Maintainability and provenance | 6 | 6 | 8 | 8 |

Full findings and inspection limits: [Opus baseline](reviews/opus-baseline.md),
[gpt-6.1-sol baseline](reviews/sol-baseline.md). Revision 1 at `771a5c1a`: [Opus](reviews/opus-revision-1.md), [Sol](reviews/sol-revision-1.md). Both baseline reviews found a design problem: mandatory context
mixed current rules with incident narratives, while current-state summaries lacked update triggers.
No critical or high safety finding was reported in either baseline.

Revision 2 at `f4eedf94`: [Opus](reviews/opus-revision-2.md), [Sol](reviews/sol-revision-2.md).
Both found incorrect decision-source citations. Sol also capped provenance because the proof
used the same incorrect default as the rewrite.

| ID | Opus revision 2 | Sol revision 2 |
|---|---:|---:|
| M1 | 8 | 8 |
| M2 | 7 | 7 |
| M3 | 9 | 9 |
| M4 | 8 | 8 |
| M5 | 9 | 9 |
| M6 | 8 | 8 |
| M7 | 8 | 9 |
| M8 | 8 | 8 |
| M9 | 8 | 9 |
| M10 | 8 | 7 |

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
board state were not checked in those baseline reviews. Revision 3 checked selected decision
bodies through read-only board requests, not current ticket status. The reports state each reviewer's exact limits. References to
`baseline.tar` and temporary ledgers in those reports refer to the original local review packet;
use the recorded baseline commit and committed inventories to reproduce its contents.

The revision covers every document family through indexes, authority rules and provenance labels.
Detailed decisions, historical findings, logs and receipts remain available. It does not replace
every historical sentence or claim a full implementation audit of the corpus.

## Changes and finding disposition

- Shortened `AGENTS.md`; retained binding rules in task-triggered engineering guidance. Added
  decision, runbook, contract, workflow and historical-record indexes.
- Moved 22 operations sections out of development. Kept their top-level headings as redirects.
  Moved ADR-085's nine detailed sections. The first revision preserved their bodies except
  relative links. Revision 2 qualified decision references but misassigned several source tickets.
  Revision 3 repairs them and removes the default-ticket convention. Old heading aliases preserve subsection links. No outcome rule or owner decision changed.
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

Exact UTF-8 byte counts compare the baseline with revision 3. A rough token estimate is bytes
divided by four. It is not a model tokenizer count. First-revision measurements remain in its
review reports.

| Entry | Baseline bytes | Revision 3 bytes | Reduction |
|---|---:|---:|---:|
| `AGENTS.md` | 35,156 | 6,935 | 80.3% |
| `docs/development.md` | 142,936 | 6,795 | 95.2% |
| `docs/LEARNINGS.md` | 56,248 | 14,106 | 74.9% |
| `docs/adr/ADR-085-scanner-local-outcome-model.md` | 234,176 | 15,271 | 93.5% |
| `docs/README.md` | 2,054 | 3,136 | Increased by 1,082 bytes for task routing |

Always-loaded AGENTS cost falls from roughly 8,789 to 1,734 tokens. Implementation tasks also
load engineering rules and relevant references. ADR-085 detail still exists in linked parts;
its entry reduction does not represent deleted decisions. Total prose can grow through indexes
and review evidence while task-specific loading falls.

## Revision 2 corrections

Revision 1's required gate failed on three scanner documentation tests. It stopped before build
and smoke. The recorded verdict was `FAIL exit=2 head=771a5c1ae6bb6520425d6453ec11804c66400cd8 tree=8ebb730835831c67a33c618db9ddd86063954921`. The defect was a document-consumer contract,
not an application behavior change.

Revision 2 keeps a short scanner safety synopsis at the tested development heading, including
exact time bounds and page-loss warning. It qualifies moved ADR decision references. Revision 2 preserved the outcome rules but
introduced the source-ticket errors identified in both second reviews. It also fixes credential scopes,
Go gate compatibility, non-Markdown incoming references, worker-overlay guidance, amendment and
historical labels, service-call purpose, and punctuation. Tests and application code are unchanged.

Opus recommended either a test change or undoing the moves, and objected to rewriting accepted
text. The owner explicitly authorized reformatting and reordering documentation. Qualifying
references is within that scope. The first-revision claim of unchanged bodies is limited to that revision. The later source
preservation claim was invalidated by the second reviews and is corrected below. No test was weakened.

The three unchanged `doc-namespace.test.ts` checks now pass. The subsequent full gate passed on revision 2. The source-citation defect still prevented
completion under the scorecard.

## Remaining context cost

The unchanged ticket skill is 65,275 bytes, roughly 16,319 tokens by bytes/4, before its stage
references. Ticket runs still load it under the selected workflow. Scanner work can still need
its approximately 35 KB runbook and large decision parts, especially the outcome tables. Use
section searches and task triggers. Entry-file reductions do not mean every task is cheap or
that the whole corpus shrank.

## Revision 3 corrections and proof limits

Every numbered decision reference in ADR-085 now explicitly names TKT-399, TKT-556, TKT-557,
or TKT-558. No default ticket remains. The correction covers grouped and historical forms at
nine sites in five parts, plus the TKT-557 TLS deviation citation in configuration. Old heading
aliases remain. The rewrite changes citation notation, not outcome rules.

The revision-2 proof was invalid for source ownership. It missed ticket context before a
number and the inherited source in slash groups. Its passing booleans were not evidence that
sources were preserved. Both failed review reports retain that finding.

The replacement [reference comparison](namespace-proof.tsv) compares the ten current ADR files
with revision 1 at `771a5c1a`. It extracts original immediate, historical, deviation and grouped
source forms. Only the original notation uses its documented TKT-556 default. Current references
must name their ticket. Ordered decision/source pairs match. Normalized text also matches after
removing citation spelling and heading aliases, excluding the separately rewritten citation
guide and two explicit routing corrections to the scanner runbook. This is a bounded notation
check, not a general authority parser or a proof that every decision was correctly summarized.

As a separate check, ten ambiguous decision identities were read from the configured board's
TKT-399 and TKT-557 records. [Decision receipts](decision-sources.tsv) record ticket, ID,
check date, body SHA-256 and short subject. These support the nine contextual sites and the
configuration deviation. They do not certify current ticket status, venue readiness, or the
unread remainder of the board. The original full records remain outside the repository.

Revision 3 also corrects the Compose-overlay startup wording, historical scanner pointer,
password paragraph and remaining staff punctuation. AGENTS restores the exact browser-submit
rule quoted in source comments. Maintenance now checks quoted rules as well as paths.
Application code and tests remain unchanged.

## Verification record

Revision 1's failed gate is recorded above. Revision 2 passed the complete `make check`,
including build and gateway smoke, on `f4eedf94df44fd37028714cdbdf903f31421eed6`.
Recorded verdict: `PASS exit=0`, tree `b916d86c480a6fe652abc34a0eccefe8bba9b7c3`.
The final PR records the gate result on its final committed head separately.

The fixed grid hash remains unchanged. All 274 baseline text paths remain. All 318 supporting
assets still match their baseline hashes. The original nine moved ADR bodies were compared
with the immutable baseline after relative-link relocation; later citation changes have the
bounded comparison described above. Revision 3 passes all three unchanged scanner documentation tests, the tracked link check
for 293 Markdown files, the 84-ADR uniqueness check, and a separate scan of 1,308 local
file/heading references with no reported issue. The latter uses a heading-slug approximation;
external links and renderer behavior are excluded. Revision 3 contains 318 text documents
and 3,197,601 bytes before this bookkeeping update. New review records account for some
corpus growth. Independent revision-3 scores remain pending. No completion claim yet. Browser forms were not changed; no new
`make browser` run was performed for this documentation change.

Maintain rules and indexes using [maintenance](maintenance.md). Preserve this review's date,
baseline and limits when future changes invalidate a size or implementation claim.
