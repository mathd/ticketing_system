# gpt-6.1-sol revision 2 documentation evaluation

Reviewer: gpt-6.1-sol, high effort. Date: 2026-11-09.
Candidate: `f4eedf94df44fd37028714cdbdf903f31421eed6`.
Baseline: `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.
Previous candidate: `771a5c1ae6bb6520425d6453ec11804c66400cd8`.

I used the unchanged M1–M10 grid in `docs/documentation/evaluation.md`. Its SHA-256 remains `3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`. This report applies unslop. Findings only. No tracked edits, source changes, gate runs, network operations or delegation.

## Scores

| ID | Metric | Score / 10 | Evidence |
|---|---|---:|---|
| M1 | Accuracy | 8 | Staff credential scopes and the Go gate requirement now agree with inspected source. The worker-overlay caveat and Access-to-Commerce purposes are correct. The remaining finding concerns decision-source provenance rather than a changed implementation claim. |
| M2 | Authority and currency | 7 | Requirement, implementation, proposal and historical boundaries are clear. However, the new ADR citation convention assigns the wrong default source to several historical and grouped references, finding R5. |
| M3 | Findability | 9 | Task, decision, operations, contract and historical indexes remain useful. Durable reconciliation and password recovery now have redirects for the non-Markdown consumers. |
| M4 | Task execution | 8 | Setup now states the gate's Go minor-version restriction. The fixed scanner synopsis satisfies the unchanged test contract by inspection; the parent reports all three targeted tests passing. Full gate verification is pending, so this score does not certify completion of that gate. |
| M5 | Coverage | 9 | Every service, frontend, shared-package family and contract type still has a discoverable reference. Historical document families, missing provenance and remaining implementation limits remain explicit. |
| M6 | Contract and safety precision | 8 | The wrong credential scopes are corrected. The 3 s/30 s limits, equality rule, page-loss residual and reconciliation boundary remain at the stable tested entry. ADR changes preserve the actual outcome rules under normalized comparison. |
| M7 | Token efficiency | 9 | Measured entry reductions remain large. The unchanged 65 KB ticket skill and expensive scanner parts are now disclosed as remaining task cost. Detailed decisions and incident evidence remain available. |
| M8 | Clarity and structure | 8 | Staff punctuation and legacy-note fragments are repaired. Index labels are deliberately shortened. Repeated ADR namespace prefixes make some long tables dense, but the structure and task scope remain usable. |
| M9 | Reference integrity | 9 | Executed checks pass for 291 Markdown files and 1,304 local references. Old heading aliases and the fixed scanner synopsis preserve incoming document consumers. External URLs and renderer-specific behavior remain outside this result. |
| M10 | Maintainability and provenance | 7 | Maintenance now covers tests, runtime strings and other non-Markdown consumers. Earlier failed scores and evidence are retained. The committed namespace proof overstates source-ticket preservation because its parser misses contextual and grouped ownership, finding R5. |

Result: FAIL. Not every metric reaches 8. M2 and M10 remain below the threshold because of one material provenance defect. No Critical or High safety finding was identified. Final reporting and the still-pending full gate are not themselves documentation defects.

## R5. Medium: contextual and grouped decision sources are missed by the namespace proof

The new citation guide, `docs/adr/scanner-outcomes/baseline-evidence.md:15–22`, defines an `ADR-085 Dn` reference as a TKT-556 decision unless an explicit source qualifier follows it. The namespace rewrite left several historical or grouped TKT-399 references without that qualifier. They now select the TKT-556 decision under the new rule, despite nearby context naming TKT-399.

The distinction matters. `decision-log.md:22` says the design-first decision is `ADR-085 D39 of TKT-399`; the separate TKT-556 D39 at line 109 concerns pre-decision database bounds. `delivery-plan.md:13` now calls the design-first decision `TKT-399 history ADR-085 D39`, which contradicts the formal source convention. Also, the guide says TKT-556 D30 was replaced; the owner time limit is TKT-399 D30. Leaving the second half of a grouped time citation as bare `ADR-085 D30` directs it to that retired TKT-556 identity.

I scanned all ten ADR-085 files for explicit, contextual and grouped ticket-source forms. This is the full identified batch:

| Revised location | Original text at revision 1 | Revised source problem |
|---|---|---|
| `outcome-tables.md:29` | `TKT-399 D29/D30` | `ADR-085 D29 of TKT-399/ADR-085 D30` leaves the second decision without the TKT-399 source. |
| `outcome-tables.md:41` | `TKT-399 history D33` | `TKT-399 history ADR-085 D33` defaults the decision to TKT-556. |
| `outcome-tables.md:244` | `TKT-399 history D7/D11` | Both rewritten references default to TKT-556. |
| `finding-map.md:27` | `TKT-399 history D18` | The rewritten reference lacks the TKT-399 qualifier. |
| `delivery-plan.md:13` | `TKT-399 history D39` | The rewritten reference lacks the TKT-399 qualifier, despite describing its design-first decision. |
| `storage-upgrade.md:17` | `TKT-399 history D18` | The rewritten reference lacks the TKT-399 qualifier. |
| `decision-log.md:69` | `TKT-399 D29/D30` | The second rewritten time-rule reference lacks the TKT-399 qualifier. |
| `delivery-plan.md:31` | `TKT-557 D29/D33` | `ADR-085 D29 of TKT-557/ADR-085 D33` leaves D33 without its TKT-557 source. |

I checked the relevant decision bodies in the local TKT-399 and TKT-557 board snapshots supplied by the parent. TKT-399 confirms D7, D11, D18, D29, D30, D33 and D39 for the contextual cases above. TKT-557 D29 states the full venue-use precondition, and TKT-557 D33 explicitly applies that D29 precondition. Thus both decisions in the last grouped reference belong to TKT-557. The already explicit TKT-557 D21 deviation reference is correct. These checks verify the cited source ownership; they do not establish current ticket status or scanner readiness.

The temporary `verify-namespaces.py` cannot prove the stated source preservation. Its `citations()` function recognizes an immediately preceding `TKT-n ` or a following `of TKT-n`, then defaults to 556. It does not recognize `TKT-399 history Dn` or inherit the source across `TKT-399 D29/D30`. Thus it assigns the same wrong 556 source to both sides and reports success. The committed `namespace-proof.tsv` booleans and the broad source-preservation statement in `results.md` exceed that evidence.

Required fix: give each historical/grouped reference its explicit real source. Give both decisions in the TKT-557 group their recorded TKT-557 source. Correct or limit the proof and its results claim. A proof should check a source mapping established from the full reference context; repeating a parser's incorrect default on both sides is not independent evidence. Keep decision identities, outcome rules and old anchors intact. This is a bounded documentation correction, not a request to change the scanner or weaken its tests.

## Previous findings and interactions

All four findings from my first revision report are corrected in this candidate:

- R1: the staff runbook now distinguishes shared Catalog/Inventory/Commerce/Access routes, Payments' separate token, Catalog channel reads and the Inventory allocation operations. This matches the source checks already recorded in revision 1.
- R2: the stable development H3 and exact safety statements are restored. The fixed subsection routes to the full scanner runbook and keeps Entry rejection instructions beside it. The unchanged test's path, section-end rule and required sentences now agree. Namespace qualification addresses the test's syntactic requirement; R5 concerns the separate semantic source check.
- R3: scheduled-workflow guidance now distinguishes local structure/YAML checks from GitHub runtime evaluation.
- R4: malformed staff-runbook punctuation is rewritten as complete sentences. Related learnings labels and legacy bullets are repaired without changing their lesson.

I also checked the other review's correction batch. AGENTS restores the owner-domain guidance and React requirement. Engineering rules restore the contingent-guard corollary and new-test scrutiny. ADR status rows now point to the named amendments. Historical-instance notes are back in the learnings index. The solution-design recovery link points to the runbook index. The load summary carries a dated correction note and preserves raw evidence. The new maintenance instructions search non-Markdown consumers and name the scanner test contract. These changes fit the authorized documentation scope.

## Checks and coverage

This is a fresh score of all ten metrics using the full-family structural coverage and source samples from my baseline and first revision review, plus a review of the 27 changed paths since revision 1. I read both committed first-revision reports and the fixed grid/results. All current tracked prose files received a new automated structural scan of path, size, line count, headings and opening/closing excerpts. The new ledger is `/tmp/ticketing-documentation-review/sol-revision-2-structural.json`.

Current inventory: 316 prose files, 291 Markdown and 25 text, totaling 3,159,083 bytes. The two added Markdown documents are the prior revision reports. No prior text document was removed. Every baseline supporting asset still matches its recorded hash, 318 of 318. The diff since revision 1 contains no application or test change; its only non-Markdown path is the namespace-proof TSV.

I inspected all 27 changed paths through complete small-file diffs, the reports, or normalized comparison of the long ADR files. For all ten ADR-085 files, stripping only the new namespace spelling and explicit heading aliases reproduces revision 1 exactly, except the separately rewritten citation-notation paragraph. I read that paragraph directly. This establishes that the outcome text was retained. It does not establish source ownership, which is why R5 required a separate contextual check. Raw grouped ADR diff output was truncated; I used full-file normalized comparisons and targeted citation excerpts instead of claiming those truncated diffs were fully read semantically.

New implementation excerpts inspected in this pass:

- `scripts/check-go-toolchain.sh`, including the minor-version convergence function and documented patch-level linter limit, against development's setup wording.
- `services/access/internal/consumer/seats.go:53` and `consumer.go:316`, confirming the additional order-seat read and committed ticket-switch callback in architecture.
- The unchanged `web/scanner/test/doc-namespace.test.ts`, as the stable document-consumer contract, against the restored development subsection and namespace rules.

The remaining source checks and full/partial-read limits from revision 1 remain applicable. There was no full semantic reread of the unchanged ADR, learning, workflow, product, spike, historical evidence and sprint corpus. I made no live board query. I read the relevant decision bodies in `/tmp/ticketing-documentation-review/TKT-399.json` and `TKT-557.json`, supplied by the parent from configured read-only board GETs. I did not read those full records as a semantic audit or verify current ticket status. No external URL fetch, browser run, security experiment, application mutation, contract generation or historical probe was performed. Binary assets were hash-checked, not visually reviewed.

Executed read-only checks in this pass:

- Markdown link script: PASS, 291 tracked Markdown files.
- Temporary local link/heading scan: 291 documents, 1,304 local references, no missing target or heading reported.
- Baseline asset hashes: 318 of 318 unchanged.
- Fixed-grid hash: unchanged.
- Structural inventory and ADR normalized-text comparisons: completed.

The parent reports the three original scanner document tests passing, the ADR-number check passing and a clean candidate. I did not run those tests or the full gate. The full `make check` result is pending at this report. Link/anchor scans use a slug approximation and do not establish GitHub renderer behavior, external references, or all non-Markdown consumer behavior.

## Design judgment

The task-specific documentation structure still resolves the baseline problem. The added non-Markdown search also addresses the first revision's missing document-consumer model. R5 exposes a narrower verification problem: token preservation and source-authority preservation are different checks. Fixing the contextual mapping and limiting the proof claim should close it without another structural rewrite. The actual safety rules should remain unchanged.
