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

- **No app exists yet.** There is no stack, framework, or architecture decided. Do not assume one —
  ask or propose, and record the choice.
- **Specs before code.** Prefer writing/refining a spec or PRD before implementing. Ground work in
  the written spec, not in assumptions about how ticketing "usually" works.
- **Record decisions.** Capture architecture and design decisions as ADRs in `docs/adr/`
  (see `registry.bindingPath` in `.claude/sdlc.config.json`; template in `docs/adr/[template].md`).
- **Documentation lives in `docs/`** — the py-moov `python_base` docs structure (architecture,
  ADRs, learnings, roadmap, solution design, conventions). Note: `configuration.md`,
  `development.md`, `docker.md`, and `testing.md` describe the py-moov Python scaffold, which is
  not implemented here yet — treat them as aspirational until a stack decision (ADR) lands.
- The `sdlc-ticket` skill and the git-derived board (`.sdlc/`) are the default workflow scaffolding
  for planning and tracking work here.
