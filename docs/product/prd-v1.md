# PRD: Ticketing System v1

Companion to [brief.md](./brief.md). This PRD covers v1 at two altitudes: a **capability map** (one epic each on the board, COS on the epic — decomposed into stories only when prioritized) and **Milestone 1 fully decomposed** (US-001…US-006, the walking skeleton).

The problem, personas, outcome, non-goals and success metric are in the brief. In two sentences: rebuild a full-breadth event ticketing platform (seated shows, festivals, outdoor parks with lodging, four sales channels, append-only money/ticket audit trails, split fees) as a Go-microservices + TypeScript system, single-organizer first but tenant-aware. Milestone 1 is the thinnest end-to-end slice across the real service boundaries: create an event → buy a GA ticket through the fake PSP → scan it at the gate.

## Architecture frame (binding decisions)

- **ADR-001** — Go services, TypeScript/React frontends (storefront, back office, scanner), EUR-primary multi-currency money type (minor units + currency, never floats).
- **ADR-002** — Services from day one, but a **coarse cut of five**: `catalog` (venues, seat maps, events/performances/series/festivals, rule definitions), `inventory` (all reservation models — GA, seats, entitlements, lodging calendars; holds; allocations; the contention hot path), `commerce` (cart, pricing/fee/promo evaluation, orders, post-purchase), `payments` (PSP port + fake provider, wallets, NF525 journal, settlement ledger), `access` (ticket issuance & delivery, scanning, pass/wristband validation). Plus an API gateway and an event bus for the audit/analytics stream. Docker Compose only.
- **ADR-003** — **Traceability is a must, for money and tickets**: money-relevant records land as immutable, hash-chained, signed journal entries from the first order (corrections = compensating entries; read models = projections), and every ticket/entitlement state change (issued, delivered, transferred, resold, exchanged, redeemed, reissued, invalidated) is an append-only lifecycle event linked to its order. NF525 is **not** a v1 goal — it's a later French-market profile (TKT-11) that must layer onto this trail without schema or flow changes.
- **ADR-005** — **Unified dated-slot admission**: every admission-granting offer (performance, festival day, park operating day) is a dated slot in one inventory machinery; passes are entitlements to claim slot entries; lodging/day resources reserve through the same core. One claim primitive, one no-oversell proof.
- **Scale NFR (owner, grilling 2026-07-12)** — design-target on-sale is **festival/stadium scale**: ~50–100k capacity event, 100k+ concurrent buyers in the waiting room, thousands of checkout attempts/min sustained. Load tests in TKT-4/TKT-20/TKT-31/TKT-37 target these numbers.
- **Discovery & agents (owner, 2026-07-12)** — **SEO, GEO (generative engine optimization) and agent-first capability are a must.** Event content must be server-renderable with structured data (search + generative engines), and LLM agents are **first-class clients**: they can discover events and complete purchases through a documented agent surface. The standing tension to balance: bot detection/prevention for big on-sales (TKT-20) must distinguish **scalper automation** (fought) from **legitimate, limit-abiding agents** (served) — blanket bot-blocking is off the table. TKT-40 owns this.
- **Currency model (owner, grilling)** — each event sells in exactly **one currency**; the platform handles many currencies across events; settlement is per currency. No FX conversion inside an order.
- **ADR-004** — **Cache-first read path**: every public read endpoint declares a volatility-tiered TTL (seat maps: hours → event lists: minutes → availability: seconds → hold/order/scan state: never); hot events are served from in-memory structures refreshed from the write path; frontends make few aggregated calls and refresh on the endpoint's TTL cadence. Writes are never cached.

## Quality gates (every story)

- Local gate green: `make check` (lint + unit + integration via Compose) — defined in US-001 and recorded as `config.code.localGate`; mirrors CI.
- Contract tests pass for any touched service boundary.
- UI stories: verified in a browser, screenshot in the PR.
- Money/ticket-path stories: audit-trail invariants hold (journal chain verifies end-to-end; every ticket state change appends a lifecycle event; **no raw PII in any append-only record** — pseudonymous IDs only, ADR-003) once US-004 lands.
- Read-endpoint stories: TTL tier declared with rationale, `Cache-Control` set (ADR-004).

## Capability map (epics)

Each is one epic on the board with COS; IDs are board keys. Decomposition into US-stories happens at prioritization, not now.

| Epic | Capability | Scope highlights |
|---|---|---|
| TKT-1 | **M1 — Walking skeleton** | US-001…006 below; the only decomposed epic |
| TKT-2 | Catalog & event structure | Events, dated performances, series, seasons, festival structure; tenant-aware from day one |
| TKT-3 | Venues & seat map designer | Venue model, GA capacities, in-house seat map authoring (sections/rows/seats), versioning against sold inventory |
| TKT-4 | Inventory & reservation core | Contention-safe claims (no oversell under load), cart holds w/ TTL, operational holds (house/artist/kills), channel allocations, group/agency reservations, best-available seating |
| TKT-5 | Pricing & overridable rules | Price levels per ticket type, time/tiered pricing, rule hierarchy venue→series→event→price with overrides; DSL spike (build vs declarative config) |
| TKT-6 | Service fees & revenue splits | Fee schedules per channel, absorbed vs passed-on, rounding; split ledger across payees: system, venue, artist, extensible |
| TKT-7 | Promotions & discount codes | Single-use and multi-use codes, %/fixed, redemption limits, comps |
| TKT-8 | Cart, combos & cross-sell | Multi-item cart across inventory models; combos/packages (bundle pricing); parking & merchandise cross-sell; refund-protection add-on (buyer-paid, own refund/settlement semantics) |
| TKT-9 | Orders & post-purchase | Refunds full/partial, exchanges w/ price-difference settlement, transfers & controlled resale, event-cancellation bulk flows |
| TKT-10 | Payments & multi-currency | PSP port + fake provider (3DS-like challenge simulation); one currency per event, many across the platform, per-currency settlement, no FX inside an order; real PSP adapter later |
| TKT-11 | NF525 compliance profile (later phase) | When the French market is prioritized: period closures (Z), fiscal archives, audit export layered on the ADR-003 journal — validated to require no schema/flow changes; verify inferred ISCA reading against the referential |
| TKT-12 | Passes & entitlements | Venue season tickets; park season passes (unlimited entries, named holder w/ photo binding); multi-park passes |
| TKT-13 | Lodging & bookable resources | Camping: nightly stays, check-in/out, occupancy; cabanas: day-slot tied to park visit; parking inventory |
| TKT-14 | Festivals | Multi-day passes + day tickets on shared capacity (zones/stages), festival camping add-on, RFID wristband media (activation/binding, simulated hardware) |
| TKT-15 | Cashless on-site wallet | Wristband-linked wallet: top-up, spend, refund-of-balance; NF525-relevant cash path |
| TKT-16 | Box office / POS | Staff-facing counter sales incl. cash, own channel allocation, NF525 receipt path, offline tolerance |
| TKT-17 | Sales channels & reseller API | Channel registry (web, POS, presale, reseller), per-channel price/fee rules & windows, presale codes, external reseller API on shared inventory |
| TKT-18 | Ticket delivery & fulfillment | QR/barcode e-tickets, anti-screenshot rotating barcodes, static PDF fallback, provider-abstracted mailer (log transport locally) |
| TKT-19 | Access control & scanning | Gate scanning UI + API, redemption state, duplicate detection, offline tolerance, pass & wristband validation, multi-entry semantics for park passes |
| TKT-20 | Waiting room & abuse protection | Virtual queue for hot on-sales (fair ordering, festival-scale NFR), purchase limits keyed on accounts — strict-limit events can require login (guest stays available elsewhere), rate limiting & suspicious-order flagging |
| TKT-21 | Customer accounts & wallet | Guest checkout + optional account, ticket wallet, prerequisites for transfers/limits |
| TKT-22 | Back office & staff RBAC | Staff identity, roles (box office / admin / finance), event & inventory builder UI, order management console |
| TKT-23 | Reporting & settlement | Sales/revenue reports, per-event settlement with fee-split breakdowns, exports |
| TKT-24 | Audit log & analytics pipeline | Immutable who-did-what on sensitive ops; domain event stream → local warehouse for analytics |
| TKT-31 | Read-path caching & hot-event serving | ADR-004 machinery: TTL tiers per endpoint class, in-memory hot-event structures with write-path invalidation, cache kill-switch, staleness tests, on-sale read load test at the festival-scale NFR |
| TKT-32 | Taxes & VAT | Per-product-class VAT rates (ticket vs merch vs lodging vs parking), tax-inclusive/exclusive display, VAT lines in journal/settlement/invoices, rates in the TKT-5 rules hierarchy |
| TKT-33 | Privacy & GDPR | The machinery over ADR-003's pseudonymous-trail invariant: erasable PII vault, erasure & data-export request flows, retention policies, automated no-PII-in-trails check |
| TKT-34 | Invoicing & B2B payment terms | Sequentially numbered invoices/receipts (feeds NF525 profile later); agency accounts with pay-later terms, due dates, partial payments, dunning — completes TKT-4's group/agency story |
| TKT-35 | Interactive seat selection | Buyer-facing seat map on the storefront: render TKT-3 maps, live availability via ADR-004 tiers, pick-your-own with adjacency rules, atomic seat claims at scale |
| TKT-36 | Internationalization (FR + EN) | Per-locale event content, localized UI/emails/documents, locale-aware date/money formatting; locale plumbing born in US-002; new locales addable without schema change |
| TKT-37 | Observability & load testing | Dashboards, distributed tracing UX, SLOs, on-sale load-test harness running the festival-scale NFR profile; builds on US-001's logs/traces/metrics foundation |
| TKT-38 | Waitlists & returns | Sold-out waitlist, auto-offer with expiry on returns/releases, offer cascade, limits honored — rides on TKT-4 holds machinery |
| TKT-40 | SEO, GEO & agent-first access | Structured data + server-rendered event content for search/generative engines; documented agent purchase surface; the written agent-vs-scalper policy reconciling with TKT-20 |

Cross-epic invariants: every money mutation flows through the append-only journal and every ticket/entitlement transition appends a lifecycle event (ADR-003, born in US-004/US-005); every inventory model (seats, GA, entitlements, lodging, wristbands) claims through the `inventory` service's contention-safe core; every price/fee decision resolves through the rules hierarchy (TKT-5) once it exists; every public read endpoint declares its ADR-004 TTL tier from birth.

## Milestone 1 — walking skeleton (user stories)

Goal: the five services, gateway, storefront and scanner exist and one GA ticket travels the whole path. Every story is a vertical slice, demoable alone. GA only; single price; EUR; no fees/promos/rules — breadth comes from the epics above.

### US-001: Platform scaffold — one command, all services up
**As** a developer, **I want** a monorepo where `docker compose up` starts the five Go services, gateway, storefront and scanner shells with health checks, and `make check` runs the full local gate. **Priority:** P0 **Depends on:** —
**Acceptance Criteria:**
- [ ] `docker compose up` from a clean clone brings up gateway + 5 service skeletons + storefront + scanner shell; `/healthz` green on all.
- [ ] Storefront placeholder page served through the gateway in a browser.
- [ ] `make check` (lint + test + build, Go & TS) passes and fails correctly on seeded lint/test errors; documented in README; CI runs the same gate.
- [ ] Services emit structured JSON logs with correlation/trace IDs propagated across service calls; a local tracing/metrics stack runs in Compose (concrete tooling chosen in the plan).
- [ ] Repo layout + service ownership documented in `docs/architecture.md`.

*Planning note:* the US-001 plan must propose the database(s) and event bus as an ADR, approved at the plan gate (owner decision, grilling 2026-07-12).

### US-002: Create and publish a GA event
**As** an organizer, **I want** to create a venue, event, performance and GA ticket type via the catalog API and see it listed on the storefront. **Priority:** P0 **Depends on:** US-001, TKT-39 (storefront-shell spike decides Astro vs SPA before this story's UI work)
**Acceptance Criteria:**
- [ ] Catalog API: create venue (GA capacity), event, dated performance, ticket type with EUR price; persisted; OpenAPI contract published.
- [ ] Publishing a performance emits a domain event; storefront lists published performances with name/date/price (browser-verified).
- [ ] Public catalog reads (event list, event detail) carry explicit `Cache-Control` TTLs per ADR-004 tiers; the storefront page issues one aggregated call and does not re-fetch within the TTL.
- [ ] Event content fields are localizable; the storefront renders FR and EN (locale switch) with locale-aware date/money formatting (TKT-36 plumbing born here).
- [ ] Entities carry an `organizer_id` (tenant-aware per ADR-002) even though v1 has one organizer.

### US-003: Contention-safe GA reservation with cart holds
**As** a buyer, **I want** to reserve N GA tickets so they're held for me briefly, **and as** the owner, I want oversell to be impossible under load. **Priority:** P0 **Depends on:** US-002
**Acceptance Criteria:**
- [ ] Inventory service exposes hold/confirm/release on performance capacity; hold has a TTL and expiry returns capacity.
- [ ] Concurrency proof: automated test slams a capacity-C performance with ≫C parallel hold requests; granted holds never exceed C; test is part of `make check`.
- [ ] Storefront: pick quantity → hold created → countdown visible (browser-verified).

### US-004: Checkout with fake PSP and append-only order journal
**As** a buyer, **I want** to pay for my held tickets and get an order confirmation. **Priority:** P0 **Depends on:** US-003
**Acceptance Criteria:**
- [ ] Commerce turns a hold into an order (EUR total, no fees); payments service authorizes/captures via the fake PSP port; success confirms the hold, failure/timeout releases it.
- [ ] Order + payment records are append-only, hash-chained and signed per ADR-003; a `verify-journal` command validates the chain end-to-end.
- [ ] Journal records reference the buyer by pseudonymous ID only; PII (name, email) lives in a separate erasable store (ADR-003 §3).
- [ ] Declined-card path returns the buyer to a retriable state (browser-verified both paths).

### US-005: QR e-ticket issued and delivered
**As** a buyer, **I want** my ticket with a QR code by email and on a web page after purchase. **Priority:** P1 **Depends on:** US-004
**Acceptance Criteria:**
- [ ] Access service issues a signed QR payload per paid ticket, triggered by the order-completed event.
- [ ] Mailer port (log transport) records a delivery with ticket link; buyer ticket page renders the QR (browser-verified).
- [ ] Issuance and delivery are recorded as append-only ticket lifecycle events linked to the order (ADR-003); the ticket's history is queryable; lifecycle events carry pseudonymous IDs only — the delivery address is resolved from the PII store at send time.
- [ ] Tickets are re-retrievable by order reference (guest flow, no account).

### US-006: Gate scan with duplicate rejection
**As** gate staff, **I want** to scan a ticket and get an unambiguous accept/reject. **Priority:** P1 **Depends on:** US-005
**Acceptance Criteria:**
- [ ] Scan API validates signature and redeems atomically by appending to the ticket's lifecycle trace; second scan rejects with reason + original scan time (derived from the trace); invalid/forged payload rejects.
- [ ] Scanner web page: camera-or-paste input, green/red result (browser-verified).
- [ ] End-to-end demo script (brief's success metric: journal chain verifies, ticket lifecycle trace complete) passes on a clean machine and runs in CI.

## Open items deliberately deferred to epic decomposition

Storefront shell framework — Astro 7 vs React SPA (spike TKT-39 → ADR-006, decided before US-002 builds storefront UI) · DSL vs declarative rules (spike in TKT-5) · seat-map versioning against sold seats (TKT-3) · resale marketplace rules (TKT-9) · queue fairness algorithm (TKT-20) · per-currency settlement mechanics (TKT-10) · RFID hardware simulation fidelity (TKT-14).
