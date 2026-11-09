# Opus 5.5 baseline documentation review

Reviewer: Opus 5.5 (claude-opus-5-5), review sub-agent. Findings only. I edited no tracked file,
made no commit, started no subagent and changed nothing on the network.

Date: 2026-11-09. Grid: `docs/documentation/evaluation.md`
(sha256 `3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`, untracked, read before
review). Baseline: HEAD `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.

I compared the 274 inventory entries with `baseline.tar` and with the working tree. All 274 sha256
values match both. This review describes that baseline.

## Scores

| ID | Metric | Score | Main reason |
|---|---|---|---|
| M1 | Accuracy | 6 | Several material current-state claims contradict code (back-office roles, scanner storage module, toolchain versions). Many sampled claims are correct. |
| M2 | Authority and currency | 5 | ADR status lines are good. Dated snapshots read as current, ADR-085 status lags delivery, backlinks are missing, and AGENTS.md mixes binding rules with narrative. |
| M3 | Findability | 4 | No ADR index for 84 ADRs (1.58 MB). Most runbooks are inside one 143 KB `development.md`. The learnings "index" is 56 KB. Several assets have no inbound link. |
| M4 | Task execution | 6 | Gate, lock, verdict, ports and most runbooks give exact commands and outputs. Host prerequisites are wrong, and the demo bootstrap is incomplete. |
| M5 | Coverage | 5 | No contract map (5 specs, 12 generated outputs). 26 of 55 Compose variables are not in `configuration.md`, and 8 are in no current doc. Hooks are not documented. No known-gaps list. |
| M6 | Contract and safety precision | 6 | Strong adversary and limit wording in ADR-021, `configuration.md` and `security.md`. One stale authorization statement, no current-state note that Commerce sells EUR only, and a scanner README without Exit or time rules. |
| M7 | Token efficiency | 3 | Mandatory `AGENTS.md` is 35,156 bytes. About 70 percent of it is narrative that is repeated in `LEARNINGS.md` and `docs/learnings/`. ADR-085 alone is 234 KB. |
| M8 | Clarity and structure | 5 | Entry pages are clear. AGENTS.md and the runbooks use very long bullets, 4,057 em dashes in total and heavy bold. `pull-requests.md` holds generic tutorial text. |
| M9 | Reference integrity | 7 | The file-link gate passes (249 Markdown files) and anchors resolve. Some current docs cite deleted code paths. One evidence file does not match the README that cites it. Some assets are orphans. |
| M10 | Maintainability and provenance | 6 | Evidence provenance is very good (dates, commits, executed or not). There is no update-ownership map or invalidation trigger per document, and one make target overwrites tracked evidence. |

No metric reaches 8. No critical or high safety finding. The highest-severity item is M1/M6-1
(medium): a runbook says back-office roles are not enforced when they are.

## Inspection scope and limits

Counts are against the 274 inventory documents.

- Fully read: 17. `AGENTS.md`, `CLAUDE.md`, `README.md`, `docs/README.md`, `docs/ROADMAP.md`,
  `docs/architecture.md`, `docs/solution-design.md`, `docs/technical-delivery-standards.md`,
  `docs/testing.md`, `docs/docker.md`, `docs/configuration.md`, `docs/demo-readiness.md`,
  `web/scanner/README.md`, `docs/conventions/commits-and-branching.md`,
  `docs/conventions/continuous-delivery.md`, `docs/conventions/security.md`,
  `docs/conventions/dependencies-and-versions.md`.
- Read in part (whole sections, not the full file): 24. `docs/development.md` (all headings, lines
  1-78, 1001-1040, 2073-2079), `docs/LEARNINGS.md` (headings, lines 1-40 and 600-638),
  `docs/conventions/pull-requests.md` (most of it), `docs/product/brief.md`,
  `docs/product/prd-v1.md` (headings, lines 1-20), `docs/product/backlog-decisions-2026-10-10.md`
  (lines 1-25), ADR-004, ADR-008, ADR-085 (status, context, grep of delivery lines), the three
  `SKILL.md` files (front matter, headings, selected lines), `local-tracker.md` (lines 1-30),
  `docs/sprints/S-2/preparation.md`, `start-prompt.md`, `report.en.json` (top-level fields),
  `docs/verification/on-sale-load/README.md` (lines 1-30), the checkout-dashboard README head,
  `docs/sdlc-vault-migration-plan.md` head, and three spike heads.
- Scanned (all 274): the inventory heading lists for every file, plus a script over the baseline tar
  that read the first 20 lines of every file and classified date, ticket, status and commit
  markers. I also printed the status line of all 84 ADRs, the heads of 15 learnings (every sixth
  file) and the heads of 12 evidence or verification files.
- Code-checked: 34 claims (list in "Claims checked against code"). The skill test suites ran:
  59 + 55 + 82 tests, all OK. `scripts/check-markdown-links.sh` and `scripts/check-adr-numbers.sh`
  ran and passed. I ran my own anchor check and a backtick-path existence check.
- Supporting assets: inventoried by `git ls-files`, not opened as binary. About 400 non-Markdown
  files under `docs/`, `.claude/` and the generated contracts (list in "Supporting assets").
- Not done: no full read of the 85 ADR files, 89 learnings, 27 skill files or 30 evidence files.
  I did not run `make check`, `make up` or `make browser`. I did not read the board, so ticket
  states (for example TKT-518, TKT-560) are not verified. Most ADR implementation claims are not
  verified. Scores are my judgment within this coverage.

## Corpus size

Total 274 documents, 3,062,164 bytes, 40,453 lines.

| Family | Files | Bytes |
|---|---|---|
| `docs/adr` | 85 | 1,583,943 |
| `.claude/skills` | 27 | 326,430 |
| `docs/learnings` | 89 | 303,284 |
| `docs/evidence` | 30 | 244,637 |
| `docs/development.md` | 1 | 142,936 |
| `docs/spikes` | 3 | 77,842 |
| `docs/product` | 3 | 73,063 |
| `docs/sprints` | 14 | 61,686 |
| `docs/LEARNINGS.md` | 1 | 56,248 |
| `AGENTS.md` | 1 | 35,156 |
| `docs/conventions` | 5 | 34,192 |
| `docs/verification` | 2 | 33,988 |
| other top-level docs and scanner README | 12 | ~88,700 |

Largest single files: ADR-085 (234,176 bytes, 948 lines), `development.md` (142,936), ADR-016
(74,680), `sdlc-ticket/SKILL.md` (65,275), ADR-084 (56,803), `LEARNINGS.md` (56,248).

## Findings by metric

Severity: H high, M medium, L low. "Verified" means I read the code or config named.

### M1 Accuracy (6)

1. M. `docs/development.md:1009-1037` (Back-office sign-in) says `box_office` and `finance` "reach
   the venue list and sign-out and little else today, because the surfaces they exist for (order
   console TKT-193/194, settlement TKT-23) are not built", and that "`--role` is stored but
   interpreted nowhere until TKT-191". Verified false for the order console:
   `web/backoffice/src/pages/orders.astro` exists and `web/backoffice/src/lib/authorization.ts:102`
   grants `/admin/orders` to `['admin', 'box_office']`. Roles are read by the route table
   (`authorization.ts:246`). `ROADMAP.md` and `architecture.md` also say order lookup and refund
   exist. The settlement part may still be true (not checked).
2. M. `web/scanner/README.md:5` names `src/occurrences.ts` as the occurrence queue. The file was
   deleted in `deec4ea3` (TKT-558). Storage is now split across `occurrence-store.ts`,
   `occurrence-model.ts`, `occurrence-coordinator.ts` and others. The README does not mention
   ADR-085, Exit direction (TKT-399), the time contract (TKT-559) or Entry refusal instructions
   (TKT-552, TKT-565). It describes the pre-ADR-085 model as current.
3. M. Toolchain. `README.md:33` says "Go 1.26+, Node 24+ (pnpm via corepack)" and
   `docs/development.md:5-6` says the same. Verified: `go.work` and all eight `go.mod` files say
   `go 1.27.3`; CI uses Go 1.27.3 and Node 24.18.0; the container images use Node 26.5.x and install
   pnpm with npm because Node 25+ has no corepack (`web/*/Dockerfile:2-10`). A host on Node 25+
   following the README will fail at `corepack`. `dependencies-and-versions.md` is correct for Go
   but says "TypeScript 6"; the three apps pin `~7.0.2` and only the root pins `~6.0.2`.
4. M. `docs/architecture.md` diagram shows no synchronous service-to-service HTTP call. Verified
   calls that exist: Commerce to Payments (`configuration.md` "Payments has its own internal
   credential", `services/commerce/cmd/commerce/main.go:717-726`), Commerce to Access (`ACCESS_URL`,
   refund voiding), Access to Catalog (`CATALOG_URL`, `services/access/cmd/access/main.go`). The
   text mentions Access reading buyer email from Commerce. A reader of the diagram sees only bus
   edges and gateway edges.
5. L. `docs/testing.md:3-6` lists the gate stages. `Makefile:33` also runs
   `check-scan-allowlist`, `check-check-workflow-trigger` and `check-astro-image-premise`, which the
   list omits.
6. L. `docs/development.md:40` says unit tests are "kept minimal at the scaffold layer (shared
   middleware)". The tree has large unit suites in every service and app (for example 40+ test
   files in `web/scanner/src`).
7. L. `docs/demo-readiness.md` Blocker 2 says the staff bootstrap "is not written down anywhere a
   presenter would find it". `docs/development.md:1026-1037` now has it. Blocker 1 is still true:
   no demo seed exists (no target in `Makefile`, catalog migrations have no demo seed).
8. L. ADR-085 status (lines 7-14) says time behavior "remains in ticket 3" and Exit is to be
   delivered. `3752a96a` (TKT-559) and `371e44b3` (TKT-399) are on HEAD, and the S-2 report says all
   four functional members are Done. Body lines 228, 276 and 368 are partly updated.
9. L. `docs/configuration.md:3` says all configuration is environment variables, then documents a
   subset (see M5-2). It does not say the table is partial.
10. L. `README.md:44` and `architecture.md` list the `shared/go` packages and say "additions require
    an ADR". The list omits `cmdline`, `contract`, `fakepsp`, `httpx`, `organizerassertion` and
    `runtimecfg`.

Correct claims I verified are listed under "Claims checked against code". They are the reason the
score is 6 and not lower.

### M2 Authority and currency (5)

1. M. `AGENTS.md` mixes binding rules (escalate a `[high]` finding, money rule, gate lock, ruleset)
   with long narrative lessons in the same bullet format. A reader cannot tell which sentences are
   requirements and which are history. Example: the "error mapper matching on a STRUCTURAL
   interface" bullet is about 600 words in one list item.
2. M. Dated snapshots that read as current. `docs/README.md` calls `demo-readiness.md` "what a demo
   can and cannot show today", but it is a 2026-08-08 assessment at `4c995e8`.
   `docs/sprints/S-2/preparation.md:3` says "S-2 remains planned" with no note that the sprint ran
   and `report.en.json` exists (state `active`, close approval `pending`).
   `docs/product/brief.md:3` says "Status: draft for Gate 1", and its non-goal "Real payment
   provider ... real adapter is a later ticket" sits beside the Stripe test-mode adapter (ADR-032).
3. M. Requirement versus implementation for money. The PRD and brief require one currency per event
   and many across the platform. Verified: Commerce refuses any non-EUR price
   (`services/commerce/internal/api/catalog_pricing.go:265`, `api/server.go:2099`). No current-state
   document (`architecture.md`, `ROADMAP.md`, `solution-design.md`, `demo-readiness.md`) says so.
   Only `AGENTS.md` and `LEARNINGS.md` mention it, inside test lessons.
4. L. Missing backlinks. ADR-052 says it amends ADR-049; ADR-049 does not mention ADR-052. ADR-004
   (the TTL-tier decision) does not link ADR-044, ADR-045 or ADR-079, which implement or changed its
   tiers.
5. L. ADR-073 is missing from main. It exists only on the unmerged branch
   `TKT-336-runtime-review-fixes` (`9f7e5fe9`). Nothing in `docs/adr/` says the number is reserved.
6. L. Conflicting branch-update guidance. `pull-requests.md` and `sdlc-ticket/SKILL.md:248,403`
   say rebase when behind. The owner memory for this project says merge `origin/main` and never
   rebase plus force-push. This is an owner decision, not a doc fix.
7. L. Commit convention versus practice. `commits-and-branching.md` requires `type(scope): ...`.
   In the last 100 commits on HEAD, 7 use that form and 77 start with `TKT-<n>`.

Strengths: every ADR has a status line with ticket and gate; ADR-075/076/084 say "Proposed" and
what is not active; ADR-079 says "Superseded"; ADR-008 states partial supersession by ADR-022;
`backlog-decisions-2026-10-10.md` says it is planning, not shipped behavior;
`sdlc-vault-migration-plan.md` has a historical banner.

### M3 Findability (4)

1. M. No ADR index. `docs/README.md` links the `adr/` directory only. 84 ADRs, 1.58 MB. AGENTS.md
   routes six topics (ADR-017 to 022, ADR-029). Every other topic needs a filename search or a
   full-corpus load.
2. M. `docs/development.md` is 2,079 lines with about 35 operator runbooks (quarantine, parked
   orders, lifecycle trail, journal key rotation, NATS ACLs, customer accounts, scanner outcomes,
   workflow failures) under the title "Development". There is no runbook index. `docs/README.md`
   describes it as "local workflow and runtime".
3. M. `docs/LEARNINGS.md` says it is "the index, one line each", but most entries are 5 to 15 lines
   and lines 303-638 are a dated appendix with full paragraphs. Three files are not indexed:
   `TKT-84-lifecycle-migration-measurement.md`, `TKT-85-...`, `TKT-87-...`.
4. L. `docs/README.md` routes by document type, not by task. It does not link `docs/sprints/` or
   `docs/diagrams/`.
5. L. Assets with no inbound Markdown link: `docs/diagrams/` (one drawio, four PNG previews, about
   2.2 MB), `docs/evidence/TKT-25/` (three PNGs), `docs/evidence/TKT-279/README.md`,
   `docs/product/tkt-102-seat-map-authoring.png`, `docs/product/tkt-53-festival-storefront.png`.

### M4 Task execution (6)

Strengths: `AGENTS.md` and `Makefile` define the gate wrapper, lock, verdict file and the wait loop.
`docker.md` gives the port-slot formula, and it matches `scripts/stack-env.sh:34-36`. Runbooks in
`development.md` give exact commands with sample output (for example `reconcile-pins`,
`list-parked`). All ten `docker compose exec <svc> /app <cmd>` commands in docs exist as
subcommands (verified). `testing.md` gives `make browser` host prerequisites (Chrome, `zbarimg`).

1. M. Wrong host prerequisites (M1-3). The first command a new reader runs can fail.
2. M. No demo bootstrap. `demo-readiness.md` lists the gap, but there is no seed and no single
   procedure for a demo stack. A reader must combine `make up`, the staff provisioning command and
   manual catalog and inventory setup.
3. L. `configuration.md` gaps (M5-2) mean a reader who must tune workers or retention cannot find
   the variable.
4. L. `docs/README.md` has no "first hour" path (prerequisites, `make deps`, `make up`, `make check`,
   expected duration, common failures such as Docker Desktop stopped or WSL2 port forwarding).
   The pieces exist in three files.

### M5 Coverage (5)

1. M. Contracts. Five specs (`services/*/api/openapi.yaml`) produce twelve generated outputs
   (`./scripts/generate-api.sh outputs`: five `openapi_gen.go`, seven `*-api-types.gen.ts` across
   storefront, back office and scanner). `solution-design.md` mentions only "Storefront types". No
   document maps spec to consumers.
2. M. Configuration. 55 variables appear in `compose.yaml`. 26 are absent from `configuration.md`.
   Of those, 8 are in no current doc (only some ADRs): `ACCESS_FEED_CURSOR_KEY`,
   `ACCESS_LIFECYCLE_FAILURE_THRESHOLD`, `ACCESS_STAFF_WRITE_TOKEN`, `HOLD_TTL`,
   `PAYMENTS_STATUS_REPLAY_RETENTION`, `STRIPE_SECRET_KEY`, and the Commerce worker knobs
   `OUTBOX_DRAIN_*`, `RECOVERY_*` (`services/commerce/cmd/commerce/main.go:285-289`).
   `CANCELLATION_REFUND_*`, the NATS passwords, staff write tokens and assertion keys appear only in
   `development.md`.
3. M. `.claude/hooks/` (Bash guard: gate lock, merge-head check, attribution refusal; Edit guard:
   gate lock) is documented in no Markdown file. Only `Makefile:118-129` comments describe it.
4. L. Only the scanner has a README. Gateway, services, storefront and back office have none. That
   is acceptable if `architecture.md` is the owner, but the doc should say so.
5. L. No "known gaps" list. Gaps are spread across `security.md` ("Not yet production-ready"),
   `ROADMAP.md` ("Later"), ADRs and evidence files.
6. L. `docker.md` documents only `compose.yaml`. The overlays `compose.direct-ports.yaml`,
   `compose.smoke.yaml` and `compose.onsale-load.yaml` are named only in `configuration.md` and
   `testing.md`. `compose.smoke-cadence.yaml` is named in no tracked Markdown file.

### M6 Contract and safety precision (6)

Strengths, verified: `configuration.md` states the Access write-timeout floor of 30s and says that
equality does not close the race (`gateway/cmd/gateway/main.go:196`,
`services/access/cmd/access/main.go:50`). It marks the 500 ms pre-decision bounds PROVISIONAL and
says nothing enforces the TKT-560 order. It states the Access `sslmode=disable` rule as
Compose-only (`services/access/cmd/access/database_config.go`). The Commerce four-credential
distinctness claim matches `credentialsAreDistinct`. `security.md` calls the Payments credential a
reduction, not a solution, and limits rate limiters to one replica. AGENTS.md and ADR-021 name the
adversary for the lifecycle chain and keep the rollback gap open. The live Stripe key refusal is
real (`services/payments/cmd/payments/main.go:211`).

1. M. Authorization understated (M1-1). A reader who trusts `development.md` may widen a route or
   skip a role check, believing roles are not used.
2. M. Money boundary not in current-state docs (M2-3). The cross-cutting invariant says "integer
   minor units + ISO currency", which implies multi-currency support that Commerce refuses.
3. M. Scanner safety rules not in its README (M1-2). The README is the natural entry point for gate
   behavior. It does not state the 3 s and 30 s limits, Exit direction or the refusal instructions
   that ADR-085 and `development.md:1562-1936` define.
4. L. `architecture.md` does not show which credential protects each internal call (M1-4), so the
   least-privilege story in `security.md` cannot be checked against the diagram.

### M7 Token efficiency (3)

Measured mandatory context: `CLAUDE.md` (34 bytes) loads `AGENTS.md` (35,156 bytes, 5,297 words,
about 9k tokens) every session. A typical ticket run adds `sdlc-ticket/SKILL.md` (65,275),
`local-tracker.md` (16,024) and `reviews.md` (28,734): about 145 KB, about 36k tokens, before any
product file.

1. H for M7 (not safety). About 25 KB of `AGENTS.md` is test and review epistemology (mutation
   readings, fixture rules, design-space claims). It is narrative with ticket history, and most of
   it is repeated in `LEARNINGS.md` and in 20 linked learning files. It matters only when a reader
   writes tests or reviews.
2. M. ADR-085 is 234 KB (about 58k tokens). Any scanner task must load it. Status, decision table
   and delivery state are not separated from baseline evidence tables (E1-E11) and defect maps.
3. M. `development.md` (143 KB) forces a reader to load all runbooks to find one.
4. M. `LEARNINGS.md` (56 KB) duplicates the learning files instead of indexing them.
5. L. `sdlc-ticket/SKILL.md` (65 KB) repeats long incident narratives inside procedure steps (for
   example the claim step at lines 271-286 cites four tickets inline).

### M8 Clarity and structure (5)

1. M. Long, dense bullets in `AGENTS.md`, `LEARNINGS.md` and `sdlc-ticket/SKILL.md`. Many sentences
   need re-reading. Em dashes: 4,057 across tracked Markdown (AGENTS.md 84, development.md 184,
   LEARNINGS.md 166, sdlc-ticket SKILL 162). Bold spans: AGENTS.md 78, development.md 296.
2. M. `docs/conventions/pull-requests.md` is generic tutorial text ("What is a Pull Request?",
   "typically into `main` or `dev`" although no `dev` branch exists, "colleagues", a 24-48 hour
   response target, a Google source link). It does not describe this repo's agent review flow.
3. L. Title case headings: `solution-design.md` "Solution Design",
   `technical-delivery-standards.md`, "Commits & Branching", "Continuous Delivery",
   "Pull Requests & Code Review", "Docker & Compose", "Dependencies & versions".
4. L. Term drift: "back office", "back-office", "backoffice"; "ai-review S8" style references with
   no definition of the ai-review series.
5. L. `docker.md` cites ADR-006 for the back office; README and architecture cite ADR-042.

Strengths: `architecture.md`, `configuration.md`, `security.md` and the evidence READMEs use plain,
scoped statements with explicit limits.

### M9 Reference integrity (7)

1. Gate: `scripts/check-markdown-links.sh` passed, 249 tracked Markdown files. It resolves files
   only, not anchors (stated in the script, not in docs). `check-adr-numbers.sh` passed, 84 ADRs.
2. My anchor check: 19 intra-repo anchor links, 0 broken (one apparent miss in ADR-083 was my slug
   bug for underscores). "§ Section" prose references: 55 found, the unmatched ones are regex
   noise; no real broken section reference found.
3. M. Stale code paths in current docs: `web/scanner/README.md:5` (`src/occurrences.ts`),
   `ADR-004:126,195,384` (`web/backoffice/src/lib/api.ts`, file no longer exists),
   `ADR-050:61` (`services/commerce/internal/mail`, now `mailer`). ADR-085 cites
   `occurrences.ts` too, but it declares those citations as pre-core baseline, which is acceptable.
4. M. `docs/verification/on-sale-load/README.md:8-17` cites git `4c6f2ec` and claim_history stats
   (mean 0.047 ms, total 1,261 ms, max 0.325). The cited raw file
   `docs/evidence/TKT-82/full-profile.json` says `git_sha ed2ed47a`, mean 0.0455, total 1,227.6,
   max 0.90. The ceiling numbers (300 and 600 per second, p50 4.6 s, p99 13.0 s, 398 ok/s, NFR p99
   8.6 ms) do match. Both files came in `e6d637d6` and `9254e057` on 2026-07-17.
5. L. `docs/reviews/` is cited by ADR-033, ADR-080 and
   `learnings/2026-07-30-a-gitignore-rule-can-outlive-its-reason.md`. ADR-033 and ADR-080 say the
   records were removed in `e3744ccd`. The learning (lines 9, 30, 89) does not.
6. L. Orphan assets (M3-5).

### M10 Maintainability and provenance (6)

Strengths: evidence files state date, base commit, who ran what and what was not executed
(`TKT-279/README.md`, `TKT-348/results.md`, `TKT-374/README.md` with `sources.json` hashes,
`TKT-380`). The S-2 report carries packet and scope digests. ADR status lines record gates.
`LEARNINGS.md` defines promotion into AGENTS.md or an ADR. Link and ADR-number checks run in the
gate.

1. M. No update-ownership map. Nothing says which document owns which fact (env variables, routes,
   service edges, contracts, runbooks) or what change invalidates it. "Correct documentation in the
   same change" (`technical-delivery-standards.md`) has no target list.
2. M. `make onsale-load-full` writes to the tracked `docs/evidence/TKT-82/full-profile.json`
   (`Makefile:242-247`). Any rerun replaces the evidence that the README cites, and the two already
   disagree (M9-4). This is a Makefile behavior; fixing it is outside a documentation-only pass, so
   record it as an owner decision or document the overwrite.
3. L. `LEARNINGS.md:6` says "Re-validated against the tree on 2026-08-03". Entries run to
   2026-10-18.
4. L. Verification folders `catalog-publication`, `checkout`, `inventory-holds`,
   `series-seasons`, `ticket-delivery` hold PNGs only, with no date, commit or method beside them.

## Design-level observation

The findings point to a design problem, not only local defects. The corpus grew by appending
incident narrative to the files every session must load (AGENTS.md, SKILL.md, LEARNINGS.md,
development.md, one very large ADR). Current-state documents (architecture, configuration, scanner
README, ROADMAP) have no owner trigger, so they drift while the narrative files grow. The fix is a
layered structure: a short binding rule set, task indexes, one owner document per fact type with
an invalidation trigger, and history kept in learnings, ADRs and evidence where it already lives.

## Prioritized changes to reach 8 on each metric

Order is by score impact per token of mandatory context. Each item keeps requirements, history and
evidence. Where I say "move", the text moves with a link back; nothing is deleted.

1. M7, M2, M8. Split `AGENTS.md` into a binding core and a topic file.
   - Keep in `AGENTS.md` only: context and goal (short), the `[high]` escalation rule, stack, money
     rule, gate wrapper and lock rules, browser-submit rule, ruleset rule, specs-before-code,
     record decisions, docs location, board location, and the ADR "read first" routes.
     Target 8 KB or less. Measure before (35,156 bytes) and after.
   - Move each testing and review lesson to `docs/conventions/testing-and-review-rules.md` as a
     one-line rule plus link to its existing learning file. Keep every rule. Add one AGENTS.md line:
     "Before you write or review a test, read testing-and-review-rules.md".
   - Mark rules as MUST and history as context, so authority is visible.
2. M3, M7. Add `docs/adr/README.md`: number, title, status, amended-by or superseded-by, and one
   "read when" line per ADR. Note the ADR-073 reservation. Consider a gate check that every ADR
   file has an index row.
3. M3, M7, M4. Split `docs/development.md` into `docs/operations/<topic>.md` (one per runbook) plus
   `docs/operations/README.md` index. Keep `development.md` for toolchain, loop and testing model.
   Keep a moved-section table so "development.md § X" citations in ADRs still resolve, or update
   the citations.
4. M1, M6. Fix `development.md` § Back-office sign-in: order console exists for `admin` and
   `box_office`, roles are enforced through `authorization.ts`, remove "interpreted nowhere until
   TKT-191". Check the settlement statement before keeping it.
5. M1, M4. Fix prerequisites in `README.md`, `development.md` and `dependencies-and-versions.md`:
   Go 1.27.3; Node 24.18.0 on the host with corepack, or Node 25+ with `npm i -g pnpm@11.12.0`;
   images use Node 26.5.x; TypeScript 7.0.2 in apps and 6.0.2 at root (with the reason).
6. M1, M6. Rewrite `web/scanner/README.md` as a short pointer page: current modules, ADR-085 as the
   current model, Entry and Exit direction, the 3 s and 30 s limits, refusal instructions, and links
   to `development.md` § Scanner Entry outcomes. Keep the ADR-025 lost-admission and version-skew
   text, marked with the ADR it comes from.
7. M2. Update the ADR-085 status block to say TKT-559 and TKT-399 delivered, with commits, and that
   TKT-560 still qualifies the bounds. Add "Amended by ADR-052" to ADR-049 and "Implemented or
   changed by ADR-044, ADR-045, ADR-079" to ADR-004.
8. M2, M10. Add a dated-snapshot banner to `demo-readiness.md` (state which blockers are still true
   at the review commit), to `docs/sprints/S-2/preparation.md` (superseded by the report; close
   pending) and to `brief.md` (status). Add `docs/sprints/README.md` with each sprint's state and
   canonical file.
9. M2, M6. Add the EUR-only Commerce limitation to `architecture.md` cross-cutting invariants and to
   the ROADMAP pricing line, with TKT-10 as the open requirement.
10. M5. Complete `configuration.md` from `compose.yaml` (26 missing variables), or say which section
    of which runbook owns each one. Add a small gate check that every `${VAR}` in `compose.yaml`
    appears in `configuration.md`, like `check-required-env`.
11. M5. Add a contract map (in `solution-design.md` or `docs/contracts.md`): spec, generator, each
    generated output, consumers, and `make generate` and `check-generate` behavior, including the
    "commit regenerated code before the gate" rule.
12. M1, M5, M6. Add synchronous HTTP edges and their credentials to `architecture.md`. I verified
    Commerce to Payments, Commerce to Access and Access to Catalog. Verify the others (Commerce to
    Catalog and Inventory, Access to Commerce, back office to Access) before drawing them.
13. M5. Document `.claude/hooks/` in one AGENTS.md line and one paragraph in
    `technical-delivery-standards.md`: what each guard refuses and that `check-hooks` tests them.
    State that no hook refuses `--admin` (TKT-518).
14. M8, M2. Replace `pull-requests.md` generic content with the repo's actual flow (PR from the
    ticket branch, review rounds, review guide, required `check`). Owner decision: align
    `commits-and-branching.md` with the `TKT-<n>` practice, or the practice with the doc. Owner
    decision: rebase or merge when behind. Use sentence case headings.
15. M9. Fix the three stale code paths. Add a note to the gitignore learning that `docs/reviews/`
    lives in git history (`e3744ccd`), as ADR-033 and ADR-080 already say. Record in the on-sale README that the
    raw JSON is from `ed2ed47a` and that the claim_history figures differ, or restore the matching
    file from history if one exists. Link or index the orphan assets. Either add GitHub-slug anchor
    checking to the link script or state the exclusion in `docs/README.md`.
16. M10. Add a "Maintaining these docs" section to `docs/README.md`: owner file per fact type,
    invalidation triggers (compose env change updates `configuration.md`; new service client
    updates `architecture.md`; scanner storage change updates the scanner README; new ADR updates
    the ADR index), and the recheck commands. Note that `make onsale-load-full` overwrites tracked
    evidence, or ask the owner to change its default path.
17. M3, M7. Make `LEARNINGS.md` a true one-line index. Move the dated appendix paragraphs into their
    learning files where they are not already there (check each), index the three TKT-84/85/87
    measurement files, and update the re-validation date or remove the claim.
18. M7. Split ADR-085 into the decision (status, decision, consequences, delivery state) and an
    evidence annex file for the baseline tables and defect maps. Keep the text; link both ways.

## Claims checked against code

Each item is correct unless the finding above says otherwise.

1. Go directive 1.27.3 in `go.work` and eight modules. README and development say 1.26+ (wrong).
2. CI Node 24.18.0 with corepack; images Node 26.5.x with npm-installed pnpm 11.12.0.
3. `packageManager` pnpm@11.12.0 in all four manifests.
4. TypeScript 7.0.2 in apps, 6.0.2 at root (deps doc says 6).
5. `Makefile` `check-all` stage list versus `testing.md` (three stages missing from doc).
6. `make check` wraps `scripts/gate.sh`; `check-all` refuses without the lock token.
7. `check-generate` diffs generated outputs against HEAD (AGENTS.md claim).
8. `check-required-env`, `browser`, `onsale-load-full`, `smoke-hermetic` targets exist as documented.
9. Port slot formula in `docker.md` matches `scripts/stack-env.sh:34-36`.
10. Image tags and digests in `docker.md` match `compose.yaml`.
11. HTTP timeout defaults 5s, 15s, 30s, 60s (`shared/go/runtimecfg/runtimecfg.go:25-37`).
12. DB pool defaults 25, 10, 30m, 5m (`runtimecfg.go:72-87`).
13. `OPENAPI_RESPONSE_VALIDATION_ENABLED` default true (`runtimecfg.go:316`).
14. Refund and exchange reversal worker defaults 1m and 16 (`commerce/cmd/commerce/main.go`).
15. Commerce refuses equal credentials among its four (`credentialsAreDistinct`).
16. Access and gateway 30s write-timeout floor (`access main.go:50`, `gateway main.go:196`).
17. Access `sslmode=disable` requirement (`access/cmd/access/database_config.go`).
18. Payments refuses `sk_live_` keys (`payments main.go:211`).
19. Back-office role table and order console (`authorization.ts:67-102`, `pages/orders.astro`).
20. Scanner `src/occurrences.ts` deleted in `deec4ea3`.
21. Ten `docker compose exec <svc> /app <cmd>` commands in docs exist in each service's `cmd`.
22. ADR-029 family advisory lock (`catalog/internal/store/postgres_seat_maps.go:137,366`).
23. Access lifecycle writes go through `appendLifecycle` (`access/internal/store/postgres.go`).
24. Smoke runs `verify-journal` and `verify-lifecycle` (`scripts/smoke.sh`).
25. Drain barrier uses `IS DISTINCT FROM 'idle'` and `backend_type='client backend'` (`scripts/smoke.sh:520-542`).
26. No hook refuses `gh pr merge --admin` (`.claude/hooks/guard-bash.py`), as AGENTS.md says.
27. `.github/rulesets/main.json` exists; three workflows and `dependabot.yml` as documented.
28. NATS payments user has publish and subscribe deny-all (`deploy/nats/nats-server.conf:165-174`).
29. Commerce EUR-only refusal (`catalog_pricing.go:265`, `api/server.go:2099`).
30. No demo seed target or migration.
31. Generated outputs list (`scripts/generate-api.sh outputs`, 12 files).
32. 55 Compose variables versus `configuration.md` (26 absent).
33. TKT-82 evidence JSON versus on-sale README figures.
34. TKT-543 and TKT-544 merged on HEAD (`4c029b66`, `d06b5d35`); the TKT-380 finding file is
    correctly dated as a pre-fix record.

## Supporting assets (inventoried, not inspected as binary)

From `git ls-files`, files that are not among the 274 documents:

- `.claude/`: 3 hooks, `settings.json`, `sdlc.config.json`, 3 skill scripts, 6 test files
  (196 tests, all pass), 9 JSON or HTML assets (report template, labels, examples).
- `docs/diagrams/`: one `.drawio` and four PNG previews (no inbound link).
- `docs/evidence/`: PNGs (TKT-25, TKT-337), Grafana and Tempo JSON captures (TKT-337), probe
  sources and scripts (`probe.go`, `run.sh`, `validate.py` for TKT-348, TKT-356, TKT-380), diffs,
  `sources.json` (TKT-374), `full-profile.json` (TKT-82).
- `docs/sprints/S-2/`: `report.en.html`, `report.en.json`, and about 300 evidence files (logs,
  JSONL test streams, mutation results, patches, gzip archives, verification scripts).
- `docs/product/`: two PNG mockups (no inbound link). `docs/verification/`: seven PNG screenshots.
- Contracts: five `openapi.yaml` specs, five `openapi_gen.go`, seven `*-api-types.gen.ts`.

I did not open images, gzip archives or JSONL streams. I read the top-level fields of
`report.en.json` and `full-profile.json` only.
