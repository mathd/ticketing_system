# gpt-6.1-sol revision documentation evaluation

Reviewer: gpt-6.1-sol, high effort. Date: 2026-11-09.
Candidate: `771a5c1ae6bb6520425d6453ec11804c66400cd8`.
Application baseline: `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.
Fixed grid: `docs/documentation/evaluation.md`, SHA-256 `3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`. I verified that hash. I applied the unslop skill to this report.

This evaluates documentation, not application correctness, security, capacity, or venue readiness. Findings only. I changed no tracked file, ran no gate, and made no network call or external write. The parent reported its gate result during this review; that evidence is identified separately below.

## Scores

| ID | Metric | Score / 10 | Evidence |
|---|---|---:|---|
| M1 | Accuracy | 7 | Corrected toolchain, currency, PSP, generated-output and role descriptions agree with inspected source. A current staff runbook still misstates two credential scopes, finding R1. |
| M2 | Authority and currency | 8 | `docs/README.md`, the ADR status register, historical family indexes and known-limits register clearly distinguish requirements, delivery, proposals, snapshots and live board state. The accepted-but-unimplemented Resend requirement and proposed ADRs remain explicit. |
| M3 | Findability | 9 | Short task indexes now route to ADRs, operations, configuration, contracts, workflow and historical evidence. Service/frontend ownership has source entry points. |
| M4 | Task execution | 7 | Setup, data-preserving shutdown, credential prerequisites and scoped runbooks are useful. The candidate breaks the existing scanner documentation checks, confirmed by the parent's gate result and my read of the test contract, finding R2. |
| M5 | Coverage | 9 | All five services, gateway, three frontends, shared packages, HTTP outputs, domain events and documentation families have a discoverable reference. All 274 original text documents and 318 historical supporting assets remain. Known gaps have explicit labels. |
| M6 | Contract and safety precision | 7 | Money, tenant, event-schema, contention, scanner and integrity boundaries remain in required references. The staff credential summary still gives a materially wrong authorization boundary, finding R1. |
| M7 | Token efficiency | 9 | Measured entry reductions match the files. Binding engineering rules and long operational/ADR details remain available through task routes. Entry cost falls without deleting the detailed decision. |
| M8 | Clarity and structure | 8 | Routine guidance is shorter and easier to scan. A mechanical punctuation replacement damaged sentences in the staff runbook, finding R4, but the meaning is recoverable. |
| M9 | Reference integrity | 7 | Executed local checks pass for 289 Markdown files and 1,291 local references. However, the old scanner subsection is also an executable document consumer and was removed, finding R2. A Markdown-only pass does not cover that contract. |
| M10 | Maintainability and provenance | 8 | Maintenance names owners, update triggers, validation limits and review evidence. Baseline inventories, asset hashes and failed baseline scores remain visible. The executable document-consumer gap in R2 needs correction. |

Result: FAIL. M1, M4, M6 and M9 are below 8. No Critical or High safety finding was identified within this inspection. Pending final review bookkeeping is not a defect and did not reduce a score.

## Findings and required fixes

### R1. Medium: the current staff runbook still misstates credential scope

`docs/runbooks/staff-accounts.md:63–66` says `INTERNAL_SERVICE_TOKEN` "opens every service's internal surface" and calls `CATALOG_STAFF_WRITE_TOKEN` "catalog writes only". These are current procedure statements, not dated evidence.

Payments uses its own credential. `services/payments/cmd/payments/main.go:349` obtains `runtimecfg.PaymentsTokenFromEnv()` and passes that result as the API credential at line 424. `shared/go/runtimecfg/runtimecfg.go:289` requires `PAYMENTS_INTERNAL_TOKEN` and refuses equality with the shared token. `services/payments/internal/api/server.go:78` compares requests with that configured credential. Separately, `services/catalog/internal/api/server.go:440` allows the staff credential on the exact `GET /internal/channels` route. The corrected architecture and configuration pages already describe the separate Payments boundary.

Correct the paragraph to describe the shared credential's actual services and the separate Payments credential. Describe the Catalog credential's allowed channel-registry read as well as writes, or link a canonical scope description. Do not infer that sharing an environment variable means a service accepts it for authorization. This is a material accuracy and safety-precision defect. M1 and M6 remain capped at 7 until it is fixed.

### R2. Medium: document moves broke existing executable document contracts

The parent reported that `make check` failed on all three tests in `web/scanner/test/doc-namespace.test.ts`. I did not run that gate, but I fully read the test and confirmed the document mismatch in this candidate.

The helper at lines 7–14 reads `docs/development.md` and requires the literal H3 `Scanner Entry outcomes and local records`, ending at the next H2. That subsection no longer exists in the development entry file. The tests require the Entry time-bound statements and the terminal-write/page-loss warning there. Moving the text to `docs/runbooks/scanner.md` and preserving only the enclosing H2 is insufficient for this consumer. The second test also diffs `docs/adr/` against `ec336db814d719130cdefd263ca84be73b9f06e2` and requires each newly added decision identifier to have an `ADR-085 ` or `TKT-558 ` prefix. The moved detail files contain unqualified identifiers and therefore violate that existing contract even though their bodies were preserved byte for byte.

Restore a short compatible subsection at the old path with the required safety statements and a canonical runbook link. Preserve existing heading anchors. Qualify identifiers in moved ADR material while retaining their real owner and ticket provenance. Do not weaken or remove the tests to pass the reorganization. Re-run the document tests and final gate. M4 and M9 remain capped at 7 until the compatibility break is corrected.

### R3. Low: the scheduled-workflow runbook overstates what the local gate omits

`docs/runbooks/scheduled-workflows.md:87–90` correctly warns that local tests do not execute GitHub's own `if`, `needs`, permissions and matrix semantics. It then says that nothing in `make check` parses workflow YAML. That broader statement is false. `Makefile:33` runs the workflow-trigger guards. `scripts/check-security-workflow-trigger.sh` reads workflow structure and invokes `scripts/scan-allowlist.mjs wiring`, which parses YAML. The hermetic guard also inspects trigger/job/step structure.

Narrow the warning to the notifier and GitHub runtime semantics that remain unverified. Keep the real-run requirement. This is a local wording error; it does not mean the gate proves the notifier works. It contributes to M1's existing deficit but does not create another material score cap.

### R4. Low: a punctuation replacement damaged the staff runbook

Examples include `docs/runbooks/staff-accounts.md:32`, `:49` and `:55`: "Provision an account ,", "share them , accepted", and "fails , that". Other replacements in the same file produce comma splices or stray spaces. The moved-body diff shows these replaced the original sentence separators.

Rewrite the affected sentences in plain English. Do not use a global dash-to-comma replacement. This is a small clarity defect and does not cap M8 below 8.

## Preservation and factual checks

I read the original 35,156-byte AGENTS file and compared its rule families with the revised AGENTS, engineering rules and required ADR routes. I found no separate material loss of a binding rule in that extraction. The model/delegation authority was not broadened by the revised contribution guide. I checked the workflow's maintenance exception and autonomous gate sections against the new PR wording and `.claude/sdlc.config.json`.

I independently compared all nine ADR-085 moved bodies with the baseline. All matched after relative Markdown-link relocation only. I compared all 22 moved development sections after expected relative-link relocation. Twenty matched. Staff accounts contains the intended role corrections plus the punctuation changes. Scanner contains a required relocated sprint-evidence link. Those comparisons establish preservation, not correctness of every retained technical assertion. R2 shows why unchanged bodies also need checks against their executable consumers.

I enumerated all 55 base-Compose interpolation names and found every name in `configuration-reference.md`. I read the interpolation/default excerpts and compared the register with them. The ten Commerce worker settings and defaults match the settings table and parsing code in `services/commerce/cmd/commerce/main.go:270–319`; the register states that base Compose does not forward those overrides. All twelve generated contract output paths match `scripts/generate-api.sh`, five Go and seven TypeScript outputs. I did not regenerate contracts.

Selected source checks cover Go/pnpm/Node pins, Makefile gate order and startup/shutdown targets, shifted test-stack identity and ports, Payments provider and credential selection, Commerce EUR-only validation, staff route roles, Catalog's staff-read exception, and scanner deadline/status/actuation checkpoints. These are source inspections, not executed security guarantees.

I verified hashes for all 318 original supporting assets in `assets.tsv`; none changed. All 274 paths in the baseline text inventory still exist. Historical logs, probes, screenshots, report JSON/HTML and receipts were preserved rather than rerun or silently relabelled as current proof.

Measured bytes match the recorded results: AGENTS 35,156 to 6,777; development 142,936 to 5,596; learnings entry 56,248 to 13,860; ADR-085 entry 234,176 to 13,036; documentation map 2,054 to 3,136. Bytes divided by four remains an estimate, not a tokenizer measurement.

## Coverage and limits

The current tracked prose inventory is 314 files, 289 Markdown and 25 text, totaling 3,115,962 bytes. Every file received an automated structural scan of path, byte size, line count, headings and opening/closing excerpts. The ledger is `/tmp/ticketing-documentation-review/sol-revision-1-structural.json`. I reviewed the resulting file/title listing and ADR status excerpts. This covers instructions, workflow and references, product planning, all ADRs and split parts, conventions, operations, learnings, spikes, evidence text, sprint records and verification records. It is not a full semantic audit of 314 files.

Fully read current documents include AGENTS, CLAUDE, the documentation map, engineering rules, fixed grid, evaluation results, maintenance, configuration and override register, contracts, known limits, development, architecture, ADR index and ADR-085 entry, scanner README, PR guide, technical delivery standards, runbook index, staff accounts, demo, browser forms, cache controls, scheduled workflows, learnings index and legacy notes, roadmap, solution design, dependency policy, Docker guide, historical-record index, evidence index, verification index, sprint index and on-sale-load summary. I fully read the unslop skill and original AGENTS. Other documents, including testing, workflow contracts, old ADRs, remaining runbooks and historical reports, received structural or targeted partial reads. Some grouped tool output was truncated; I do not count unseen portions as full reads.

Implementation reads were targeted excerpts, not whole service audits. Two guessed source paths did not exist, `shared/go/runtimecfg/credentials.go` and `services/commerce/cmd/commerce/workers.go`; I corrected those searches to `runtimecfg.go` and Commerce `main.go` before drawing conclusions.

Executed read-only checks:

- `./scripts/check-markdown-links.sh`: PASS, 289 tracked Markdown files.
- Temporary link/heading scan: 289 documents, 1,291 local references, no reported missing target or heading.
- Baseline asset hash comparison: all 318 unchanged.
- Original text-path retention: all 274 present.
- Moved-body and register comparisons described above.

The temporary heading scan uses a slug approximation. It does not prove renderer behavior, external URL validity, bare code-path validity or executable document contracts. R2 was found through the test consumer, not that scan. No external links, board status, live GitHub account state, application, browser, security scenario or historical probe was exercised. Images were inventoried and hash-checked, not visually reviewed. The parent's gate failure is reported evidence, not a gate I executed. No later fix is included in these candidate scores.

## Design judgment

The revision addresses the baseline design problem. Rules, task routes and historical evidence now have distinct homes, and the measured entry cost is much lower. The remaining material findings concern incomplete credential-summary correction and compatibility with executable document consumers. They require a bounded documentation correction pass, not a new documentation architecture. Maintain one canonical credential description and treat tests that read prose as incoming references during later moves.
