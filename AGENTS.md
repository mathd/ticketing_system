# Ticketing System

Event ticketing software, rebuilt from scratch with AI-assisted development.

## Context

The owner has built **three** event ticketing platforms over their career and sold **tens of
millions of tickets** on them; the most recent exceeded **1M lines of code**. Treat them as a
**domain expert** in event ticketing — inventory, holds/reservations, seating, pricing, fees,
payments, fraud, high-contention on-sales, etc. Do not over-explain ticketing basics; do surface
non-obvious engineering trade-offs.

## Goal of this repo

This is a **testbed**, not a client engagement. The point is to evaluate AI-assisted software
development on a **non-trivial** application by:
- authoring specs and PRDs from the owner's domain knowledge, and
- trying **different AI / development flows** to plan and build the system (e.g. the `sdlc-ticket`
  skill and board in `.sdlc/`).

Optimize for learning how well a given flow produces a real system — not for shipping the fastest
possible MVP. When a flow or process choice is being exercised, follow it faithfully so the
experiment stays valid.

## Working agreement

- **The stack is decided and scaffolded** (TKT-25 / PR #1, 2026-07-12): five Go services
  (catalog, inventory, commerce, payments, access) behind a thin Go gateway, TypeScript + React
  frontends (storefront, scanner shell), PostgreSQL 18.4 with one database per service, NATS
  JetStream as the event bus, all under a single `docker compose up`. Money is integer minor
  units + ISO currency code — floats are banned on money paths. See ADR-001/002/007 and
  `docs/architecture.md` for layout and service ownership. Don't propose a new stack; extend
  this one, and record deviations as ADRs.
- **Local gate:** `make check` (lint + test + build, Go & TS, plus the gateway smoke suite;
  run `make deps` first on a clean clone). CI runs the same gate — keep them mirrored.
- **Specs before code.** Prefer writing/refining a spec or PRD before implementing. Ground work in
  the written spec, not in assumptions about how ticketing "usually" works.
- **Record decisions.** Capture architecture and design decisions as ADRs in `docs/adr/`
  (see `registry.bindingPath` in `.claude/sdlc.config.json`; template in `docs/adr/[template].md`).
- **Documentation is 100% in-repo.** No Confluence or external wiki is attached to this project:
  PRDs and briefs live in `docs/product/`, ADRs in `docs/adr/`, learnings/process notes in `docs/`,
  and ticket-level context on the `.sdlc/` board. Anything the sdlc-ticket skill would send to a
  wiki goes to `docs/` instead.
- **Documentation lives in `docs/`** — a standard docs scaffold (architecture,
  ADRs, learnings, roadmap, solution design, conventions). The former Python-scaffold docs
  (`configuration.md`, `development.md`, `docker.md`, `testing.md`) were superseded by the
  Go/TS equivalents in TKT-25.
- The `sdlc-ticket` skill and the git-derived board (`.sdlc/`) are the default workflow scaffolding
  for planning and tracking work here.
- **Catalog slot state transitions decide under a row lock.** Any catalog slot transition that both
  mutates state *and* derives an owed domain event must decide from the locked current row in one
  transaction — lock it (`SELECT … FOR UPDATE`), apply the transition atomically, then commit and
  emit the event *at-least-once after the commit* (the existing owed-marker pattern; never publish
  while holding the transaction). A conditional `UPDATE … WHERE status = x` followed by a *separate*
  re-read to derive event state is racy: a concurrent transition can commit in between and emit a
  phantom event (e.g. `performance.archived` on a still-published row, nil `archived_at` →
  mismatched deterministic id). Reference impls: catalog `ArchivePerformance` and
  `CloseSlot`/`ReopenSlot` in `services/catalog/internal/store/postgres.go`; the same row-lock
  decision pattern underlies access `Redeem` (`services/access/internal/store/postgres.go`).
