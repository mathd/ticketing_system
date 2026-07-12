# Product brief — Ticketing System v1

Status: draft for Gate 1 · Date: 2026-07-12 · Source: owner discovery interview (6 rounds + additions), synthesized per `sdlc-ticket` exploration Mode B.

## Brief

**Problem** — The owner (domain expert: three ticketing platforms built, tens of millions of tickets sold) needs a modern event ticketing platform covering the full breadth of a real operation — seated shows, festivals, outdoor parks with lodging, box office, resellers — with full money and ticket traceability designed in from day one (NF525-ready for a later French-market phase). Existing platforms are legacy (the last one exceeded 1M LOC); this rebuild is also the testbed for AI-assisted SDLC: the product must be non-trivial for the experiment to mean anything.

**Target user** — Three personas: the **organizer/operator** (programs venues, parks, festivals; sells; settles revenue), **staff** (box office, gate/entry, finance), and the **ticket buyer** (consumer web, guest checkout with optional account).

**Outcome** — A single organizer runs their venues, parks and festivals end-to-end on the system: program events through overridable rules, sell across four channels without overselling under on-sale load, trace every euro and every ticket end-to-end from an append-only audit trail, and settle split fees to system/venue/artist payees. Multi-tenancy is a designed-for later phase, not a v1 feature.

**Non-goals (v1)** —
- Multi-tenant onboarding/self-serve (domain model is tenant-aware; no tenant UX).
- Real payment provider (PSP is a port with a fake provider; real adapter is a later ticket).
- Cloud deployment (Docker Compose local only; no k8s, no managed cloud).
- NF525 compliance itself (closures, fiscal archives, certification): the French market is a later phase. V1's audit trail is *designed* so the NF525 profile layers on without schema or flow changes (ADR-003), which is the cheap moment to get it right.
- Standalone notifications service (thin provider-abstracted mailer only, log transport locally).
- Market/domain research; the owner is the domain source.

**Assumptions & risks** —
- *Validated (owner decisions, incl. grilling 2026-07-12)*: Go microservices + TS/React fronts; services from day one; traceability on money and tickets is a must from day one, French market/NF525 is a later phase; walking skeleton is milestone 1; design-target on-sale is festival/stadium scale (~50–100k capacity, 100k+ queued); full VAT modeling in v1; GDPR handled by pseudonymous trails + erasable PII store; one currency per event (no FX inside an order); unified dated-slot admission model (performances, festival days, park days); pick-your-own seats, FR+EN, invoicing/B2B terms, waitlists and refund protection all in v1 scope; SEO, GEO and agent-first capability are a must — LLM agents are legitimate buyers.
- *Unvalidated (new)*: that scalper automation and legitimate limit-abiding agents can be reliably distinguished at on-sale scale — the TKT-20/TKT-40 balance is a design problem to solve, not a solved one.
- *Unvalidated*: a solo/AI team can carry a services-from-day-one estate on Compose (flagged in ADR-002); DSL for event programming is worth it over declarative rule config (deferred to a spike); RFID wristband hardware can be simulated convincingly in a testbed.
- *Risk*: scope breadth (4 inventory models + fiscal compliance) — mitigated by epic-level backlog with decomposition only at prioritization, walking skeleton first.

**Success metric** — A scripted end-to-end demo passes on a clean machine: `docker compose up` → create venue/event → simulated contended on-sale (N buyers > capacity, zero oversell) → purchase (fake PSP) → QR e-ticket → gate scan (duplicate rejected) → the sale's hash-chained journal entries verify and the ticket's full lifecycle trace (issued → delivered → redeemed) is reconstructible. Secondary (testbed): every shipped ticket passed the full SDLC pipeline gates.

**Feasibility** — *Read in the repo*: no application code exists; only docs scaffold, the `sdlc-ticket` skill and the local board (`.sdlc/`). `docs/{configuration,development,docker,testing}.md` describe a Python scaffold that does **not** apply — superseded by ADR-001 (Go + TypeScript). Local gate is currently `pre-commit run --all-files`; the real gate is defined in US-001. *Inferred, not verified here*: NF525 (LNE/AFNOR NF525, art. 286-I-3° bis CGI) imposes inalterability/security/conservation/archiving on cash-register software — one driver (with the owner's traceability directive) of ADR-003's append-only trails; contention-safe reservation wants a single-writer inventory service (ADR-002).

## Pre-mortem (lens note)

*"Shipped and flopped / never shipped — why?"* Most likely failure: **breadth starvation** — 23 capabilities × services-from-day-one means integration tax eats the testbed before domain depth exists. Patch applied: only the walking skeleton is decomposed; every other capability is a COS-bearing epic awaiting prioritization, and ADR-002 records the coarse-services compromise (5 services, not 15). Second failure mode: **traceability as an afterthought** — retrofitting an audit trail (or NF525 chaining, later) onto mutable stores is a rewrite; patch: ADR-003 makes money and ticket paths append-only from US-004 onwards.
