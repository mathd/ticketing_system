# gpt-6.1-sol baseline documentation evaluation

Reviewer: gpt-6.1-sol, high effort. Date: 2026-11-09.
Scope: the documentation at the baseline captured in `baseline-inventory.json` and `baseline.tar`.
Grid: the parent's fixed `docs/documentation/evaluation.md`, M1 through M10. Scores evaluate documentation quality, not the application's security, correctness, or deployment readiness.

## Scores

| ID | Metric | Score / 10 | Evidence |
|---|---|---:|---|
| M1 | Accuracy | 6 | Current guidance contains obsolete back-office permissions, stale toolchain text, and a scanner source path that no longer exists. |
| M2 | Authority and currency | 6 | Accepted/proposed ADR statuses and dated proof records are good. Contribution guidance contradicts autonomous workflow gates; current indexes do not clearly separate all historical snapshots. |
| M3 | Findability | 5 | `docs/README.md` finds document families, but there is no ADR status/topic index or operations index. Recoveries occupy a 2,079-line development file. |
| M4 | Task execution | 7 | Recovery commands and refusal boundaries are detailed. Basic setup/toolchain information is stale; service operations require scanning one large file, and some historical statements appear as current operator instructions. |
| M5 | Coverage | 8 | Architecture covers all five services, gateway and three frontends. Product, contracts, configuration, testing, operations, security, workflow and evidence families exist. Coverage does not imply all implementation assertions were audited. |
| M6 | Contract and safety precision | 7 | ADRs and instructions state strong money, tenant, event-schema and adversary boundaries. Current scanner README omits the delivered outcome model and overstates that every scan mints an occurrence. Staff role guidance is obsolete. |
| M7 | Token efficiency | 3 | `AGENTS.md` is 35,156 bytes, development 142,936, LEARNINGS 56,248. The learnings "index" embeds long repeated lessons, and ADR-085 is 234,176 bytes. Routine tasks pay for unrelated history. |
| M8 | Clarity and structure | 6 | Precise technical content is mixed with long nested examples, amendments, repeated explanations and generic PR tutorials. Main runbook lacks a topic index. |
| M9 | Reference integrity | 8 | Existing local-link checker passes all 249 tracked Markdown files. An independent heading-anchor scan found 19 anchor links and no suspects. Bare inline-code references are outside those checks, and scanner's obsolete source path demonstrates that limit. |
| M10 | Maintainability and provenance | 6 | ADR status/date and proof commit identities are strong. No compact documentation ownership/update/check policy or corpus manifest exists. Repeated current-state summaries have drifted. |

Completion: FAIL. Eight metrics are below 8. No critical or high finding identified within this inspection. Medium findings must be corrected before the affected metrics can reach 8.

## Findings

1. **Medium, current authorization guidance is obsolete.** `docs/development.md:1009` says box_office and finance reach little beyond the venue list and logout because the order console is unbuilt. `web/backoffice/src/lib/authorization.ts` includes `/admin/orders` for admin and box_office. The same runbook says `--role` is interpreted nowhere until TKT-191, although the matrix now enforces it. Replace historical implementation promises with current route-role behavior and link the canonical matrix. M1, M2, M4 and M6.

2. **Medium, scanner entry documentation names removed code and omits delivered behavior.** `web/scanner/README.md` points to `src/occurrences.ts`, which does not exist. Current code uses occurrence-store/model/coordinator/owner modules, entry-scan and scan-result. The README's opening "Every scan mints" needs to distinguish scans stopped/refused before a request. `scan-result.ts` has stopped-before-send, mint-unconfirmed, accepted-too-late and Exit outcomes; ADR-085 describes the staged delivery and limits. Rewrite this README as a short current implementation map, retaining pending-before-send, own-row actuation, offline/reconciliation distinctions and irreducible storage limits. M1, M3, M6.

3. **Medium, workflow approval rules conflict.** `docs/conventions/pull-requests.md` says human approval is always required before merge. `docs/technical-delivery-standards.md` also implies owner/human gates always remain. The ticket skill explicitly replaces gates 2–4 in autonomous mode, and `.claude/sdlc.config.json` selects autonomous mode. State the applicable gate-mode rule and preserve owner-only priority and explicit request boundaries. Do not silently change the process to fit one document. M2 and M10.

4. **Medium, current setup information has drifted.** Root README and the development toolchain section say Go 1.26+, while `go.work` and CI select 1.27.3. Document the committed selected toolchain and any auto-download behavior distinctly. `docs/configuration.md` presents fixed smoke ports although stack-env derives shifted slots from checkout path and stack name. A future operator needs exported resolved ports, not a literal copied from the table. M1 and M4.

5. **Medium, essential context is too expensive.** Extract incident narratives from AGENTS into learnings, preserve concise binding rules, and add task-triggered references. Split development into service or operation runbooks. Turn LEARNINGS into the one-line topic index its opening promises. Add a short ADR index with status and reading triggers. Preserve old headings with stable redirects or update every local reference when moving text. M3, M7 and M8. This is the main design problem, not just a series of typos.

6. **Medium, historical and current state need a single reading rule.** `docs/demo-readiness.md` is an August assessment with "today" claims and old blockers. The product brief remains labelled draft for Gate 1 despite shipped milestones; the roadmap explains why PRD boxes are not live status, but the product entry pages do not. Sprint preparation says S-2 remains planned while the adjacent report/publication evidence records later work. Preserve dated records, mark snapshot scope at family entry points, and route current task/status questions to live code or the board. Proposed ADR-075/076/084 must remain visibly proposed; accepted ADR-078 explicitly remains unimplemented. M2 and M10.

7. **Medium, overview wording understates existing adapters and gate checks.** `docs/solution-design.md` says no real PSP is in scope, but payments selects an implemented Stripe test-mode adapter and refuses live keys. Say fake and Stripe test-mode exist and live PSP operations remain excluded. Technical-delivery-standards and testing summarize a gate that now includes scan-allowlist and Astro image-premise checks. Prefer one canonical gate description linked to `Makefile:check-all`. The same standards file broadly excludes SLOs although on-sale evidence already defines local NFR SLOs; distinguish production SLO commitments from local measured targets. M1, M2 and M10.

8. **Low, remove generic contribution tutorials.** PR conventions explain what a PR is, compare to dev branches the repo does not use, set generic review response expectations, and repeat a long general checklist. Keep the actual naming, scope, required proof, approval, and handoff rules. Maintain direct-maintenance exceptions without changing the user's explicit PR request. M7 and M8.

## Coverage and checks

Baseline inventory: 274 tracked text documents, 3,062,164 bytes. It includes 249 Markdown and 25 text files. All 274 received a structural scan of path, size, headings and family; targeted first/last excerpts covered the historical and evidence families. This is not a full semantic read of every line.

Full reads: 13 baseline documents. They are CLAUDE.md, README.md, docs/README.md, docs/ROADMAP.md, docs/solution-design.md, docs/technical-delivery-standards.md, docs/docker.md, docs/demo-readiness.md, docs/conventions/commits-and-branching.md, docs/conventions/continuous-delivery.md, docs/conventions/dependencies-and-versions.md, docs/conventions/pull-requests.md and web/scanner/README.md.

The remaining 261 documents were structurally scanned or read in selected sections, including AGENTS, all ADR headings/status excerpts, the 27 workflow documents, 89 learnings, product planning, spikes, evidence logs, sprint records, configuration and testing. Some large grouped reads were truncated by tool output limits. I do not count those as full reads. The per-file coverage ledger is `sol-baseline-coverage.json`.

The documentation assets inventory covers 561 tracked files under docs/. The 243 Markdown/text files there are part of the 274-text inventory. The remaining assets include 118 JSON, 103 logs, 26 JSONL, 17 PNG, a draw.io diagram, an HTML report and executable probe/verification material. Binary assets were not visually inspected, historical probes were not rerun, and report JSON/HTML fidelity was not revalidated. These limits apply to the scores.

Implementation cross-checks were selected excerpts from Makefile, go.work, .github/workflows/check.yaml, scripts/check-markdown-links.sh, .claude/sdlc.config.json, payments main/provider selection, scanner scan-result and source-file enumeration, and back-office authorization.ts. No whole-application code audit, live board lookup, external link fetch, gate run, browser run or application mutation was performed.

Executed checks:

- `./scripts/check-markdown-links.sh`: PASS, 249 tracked Markdown files checked.
- Temporary independent anchor scan over inventory documents: 19 Markdown links with fragments, zero suspects. This uses a simple heading slug heuristic, not GitHub's renderer. It does not check inline-code paths or external URLs. Saved as `sol-anchor-scan.json`.
- Corpus/asset enumeration through `git ls-files`: completed. No tracked file changed by this review.

## Recommended revision strategy

Use a short always-loaded agreement, task routing, an ADR status index, and operations files. Define authority once, then link to it. Preserve accepted decision semantics and append-only historical evidence; shorten how readers reach them. Verify changed factual claims against the code at their decision point. Run local-link checks plus moved-anchor checks, measure the entry-path byte/token change, then request new independent reviews against the same fixed grid. No score should rise solely because a historical family was excluded from the corpus.
