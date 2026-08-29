# Documentation Review

**Date**: 2026-08-28
**Scope**: All in-repo documentation — README.md, AGENTS.md, docs/ (operational docs, 69 ADRs, product, learnings, conventions, verification, reviews). Every claim below verified against the doc and the code/Makefile/compose it describes.

## Summary

The documentation is in unusually good shape for 361 commits of accreted features. The operational docs are actively maintained and every CLI subcommand they document exists; ADR hygiene is strong (`check-adr-numbers` and `check-markdown-links` run in the gate, supersessions cross-reference in both directions, corrections are recorded in place rather than silently rewritten); no sampled accepted ADR is contradicted by later code.

The failures cluster in two places. The "git-derived `.sdlc/` board" description that commit b0b7f7c1 fixed in AGENTS.md is still live in four other docs, so the wrong source of truth is still pointed at from `docs/`. And the docs written in mid-July and never revisited (docker.md, docs/README.md, solution-design.md, ROADMAP.md) have drifted: the back office is absent from all of them, pinned image versions are wrong, the index omits roughly half of what lives under `docs/`, and ROADMAP's Completed section stops ~150 merged tickets ago.

## Strengths

- Doc claims are largely executable and true: Makefile targets, env vars, ports, tooling, and CI mirroring all check out against the repo.
- The gate enforces doc health mechanically (`check-markdown-links`, `check-adr-numbers`), both born from real defects the Makefile comments name.
- ADR amendments are recorded in place with tickets and dates (ADR-008/022 placement-only supersession, ADR-006's API-name correction, ADR-021's implementation status), and the ADR-021 trust-boundary phrasing discipline propagates consistently.
- `docs/development.md` is a genuinely operator-grade runbook with honest scoping of every claim; `docs/demo-readiness.md` self-corrects with struck-through, annotated findings.

## Recommendations

### Critical (Must Fix)

- [ ] **R1** — The stale "git-derived `.sdlc/` board" fix missed four docs: `docs/ROADMAP.md:3`, `docs/technical-delivery-standards.md:11`, `docs/product/prd-v1.md:6`, `docs/conventions/commits-and-branching.md:26` still direct readers to `.sdlc/` as "the source for ticket status" / a "source of truth", contradicting AGENTS.md's corrected description (Fast Note Sync vault via the sdlc-board server; `.sdlc/` is a superseded rollback stub that must not be run). ROADMAP.md was edited 2026-08-20 and kept the claim.

### Important (Should Fix)

- [ ] **R2** — ROADMAP.md's Completed section stops at TKT-31 (`docs/ROADMAP.md:7-27`) despite the channel/partner-credential epic (TKT-236-252, ADR-053-060, ADR-064), reconciliation runners (TKT-163/259), the voided-ticket feed (TKT-162/ADR-066), staff redelivery (TKT-203/ADR-068) and more since. The page's own rule is "update this summary when an epic becomes true in the running system"; two months of merges currently read as zero progress.
- [ ] **R3** — `docs/docker.md` is a July-13 snapshot: the service table omits `backoffice` (compose.yaml:429), omits the per-service migrate jobs and `access-lifecycle-backfill` that ADR-022 makes structural (compose.yaml:147, 323), and pins `nats:2.14.3` / `grafana/otel-lgtm:0.29.0` where compose.yaml:96,138 have 2.14.4 and 0.30.0.
- [ ] **R4** — `docs/README.md` (12 lines, last touched 2026-07-17) indexes about half of `docs/`: `configuration.md`, `demo-readiness.md`, `reviews/`, `evidence/`, `spikes/`, and five of six `verification/` directories are unindexed and effectively orphaned. Also `docs/architecture/` contains only a `.gitkeep` — remove it or use it; beside `architecture.md` it invites confusion.
- [ ] **R5** — `docs/testing.md:63` cites `docs/verification/gate-scan/`, which does not exist (`docs/verification/` holds six other directories). `check-markdown-links` misses it because the path is prose, not a link.

### Suggestions

- [ ] **R6** — The back office is missing from the design docs: `docs/solution-design.md:20` and `docs/development.md:34-35` describe the frontends as storefront + scanner only, though development.md documents the back-office sign-in flow at length further down. Same lag in `README.md:46`, which describes `shared/go` as "healthz contract + observability" when it now holds 10 packages (`docs/architecture.md:98` has the current list).
- [ ] **R7** — `docs/testing.md:3` and `README.md:28` understate `make check`'s stage list (Makefile:17): `check-generate` — the stage AGENTS.md warns costs gate runs when forgotten — the workflow-trigger checks, `check-adr-numbers`, and `check-markdown-links` are all omitted.
- [ ] **R8** — `README.md:31` and `docs/docker.md:16-18` describe the smoke stack's project name and port 18080 as fixed; `scripts/stack-env.sh:34-35` slot-shifts both (`ticketing-smoke-<slot>`, `18080+slot`). True for slot 0 only, and `docs/demo-readiness.md:106-110` shows the collision trap is real.
- [ ] **R9** — `docs/adr/ADR-064-presale-unlock-codes.md:1-6` uses bullet-list front matter instead of the template's `## Status` section every other ADR uses; status-extraction tooling keyed on the template reads its status as empty.
- [ ] **R10** — Untracked strays: delete `.sdlc/vault-migration-plan.md:Zone.Identifier` (Windows NTFS stream artifact, never commit); move or delete `.sdlc/vault-migration-plan.md` (an implementation plan for the now-executed board migration, sitting inside a directory AGENTS.md calls a superseded stub — it belongs with the board repo or in `docs/` as a record); commit `docs/reviews/2026-08-26-backlog-decision-brief.md` (finished, self-contained, matches its directory's conventions — it looks like it was simply never staged).
