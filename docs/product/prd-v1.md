# PRD: Ticketing System v1

Companion to [brief.md](./brief.md). This PRD covers v1 at two altitudes: a **capability map**
(one epic each on the board, COS on the epic, decomposed into stories only when prioritized) and
**Milestone 1 fully decomposed** (US-001…US-006, the walking skeleton). Its checkboxes preserve the
planning baseline rather than live delivery status; use the sdlc board (`~/sources/sdlc-board`,
backed by a Fast Note Sync vault) for ticket state and [the roadmap](../ROADMAP.md) for the current
capability summary.

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

## TKT-2 — Catalog & event structure (user stories)

Decomposed 2026-07-14 when the owner prioritized TKT-2 (Gate 1) as the head of M2. Scope per the
epic COS + the ADR-005 amendment (owner, backlog grilling): deliver the catalog **structure**
(single show, run, series/season, festival skeleton) and the **draft/published/archived** lifecycle
with domain events, **and generalize the dated-slot model** so one slot primitive carries
performances, festival days and park operating days — pressure-tested against re-entry, mid-day
capacity changes and weather closures. Full festival and park *verticals* (multi-day passes on
shared zones, RFID media, lodging, cabanas) stay in TKT-14 / TKT-13 / TKT-12; this epic ships the
catalog skeleton and the load-bearing slot abstraction they compose against.

Baseline (from M1 / TKT-26): the catalog already has `organizers`, `venues`, `events`,
`performances` (the first dated slot, deliberately concert-neutral) and `ticket_types`, plus
draft→published publish with the `platform.catalog.performance.published` domain event and
aggregated, TTL-tiered public reads. These stories extend that seam; they do not rebuild it.

Every story carries the epic's cross-cutting COS: entities are `organizer_id`-scoped (ADR-002),
public reads declare their ADR-004 TTL tier, localizable content stays locale-keyed (TKT-36), and
lifecycle transitions emit versioned domain events on the platform stream (ADR-009 envelope).

### US-007: Archived lifecycle state with domain events
**As** an organizer, **I want** to archive a published performance so it stops appearing on the
storefront while its history is preserved. **Priority:** P1 **Depends on:** — (extends TKT-26 publish)
**Acceptance Criteria:**
- [ ] Catalog exposes an idempotent archive transition on a performance (`published → archived`);
      archiving a draft is a 409 (only published offers archive); archiving an already-archived
      performance returns 200 without re-emitting — mirrors the publish idempotency precedent.
- [ ] Archiving emits a versioned `platform.catalog.performance.archived` domain event with a
      deterministic envelope id (retried/raced emissions de-duplicate), so inventory can stop
      offering the slot; the transition and its emission use the existing poor-man's-outbox marker.
- [ ] Archived performances disappear from the aggregated public list/detail reads; an event whose
      every performance is archived drops off the public list. TTL tiers unchanged (ADR-004).
- [ ] The lifecycle is a single explicit state machine (`draft | published | archived`) with
      rejected transitions returning 409 and a reason; unit + contract tests cover every edge.

### US-008 (SPIKE): Pressure-test the generalized dated-slot model
**As** the team, **I want** to prove the ADR-005 slot abstraction survives the hard cases **before**
US-009 hardens the schema, so a bad generic design doesn't hurt every downstream vertical.
**Priority:** P0 **Depends on:** — · **Type:** Spike (timeboxed, no production code) · `risk:low`
**Acceptance Criteria:**
- [ ] A written analysis (in `docs/` + an ADR-005 amendment) models three stress cases against a
      candidate slot shape: **re-entry** (a park pass holder exits and re-enters the same operating
      day — how the slot/claim expresses multi-entry vs the single-redemption performance case),
      **mid-day capacity change** (an operating day's capacity is raised or cut while entries are
      live — where the number of authority lives, catalog vs inventory, and how a cut below current
      occupancy behaves), and **weather closure** (an operating day closes partway through — the
      slot state, the effect on already-admitted and not-yet-arrived holders, refund signalling).
- [ ] The analysis names, for each case, **which attributes live on the catalog slot** vs **what the
      inventory claim core owns** (the ADR-005 boundary), and produces the concrete attribute set +
      state-machine US-009 will implement (slot kind, operating hours, re-entry policy, closure).
- [ ] Verdict: the unified model holds (with the documented attribute shape) **or** a scoped
      exception is recorded as a new ADR — either outcome unblocks US-009 with a decided schema.

### US-009: Generalize the dated slot (performance → typed slot with attributes)
**As** an organizer, **I want** the catalog to model performances, festival days and park operating
days as one dated-slot kind, so passes and inventory compose against a single primitive. **Priority:**
P0 **Depends on:** US-008 (attribute shape decided), US-007 (lifecycle to carry onto every slot kind)
**Acceptance Criteria:**
- [ ] Migration generalizes `performances` into the slot model decided in US-008: a `kind`
      (`performance | festival_day | operating_day`) and the kind-specific attributes (operating
      hours, re-entry policy, closure state) as decided — **existing performances migrate to kind
      `performance` with no behavioural change** (M1 flow, US-003/004/005/006 tests stay green).
- [ ] Catalog API creates a slot of each kind and sets its attributes; the publish/archive lifecycle
      (US-007) applies uniformly across kinds; OpenAPI contract regenerated and published (ADR-009).
- [ ] The publication domain event carries the slot kind + capacity so inventory provisions the pool
      identically regardless of kind (no fork in the claim path — ADR-005).
- [ ] Weather-closure and mid-day-capacity transitions from the US-008 spike are representable and
      emit their domain events; unit + contract tests cover each kind and each attribute transition.

### US-010: Series and seasons — grouping events into runs
**As** an organizer, **I want** to group performances into a series (a run of one show) and a season
(a programmed set), so buyers and reporting see the run, not scattered dates. **Priority:** P1
**Depends on:** US-007 (grouped offers publish/archive as a unit)
**Acceptance Criteria:**
- [ ] Catalog models a `series` (ordered run of slots for one event) and a `season` (a named set of
      series/events), both `organizer_id`-scoped, localizable names; API to create them and attach
      slots/events; a slot belongs to at most one series.
- [ ] Public reads expose the grouping: an event detail shows its series/run; a season read
      aggregates its events. New read endpoints declare their ADR-004 TTL tier; storefront renders
      a run as one grouped listing (browser-verified).
- [ ] Publishing/archiving is expressible at the series level (publish the run) and fans out to its
      slots idempotently; partial states (some slots archived) render correctly.

### US-011: Festival skeleton — festival over shared-capacity day slots
**As** an organizer, **I want** to model a festival as a container of festival-day slots that draw on
shared festival capacity, so TKT-14 can layer multi-day passes and zones onto a real structure.
**Priority:** P2 **Depends on:** US-009 (festival-day is a slot kind), US-010 (a festival groups like a season)
**Acceptance Criteria:**
- [ ] Catalog models a `festival` grouping festival-day slots (US-009 kind `festival_day`) that
      reference a **shared festival capacity** rather than per-day independent pools; the publication
      events express the shared-capacity relationship so inventory can provision it (TKT-14 consumes).
- [ ] API to create a festival, its days and the shared-capacity linkage; lifecycle (US-007) applies
      to the festival and cascades to its days; `organizer_id`-scoped throughout.
- [ ] Public reads list a festival with its days as one grouped offer (ADR-004 TTL tier declared;
      browser-verified). **Non-goals (TKT-14):** multi-day pass products, zones/stages, RFID media,
      camping add-on — this story ships only the catalog skeleton + shared-capacity structure.

## TKT-4 — Inventory & reservation core (user stories)

Decomposed 2026-07-16. This section preserves its planning baseline; the board records current
delivery status. Scope per the epic COS + the backlog-grilling amendments: complete the
reservation core every later vertical
claims through — slot lifecycle reactions, capacity adjustment (the behaviour ADR-005's amendment
decided and left to "the inventory-side capacity-adjustment ticket"), operational holds, channel
allocations, group/agency reservations, seated claims with best-available, and the on-sale load
proof. Occupancy capping stays out per ADR-013; shared-festival-capacity *pass products* stay in
TKT-14; agency payment terms stay in TKT-34; the channel registry stays in TKT-17 (allocations
here key on opaque channel codes).

Baseline (from M1 + TKT-2): inventory holds GA quantity pools keyed on `slot_id`
(`inventory_pools` + `claims`), with hold/finalize/confirm/release, lazy TTL expiry,
organizer-scoped idempotency keys, festival shared-capacity pools (publication schema 3), and the
≫C no-oversell contention test in the gate. It consumes only
`platform.catalog.performance.published` (ADR-017 schema dispatch, readiness latch) and emits no
events. These stories extend that seam; they do not rebuild it. Every claim-model story preserves
the ADR-010 invariants: pool→claim lock order, `confirmed + held ≤ capacity`, DB-time expiry,
single-use idempotency keys — and keeps the M1 contention test green.

### US-012: React to slot archival and closure — stop offering dead slots
**As** an organizer, **I want** archiving or weather-closing a slot to stop new reservations
immediately, so buyers can't hold capacity on an offer that no longer exists. **Priority:** P1
**Depends on:** —
**Acceptance Criteria:**
- [ ] Inventory consumes `platform.catalog.performance.archived`, `.closed` and `.reopened` per
      the ADR-017 rules (dispatch on `schema` before decoding `data`; unknown future schema →
      NAK + readiness latch; `schema <= 0` and malformed known payloads → Term; consumed-event
      dedupe as today).
- [ ] An archived or closed pool rejects **new** holds (409 with a distinguishable reason);
      confirmed claims are untouched; live holds keep their TTL and may still finalize/confirm
      (the buyer already in checkout is a commerce/refund concern, not an inventory revocation).
- [ ] `reopened` restores claimability (closure is reversible per the US-008 spike); archival is
      terminal. Availability reads reflect the offering state.
- [ ] Ordering/skew safety: a `closed` arriving for a pool that was never provisioned does not
      poison the consumer; the disposition table in the consumer tests covers the new subjects.

### US-013: Capacity adjustment with the clamp floor
**As** an organizer, **I want** to raise or cut a slot's capacity after publication, so production
changes (extra seats released, a stage reconfiguration) don't require republishing. **Priority:** P1
**Depends on:** —
**Acceptance Criteria:**
- [ ] Inventory exposes a staff/internal capacity-adjustment operation per the ADR-005 amendment:
      raises apply freely; a cut below demand clamps to the invariant floor
      `max(new, confirmed + held)` and blocks new claims while demand exceeds the target — never
      force-releasing a confirmed admission (forward-only).
- [ ] Adjustment is idempotent and audit-visible (who/when/from→to recorded); it takes the pool
      row lock (ADR-010) so it serializes correctly against concurrent holds; the contention test
      extended with an adversarial adjust-during-holds interleaving stays oversell-free.
- [ ] Shared festival pools adjust as one pool (the group is the pool); availability reads
      reflect the new capacity within their ADR-004 TTL tier.
- [ ] Catalog's published capacity stays the initial snapshot only — no silent overwrite of a
      pool with claims (existing `Provision` guard preserved and tested).

### US-014: Operational holds — house, artist and kills
**As** box-office staff, **I want** to place named operational holds (house seats, artist
allotment, kills) that sit outside public sale, and release or convert them without racing the
public. **Priority:** P0 **Depends on:** —
**Acceptance Criteria:**
- [ ] Claims gain an operational type with a named purpose (`house | artist | kill | other` +
      label); operational holds have **no TTL**, count against pool capacity, and are placeable/
      releasable via staff/internal API only (gateway keeps them off the public edge).
- [ ] **Convert without a gap:** converting an operational hold (whole or partial quantity) into
      a buyer-purchasable claim happens atomically under the pool lock — released-to-order
      capacity is never observable as publicly available in between.
- [ ] Partial release/convert supported (release 3 of a 10-seat house hold); the remainder stays
      held; all transitions journal-style auditable (who/when/why on the claim history).
- [ ] Availability reads distinguish sellable from operationally-held capacity for staff, while
      the public read keeps a single `available` number.

### US-015: Channel allocations — caps and scheduled release
**As** an organizer, **I want** to split a slot's sellable capacity across sales channels with
per-channel caps and scheduled give-back, so presales and resellers can't starve the public
on-sale. **Priority:** P1 **Depends on:** US-014 (extends the same claim-model surface; soft —
parallelizable if Planning confirms no schema overlap)
**Acceptance Criteria:**
- [ ] A pool can carry channel allocations (opaque channel code + cap + optional release-at
      time); allocations never sum above pool capacity; unallocated capacity is the default/public
      channel.
- [ ] Holds carry a channel; a hold beyond its channel's remaining allocation is rejected even
      when the pool has capacity (and vice-versa: pool exhaustion rejects regardless of channel
      headroom). The no-oversell proof extends to per-channel caps under the contention test.
- [ ] At `release-at`, a channel's unsold allocation returns to the public channel — enforced by
      DB time like TTL expiry (lazy, correct without a sweeper); the release is observable in
      availability reads.
- [ ] Channel codes are opaque strings here; the channel registry, per-channel pricing and
      windows stay in TKT-17/TKT-5 (non-goal).

### US-016: Group and agency long-lived reservations
**As** an organizer, **I want** to grant a group or agency a long-lived reservation they draw down
over time, so bulk buyers don't fight the public TTL. **Priority:** P2 **Depends on:** US-014
(typed-claim machinery; soft)
**Acceptance Criteria:**
- [ ] A reservation claim type with an explicit expiry date (not the cart TTL), placeable by
      staff for a named counterparty; counts against capacity like any claim (and against a
      channel allocation when one applies).
- [ ] Draw-down: quantity converts to confirmed orders **partially and repeatedly** (10 of 200
      today, 50 next week) without the unconverted remainder ever passing through a publicly
      claimable state; unconverted quantity returns to sale at expiry (lazy, DB-time).
- [ ] The order/payment side of a draw-down uses the existing commerce checkout seam; agency
      pay-later terms, invoicing and dunning are explicitly TKT-34 (non-goal).
- [ ] Contention test covers concurrent public holds racing a reservation draw-down.

### US-017: Seat-level claims — reserved seating enters the core
**As** a buyer, **I want** to hold specific seats, not just a quantity, with oversell impossible
per seat. **Priority:** P2 **Depends on:** TKT-3 (a seat map to claim against — hard blocker)
**Acceptance Criteria:**
- [ ] The claim core accepts a seat-set claim against a seated slot: each seat is held/confirmed
      by at most one live claim (DB-enforced), reusing the ADR-010 lifecycle, lock order,
      idempotency and TTL machinery — the primitive extends, it does not fork (ADR-005 /
      ADR-010 "seats reuse the lifecycle and lock ordering but add their own resource
      constraints").
- [ ] GA and seated pools coexist; the existing GA path is untouched (M1 tests green).
- [ ] Seat-map versioning interplay (TKT-3 COS: edits never orphan or duplicate sold/held seats)
      is honored from the inventory side: a claim pins the seats it named regardless of later map
      edits.
- [ ] The contention test gains a seated variant: ≫C claimants targeting overlapping seat sets;
      no seat is ever double-held.

### US-018: Best-available seat selection
**As** a buyer, **I want** "best available N" to return contiguous seats honoring the map's
adjacency, atomically claimed. **Priority:** P2 **Depends on:** US-017
**Acceptance Criteria:**
- [ ] A best-available request for N seats returns the first legal contiguous run within the
      pool's bounded ordering projection and claims it in the same transaction that selected it;
      selection and claim never race (ADR-061).
- [ ] Relaxation is explicit and deterministic: never split a party; when orphan prevention is
      enabled, skip a run that would strand a seat and refuse only when no legal run is found in
      the bounded window.
- [ ] Under contention, parallel best-available requests never double-assign or deadlock. The
      pool lock serializes selection and the ordered scan is bounded; there is no retry loop.
- [ ] A pool without an ordering projection returns `best_available_unsupported`, not a sellout.
      Today that means best-available is limited to pools provisioned with orphan prevention.

### US-019: On-sale load proof — the sustained no-oversell gate
**As** the owner, **I want** a sustained adversarial load test proving no-oversell and acceptable
latency on the claim path, so the festival-scale NFR is measured, not asserted. **Priority:** P1
**Depends on:** —
**Acceptance Criteria:**
- [ ] A load harness (tool chosen at Planning) drives the real gateway→inventory hold/finalize/
      confirm path against a hot pool with a **sustained** profile derived from the NFR
      (thousands of checkout attempts/min for minutes, not a burst); asserts zero oversell and
      records latency percentiles + the per-pool throughput ceiling (ADR-010 deliberately
      serializes a hot pool — this story publishes the measured number the waiting-room design
      (TKT-20) must respect).
- [ ] A scaled **gate profile** runs in `make check` within the existing gate-time budget; the
      **full NFR profile** is a documented on-demand target (owner decision at Gate 1 confirms
      this split — the epic COS says "in the gate", full-scale locally in every gate run is the
      open question).
- [ ] Results land in `docs/` (evidence format per `docs/testing.md`); TKT-31/TKT-37 reuse the
      harness (read-path and observability profiles are their scope, non-goal here).

## Open items deliberately deferred to epic decomposition

Storefront shell framework — Astro 7 vs React SPA (spike TKT-39 → ADR-006, decided before US-002 builds storefront UI) · DSL vs declarative rules (spike in TKT-5) · seat-map versioning against sold seats (TKT-3) · resale marketplace rules (TKT-9) · queue fairness algorithm (TKT-20) · per-currency settlement mechanics (TKT-10) · RFID hardware simulation fidelity (TKT-14) · shared-festival-capacity claim mechanics (spike US-008 decides the catalog/inventory boundary; TKT-4/TKT-14 implement the claim path).
