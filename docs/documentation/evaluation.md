# Documentation evaluation

Scope: all tracked prose, repo instructions, workflow references, and documentation assets.
This evaluates the application's documentation, not application correctness or production readiness.
Primary reader: an LLM loading only the context needed for one task.

## Fixed scorecard

Score each metric from 0 to 10. Both reviewers must independently award at least 8 on
**every** metric. Do not average away a failure. Freeze these criteria before the baseline review.

| ID | Metric | Evidence required for 8/10 |
|---|---|---|
| M1 | Accuracy | Current implementation claims agree with inspected code, contracts, and build configuration. State unverified claims and review limits. No known material contradiction. |
| M2 | Authority and currency | Readers can distinguish requirements, accepted decisions, implementation, proposals, and historical evidence. Resolve conflicting guidance explicitly. |
| M3 | Findability | A small entry index routes common tasks to their canonical document and relevant code. Decision and operations indexes avoid full-corpus loading. |
| M4 | Task execution | Setup, checks, configuration, recovery, and contribution guidance give prerequisites, exact commands, expected outcomes, and relevant failure paths. |
| M5 | Coverage | Every service, frontend, contract type, and document family has a discoverable owner or reference. Identify known gaps instead of implying completeness. |
| M6 | Contract and safety precision | Preserve money, tenant, authorization, event evolution, contention, and evidence boundaries. Procedures do not promise guarantees beyond their checks. |
| M7 | Token efficiency | Keep mandatory context short. Use task-specific files and canonical references. Remove repeated explanations without deleting requirements or evidence. Measure changed entry points before/after. |
| M8 | Clarity and structure | Plain language, descriptive headings, stable terms, readable commands, and explicit scope. No generic tutorial filler in routine project guidance. |
| M9 | Reference integrity | Validate local document links and moved anchors. Preserve evidence paths and external tooling references; state exclusions and any unresolved links. |
| M10 | Maintainability and provenance | Define where updates belong, what invalidates them, and how to recheck links and claims. Preserve decision history and reviewer evidence. |

Scoring anchors: 0 absent; 2 unusable; 4 major gaps; 6 usable with repeated detours;
8 meets the criterion with bounded disclosed limits; 9 strong with minor defects; 10 no
identified defect within the stated inspection. A known material defect caps its affected
metric at 7. A critical or high safety finding blocks completion until fixed or escalated.
Historical documents are evaluated for provenance and discoverability, not rewritten to
pretend they describe current behavior. Do not omit a document family to raise a score.

## Review method

1. Inventory every tracked document and supporting asset. Record counts, sizes, and classification.
2. Give Opus 5.5 and gpt-6.1-sol the same scorecard and immutable baseline.
3. Require scores, file-based evidence, defects, and an explicit inspection scope from each.
4. Verify findings against the files. Revise documentation only. Preserve binding requirements.
5. Request fresh independent reviews of the revised corpus. Keep sub-8 scores and open defects
   visible until resolved. Record results and verification separately from this fixed grid.

A corpus inventory is not a line-by-line implementation audit. Reviews must state which
files were read fully, scanned, or checked against code. Model scores are judgments within
that coverage, not proof that every technical assertion is true.
