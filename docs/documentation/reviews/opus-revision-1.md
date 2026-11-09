# Opus 5.5 revision review 1

Reviewer: Opus 5.5 (`claude-opus-5-5`), review sub-agent, high effort. Findings only. I edited no
tracked file, made no commit, started no subagent, ran no gate and wrote nothing to the network.
The only file I wrote is this report.

Date: 2026-11-09. Candidate: `771a5c1a`. Baseline: `57dc4f4e6a297433a86a6e136cb1f84d1fc12fe5`.
Grid: `docs/documentation/evaluation.md`, sha256
`3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`. I checked the hash before
scoring. The worktree was clean at the candidate for the whole review.

## Scores

| ID | Metric | Baseline (mine) | Revision | Main reason |
|---|---|---:|---:|---|
| M1 | Accuracy | 6 | **7** | Most corrected claims now match code. `development.md:8-10` says a newer local Go can run; the gate refuses a different Go minor version. Capped at 7. |
| M2 | Authority and currency | 5 | 8 | Clear authority rules, known-limits register, historical banners. The ADR register omits some recorded amendments, and the learnings index lost its "instance gone" markers. |
| M3 | Findability | 4 | 8 | Small task index, ADR topic table, runbook index, record index. Two runtime error paths and one current page still route to `development.md` with no matching topic. |
| M4 | Task execution | 6 | **7** | Exact commands and expected verdict. The same Go claim misdirects setup. Capped at 7. |
| M5 | Coverage | 5 | 9 | All 55 Compose inputs, 12 generated outputs, hooks, overlays, ownership and known gaps now have a reference. |
| M6 | Contract and safety precision | 6 | 8 | Safety wording kept and tightened. A few baseline rule corollaries are gone (listed under D9). No over-promise found. |
| M7 | Token efficiency | 3 | 8 | `AGENTS.md` 35,156 to 6,777 bytes. Implementation entry is about 14.3 KB. The 65 KB ticket skill is unchanged and not disclosed in the cost table. |
| M8 | Clarity and structure | 5 | 8 | Plain, scoped entry pages. 26 added lines have broken punctuation left by em-dash removal. |
| M9 | Reference integrity | 7 | **7** | Markdown links and anchors pass. A tracked test reads the moved scanner section by path, and the candidate fails `make check`. Capped at 7. |
| M10 | Maintainability and provenance | 6 | 8 | Update map, invalidation triggers, inventory and results record. Maintenance steps do not cover non-Markdown referrers. One evidence summary was corrected in place with no inline note. |

Result: the owner bar is not met. M1, M4 and M9 are below 8 because of known material defects.
No critical or high safety finding. D1 blocks completion because the required gate fails. It is
not a safety finding.

## Prioritized defects

### D1. The candidate fails `make check` because of the documentation move (completion-blocking)

Metrics: M9 (cap), M10.

Evidence. A gate run started at 2026-11-09T06:18:40Z by another process on this worktree. I did
not start it. I waited for `.gate.lock` to disappear and then read the result:

```text
FAIL exit=2 head=771a5c1ae6bb6520425d6453ec11804c66400cd8 tree=8ebb730835831c67a33c618db9ddd86063954921
web/scanner test:  Test Files  1 failed | 42 passed (43)
web/scanner test:       Tests  3 failed | 2098 passed (2101)
```

All three failures are in `web/scanner/test/doc-namespace.test.ts` (DOC-01):

1. `scannerOperatorGuide()` reads `docs/development.md` and looks for
   `### Scanner Entry outcomes and local records`. That section moved to
   `docs/runbooks/scanner.md:49`. `development.md` keeps only `### Entry rejection instructions`.
2. The same helper failure breaks the time-bounds test.
3. "qualifies every decision identifier added to an ADR" runs
   `git diff ec336db8 -- docs/adr/` and refuses any added line with a bare `Dn` identifier. The nine
   new `docs/adr/scanner-outcomes/*.md` parts and the ADR-085 stub headings add 266 such lines.
   The text is unchanged accepted ADR text. The test reads a move as an addition.

What I checked: the moved runbook section satisfies DOC-01 on its own. It has no `D`/`P`
identifiers, and all four required sentences are present. Only the path is stale for tests 1
and 3. Test 2 cannot pass while the ADR-085 parts are new files, unless the test changes or the
moved text is rewritten.

Stages after `test-ts` (build, smoke) did not run, so those stages have no result for this candidate.
Earlier stages passed, including `check-generate`, `check-adr-numbers` (84 ADRs) and
`check-markdown-links` (289 files).

Required fix. This is an owner decision, because the change is documentation-only:

- Option A, test change with owner authorization: point `scannerOperatorGuide()` at
  `docs/runbooks/scanner.md`. Make the ADR check ignore moved lines. For example, diff with copy
  detection, or diff the parts against the ADR-085 body at `57dc4f4e`.
- Option B, no test change: keep the operator section in `development.md` and keep ADR-085 as
  one file. That reverses most of the M7 gain for scanner work.

Do not qualify each moved `Dn` by hand. That rewrites the accepted ADR text that
`results.md` says was moved unchanged.

### D2. The setup page states a toolchain rule that the gate refuses (medium)

Metrics: M1 (cap), M4 (cap).

`docs/development.md:8-10`: "A newer local Go can run because the `go` directive is a minimum."
`scripts/check-go-toolchain.sh` runs in the gate (`Makefile:131-133`). Its `converge()` function
compares the minor version of the active Go, the golangci-lint build, `go.work`, every `go.mod`,
the builder image and each workflow pin, and returns 1 on any difference. A host on Go 1.28 fails
`make check`. The script comment also records a patch-level linter failure (go1.27.2 export data,
TKT-523).

Required fix: say that the gate requires the active Go minor version to equal the declared one
(1.27 at this revision), and that the pinned linter must be able to read the toolchain. Link the
check script.

### D3. Code and runtime messages point to moved procedures with no matching redirect (medium-low)

Metrics: M9, M3, M4, M10.

The redirects for the 22 top-level sections are correct, and code that cites a `§` heading still
resolves in one hop. Two cited topics have no heading on the redirect page:

- `services/access/internal/consumer/config_verification.go:63` and
  `services/inventory/internal/consumer/config_verification.go:79` print
  "see docs/development.md to reconcile it" when a durable drifts. `compose.yaml:137` says the same.
  The procedure is now `docs/runbooks/nats.md` "Pre-provisioned service durables (TKT-327)".
  `development.md` does not name durables or consumers.
- `test/browser/reset-password.mjs:37` cites "development.md § Password recovery". It is now
  `docs/runbooks/customer-accounts.md:171`.

About 40 more non-Markdown references cite `docs/development.md` (source comments, a migration,
error text in `services/payments/internal/store/keyring.go:140`). They resolve through redirects.
`docs/documentation/maintenance.md` tells the reader to update incoming Markdown links and
inline-code paths. It does not say to search tests, source, Compose comments or runtime strings.
D1 comes from the same gap.

Required fix: add redirect headings for the durable reconciliation and password-recovery topics.
Add one maintenance row for moved or renamed documents:
`git grep -n "<old path>" -- ':!*.md'`, then review each hit. Name the DOC-01 test as a tracked
consumer of the scanner guide.

### D4. Broken punctuation left by em-dash removal (low)

Metric: M8.

26 added lines contain ` , `, ` : ` or `**. a` where an em dash was removed:

- `docs/runbooks/staff-accounts.md:32,49,76,85,89,90,115`, for example "**Provision an account** ,
  there is no seeded default credential".
- `docs/LEARNINGS.md`: 12 index titles, for example "turns storage into a validator : silently".
  These titles no longer match the H1 of their target files.
- `docs/learnings/legacy-index-notes.md`: 7 bullets, for example "not equivalent code**. a forced-partition".

Required fix: rewrite each sentence. Keep index titles identical to the target H1, or shorten
them on purpose.

### D5. A current page still sends recovery work to `development.md` (low)

Metrics: M3, M1. `docs/solution-design.md:42-43`: "Configuration and operational recovery
procedures live in `configuration.md` and `development.md`." The revision edited the line above
it. Point to `runbooks/README.md`.

### D6. Commerce worker tuning is described two ways (low)

Metrics: M1, M4. `docs/configuration-reference.md:74` says correctly that base Compose does not
forward the worker variables. I checked `compose.yaml`, and `workerConfigFromEnv` at
`services/commerce/cmd/commerce/main.go:278-305`. `docs/configuration.md:66-73` says the
variables "tune the runner" and have "no compose `:?` marker". A reader who sets them in `.env` sees
no change. Add the overlay requirement or link the register section.

### D7. The ADR status register drops recorded amendments (low)

Metric: M2. `docs/adr/README.md` has a "Status and qualification" column, but it shows several
amended ADRs as plain "Accepted":

- ADR-025 is amended by ADR-085. The amendment is at `ADR-025...md:211`, and the ADR-066 row
  does show its amendment.
- ADR-042 status records TKT-191, TKT-194 and TKT-197 amendments.
- ADR-020 status records the TKT-249 amendment.
- ADR-001 status records amendment by ADR-006.

The preamble says the linked status governs nuance, so this is disclosed. The ADR-025/066
asymmetry can still mislead a scanner change. Add amendment pointers for these rows, or rename
the column so it does not imply completeness.

### D8. Learnings index lost its currency markers (low)

Metric: M2. The baseline section "Historical — the repo instance is gone, the lesson is kept"
said that the `-run` allowlist was fixed, the `pgrep` watcher no longer exists, and the ADR-017 §2
example was superseded. `docs/LEARNINGS.md` now lists those notes as plain dated rows. Add a short
"historical instance" mark to those rows.

### D9. Small semantic losses from the baseline `AGENTS.md` (low)

Metrics: M6, M2. I compared every baseline working-agreement bullet with `AGENTS.md`,
`docs/conventions/engineering-rules.md`, the PR guide, the runbooks and the routed ADRs. Almost
every binding rule survives. These items have no current home:

- "Do not over-explain ticketing basics; do surface non-obvious engineering trade-offs." This was
  guidance on how to work with the owner.
- The stack line lost "React". `AGENTS.md` says "TypeScript frontends". All three apps use React
  19 (`web/*/package.json`), and ADR-001 names React.
- The contingent-guard corollary: a test that asserts the currently observed refusal pins the
  limitation and goes red when the guard starts working. This applies to the EUR-only refusal.
- "A new test is a change that earns the same treatment as production code." The PR guide's
  "review every changed line, including test fixtures" covers most of it.

The ADR-017 traps (bottom end at `schema <= 0`, fixtures built from the current struct) and the
ADR-029 honest-writer limit are still in their ADRs (`ADR-017:167,186-197`, `ADR-029:34,117-121`).
These are not losses.

### D10. Access to Commerce sync edge has an incomplete purpose (low)

Metric: M5. `docs/architecture.md` lists Access to Commerce as "Resolve a delivery address".
Access also calls `/internal/orders/{id}/seats` (`services/access/internal/consumer/seats.go:53`)
and `/internal/exchanges/{id}/tickets-switched` (`consumer.go:316`).

### D11. Historical evidence summary corrected in place with no inline note (low)

Metric: M10. `docs/verification/on-sale-load/README.md:9-18` now says `git ed2ed47a` and the
JSON figures. I checked the figures against `docs/evidence/TKT-82/full-profile.json`, and they
are correct. The README does not say that it previously cited `4c6f2ec` and other
`claim_history` figures. `results.md` records this, but a reader of the evidence page cannot
see it. Add one dated correction line.

### D12. Ticket-run context cost not disclosed (low)

Metric: M7. `.claude/skills/sdlc-ticket/SKILL.md` is still 65,275 bytes. A ticket run still loads
it plus its stage references. The skill may be owned upstream (commit `57dc4f4e` ports it), so
leaving it unchanged can be correct. The context-cost section of `results.md` should still list
it as a remaining load. Scanner work also still needs `runbooks/scanner.md` (35,429 bytes) and
large ADR parts (`outcome-tables.md` 75,156 bytes).

### D13. Term drift on the CI page (low)

Metric: M8. The H1 of `docs/conventions/continuous-delivery.md` is now "Continuous integration".
The file name and the `AGENTS.md` link text still say "continuous delivery".

## Design problem judgment

D1 and D3 point to a design problem, not only local slips. The revision treats documentation as
a closed Markdown graph: links, anchors and redirects are checked among `.md` files. But document
paths and headings are also part of executable and operator-facing contracts. A vitest suite
reads a named section, runtime error strings send operators to a file, and Compose and migration
comments cite sections. Neither the link check nor the maintenance guide sees those consumers.
Each further move will repeat the problem until the maintenance procedure includes a
non-Markdown reference search. The better fix is to have doc-reading tests and messages cite a
stable entry, such as the runbook index or a fixed redirect heading.

The other findings are local. The layered structure (short binding core, task indexes, one owner
page per fact type, history in records) works and fixes the baseline design problem.

## What improved, with evidence

- `AGENTS.md` is 6,777 bytes, down from 35,156. The binding rules moved to
  `docs/conventions/engineering-rules.md` (7,543 bytes) as short imperative rules.
- `docs/development.md` is 5,596 bytes, down from 142,936. I diffed all 22 moved sections against
  the baseline. 13 have identical bodies. Eight differ only in lines that hold rewritten links or
  file paths: 2 changed lines each in nats, the four order-recovery sections and payments
  cancellation, 4 in scanner, 10 in ticket-lifecycle. `staff-accounts.md` has 43 changed lines:
  the corrected role text and the D4 punctuation.
- ADR-085: the stub keeps every baseline heading, so all old anchors resolve. I diffed the nine
  parts against the baseline sections. Each part adds only a four-line preamble and its heading.
- Back-office roles: `staff-accounts.md` now matches `authorization.ts` and `orders.astro`.
- Scanner README: all nine files in its map exist. It states the 3 s and 30 s limits, Entry and
  Exit, the 502/503/504 offline set and TKT-560.
- EUR-only selling appears in `architecture.md`, `ROADMAP.md`, `solution-design.md` and
  `known-limits.md`.
- Configuration register: a script compared every `${VAR}` in `compose.yaml` with
  `configuration-reference.md`. All 55 inputs are present with matching defaults or "Required".
  None is missing and none is extra.
- Contracts: `contracts.md` matches `./scripts/generate-api.sh outputs` (12 paths), and
  oapi-codegen config use for Catalog matches lines 8, 73 and 80 of the script.
- Hooks: `technical-delivery-standards.md#tool-hooks-and-limits` matches `.claude/settings.json`
  and `guard-bash.py` (gate-lock allowlist, merge verdict/tree/head checks, attribution regex at
  lines 170-176). I saw the Bash guard block my commands while the gate ran. The Read tool was
  not blocked, which matches "Edit/Write guard" scope.
- History: banners on `demo-readiness.md`, `brief.md`, `S-2/preparation.md`, and the gitignore
  learning. New `records.md`, `evidence/README.md`, `verification/README.md`, `sprints/README.md`.
  Screenshot-only folders are labelled as illustrations.

## Claims checked against code or configuration at the candidate

1. 55 base Compose interpolations versus the register: names and defaults match (script).
2. Commerce worker variables are absent from `compose.yaml`; defaults and parsing in
   `main.go:278-305`.
3. `STRIPE_SECRET_KEY` default `fake` and selection rules (`payments/cmd/payments/main.go:166-206`).
4. 12 generated outputs (`generate-api.sh outputs`); Catalog codegen config; models for the others.
5. `make up` and `make down` use the direct-ports overlay; `down` uses `-v` (`Makefile:260-264`).
6. Verdict format `PASS`/`FAIL exit= head= tree= log=` (`scripts/gate.sh:87-114`).
7. Gate stage order in `testing.md` equals `Makefile:33`.
8. CI runs `make check` and `gate-selftest.sh` with Corepack (`.github/workflows/check.yaml`).
9. `check-go-toolchain` minor-version rule (`scripts/check-go-toolchain.sh`), against D2.
10. Hook registration and guard behavior (`.claude/settings.json`, `guard-bash.py`).
11. Synchronous edges: Compose `*_URL` per service; `X-Internal-Token` clients in Access, Commerce,
    Inventory; back-office URLs and staff tokens (`compose.yaml:480-570`).
12. `shared/go` package list equals the directory listing.
13. React in all three web manifests.
14. Back-office `catalog.ts` has `getVenues` and `listVenueSeatMaps` (ADR-004 path fix).
15. TKT-82 JSON figures and commit against the on-sale README.
16. The scanner runbook section satisfies the DOC-01 content assertions.
17. Scanner README file map: nine files exist.
18. Learnings index: all 91 topic files linked; all 87 baseline links kept.
19. ADR-085 stub headings equal baseline headings; nine parts equal baseline bodies plus preamble.
20. 22 runbook sections versus baseline bodies (script diff).
21. ADR status sections versus register (script; D7).

## Coverage and limits

Counts refer to the candidate tree: 289 tracked Markdown files and 25 text files.

- Read in full at the candidate: `AGENTS.md`, `docs/README.md`,
  `docs/conventions/engineering-rules.md`, `docs/development.md`, `docs/runbooks/README.md`,
  `docs/configuration.md`, `docs/configuration-reference.md`, `docs/contracts.md`,
  `docs/records.md`, `docs/known-limits.md`, `.claude/skills/README.md`, `docs/adr/README.md`,
  `docs/adr/ADR-085-scanner-local-outcome-model.md`, `docs/architecture.md`,
  `web/scanner/README.md`, `docs/conventions/pull-requests.md`, `docs/evidence/README.md`,
  `docs/verification/README.md`, `docs/sprints/README.md`, `docs/runbooks/demo.md`, and the three
  `docs/documentation/` method files (`evaluation.md`, `maintenance.md`, `results.md`). 23 files.
- Read in full at the baseline, for semantic comparison: `AGENTS.md` (378 lines) and the
  `development.md` sections "Testing model", "Observability" and "Conventions".
- Read in part: `docs/testing.md` (lines 1-40 and greps), `technical-delivery-standards.md` (hooks
  section), `runbooks/staff-accounts.md` (lines 1-60), `runbooks/scanner.md` (lines 1-60 and the
  DOC-01 section by script), `docs/LEARNINGS.md` (head, tail, greps), `legacy-index-notes.md`
  (head), `scanner-outcomes/state-model.md` (head), ADR-017 and ADR-029 (greps), ADR-025 (status
  grep), and the diffs of `README.md`, `ROADMAP.md`, `docker.md`, `solution-design.md`,
  `dependencies-and-versions.md`, `commits-and-branching.md`, `continuous-delivery.md`, ADR-004,
  ADR-049, ADR-050, `demo-readiness.md`, `brief.md`, `S-2/preparation.md`, the gitignore learning
  and the on-sale README.
- Structural scan by script, all files: Markdown link and anchor scan over all 289 tracked `.md`
  files (1,287 relative file links, 56 fragment links, 0 broken anchors; the only missing targets
  are the two placeholder links in `docs/adr/[template].md`, which the gate excludes); runbook and
  ADR-085 body diffs; learnings link coverage; ADR status-versus-register comparison; Compose
  register comparison; em-dash count (3,763 in tracked `.md` and `.txt`, down from 4,057); byte
  counts. Total tracked prose is now 3,115,962 bytes, up from 3,062,164, because indexes and review
  records were added.
- Gate: observed, not run by me. See D1.
- Not done: no full read of the other 83 ADRs, the 89 learnings, the 27 skill files, the evidence
  or sprint records, or the scanner runbook beyond its first section. I did not run `make up`,
  `make browser` or smoke. Smoke and build did not run in the observed gate. I did not open images
  or check external URLs. I did not read the board, so ticket states such as TKT-560 and TKT-518
  are unverified. The baseline ledgers in `/tmp/ticketing-documentation-review` were used only for
  my own baseline findings. I did not audit every technical sentence. These scores are my judgment
  within this coverage.

## Required fixes to reach 8 on every metric

1. D1: get an owner decision on Option A or B. Then get a `make check` PASS on the committed
   candidate.
2. D2: correct the Go toolchain sentence in `development.md`.
3. D3: add the two missing redirect headings and the non-Markdown reference step in
   `maintenance.md`.
4. Then fix D4 to D13 in the same pass. Each is small, and none needs a source change.

After items 1 to 3, I expect M1, M4 and M9 to reach 8, subject to a fresh review.
