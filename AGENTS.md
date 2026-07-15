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

- **The stack is decided and scaffolded.** Five Go services (catalog, inventory, commerce,
  payments, access) behind a Go gateway, TypeScript + React frontends, PostgreSQL (one database
  per service), NATS JetStream, all under one `docker compose up`. Layout and service ownership:
  `docs/architecture.md` (ADR-001/002/007). Don't propose a new stack; extend this one, and
  record deviations as ADRs.
- **Money is integer minor units + ISO currency code — floats are banned on money paths.**
- **Local gate:** `make check` (lint + test + build, Go & TS, plus the gateway smoke suite;
  run `make deps` first on a clean clone). CI runs the same gate — keep them mirrored.
- **Specs before code.** Prefer writing/refining a spec or PRD before implementing. Ground work in
  the written spec, not in assumptions about how ticketing "usually" works.
- **Record decisions.** Capture architecture and design decisions as ADRs in `docs/adr/`
  (see `registry.bindingPath` in `.claude/sdlc.config.json`; template in `docs/adr/[template].md`).
- **Documentation is 100% in-repo** — no Confluence or external wiki. PRDs and briefs in
  `docs/product/`, ADRs in `docs/adr/`, learnings in `docs/LEARNINGS.md` + `docs/learnings/`,
  ticket context on the `.sdlc/` board; see `docs/README.md` for the rest. Anything the
  sdlc-ticket skill would send to a wiki goes to `docs/` instead.
- The `sdlc-ticket` skill and the git-derived board (`.sdlc/`) are the default workflow scaffolding
  for planning and tracking work here.
- **Touching a catalog slot transition? Read [ADR-018](docs/adr/ADR-018-catalog-slot-transition-concurrency.md) first.**
  State-deriving transitions decide under a row lock and emit after commit; grouped members
  (festival days) refuse their own publish/archive. Both rules are narrower than they sound —
  the ADR draws the lines.
- **Scoping a catalog read to a subset? Read [ADR-019](docs/adr/ADR-019-catalog-read-path-scoping.md) first.**
  A scoped read is only scoped if an index backs the filter — copying a query shape that scales
  ships a no-op. Proving it takes two tests: the result is scoped, *and* the scan is. Narrower
  than it sounds — the ADR draws the lines.
- **Adding an index to a catalog migration? Read [ADR-020](docs/adr/ADR-020-catalog-index-build-concurrency.md) first.**
  Plain `CREATE INDEX` stays — `CONCURRENTLY` is still *not* adopted. [ADR-021](docs/adr/ADR-021-out-of-band-service-migrations.md)
  satisfied its precondition (1), but the preconditions are conjunctive and (2) and (3) remain false,
  so nothing changed. The ADR names the preconditions and the traps.
- **Claiming an audit or integrity guarantee? Read [ADR-021](docs/adr/ADR-021-ticket-lifecycle-trail-integrity.md) first.**
  **State inside the database cannot constrain an adversary who writes to the database** — so a
  quarantine row, a retry counter, a retained signature or a chain head proves nothing against one.
  A hash chain over data they cannot re-sign *does*, which is why modification and insertion are
  closed and rollback is not. Say which adversary you mean before writing "tamper-evident". Three
  clauses of ADR-021 broke this rule across two adversarial review passes — it is easy to get
  wrong while every individual fact is correct.
- **Touching how or where migrations run? Read [ADR-021](docs/adr/ADR-021-out-of-band-service-migrations.md) first.**
  Migrations run **out-of-band**: each binary's `migrate` subcommand, as a one-shot Compose job the
  service waits on (`service_completed_successfully`). The server path never migrates. ADR-008 still
  governs everything else (goose-as-library, embedded SQL, per-service ownership, fail-fast, 30s
  bound) — ADR-021 superseded it on *placement only*.
