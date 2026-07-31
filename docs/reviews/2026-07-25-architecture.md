# Architecture Review

**Date**: 2026-07-25
**Scope**: Full codebase — 5 Go services + gateway + 3 web apps, ~24k prod LOC / ~30k test LOC,
32 ADRs, ~75 tickets in.

## Summary

The service boundaries have held. Nothing in 75 tickets suggests catalog/inventory/commerce/
payments/access was the wrong cut, and the contention hot path is the only place where the split
costs anything. What has drifted is **where the load-bearing rules live**: the event envelope, the
durable-consumer protocol and the internal-auth check are each implemented 3–6 times, and the two
biggest scalability assumptions (cache-first reads, contract enforcement) are implemented in ways
that either don't exist yet or sit directly in the hot path. The single highest-value change is
gating response validation; the single riskiest thing found is that a buyer-visible hold
transition is guarded by a string-suffix denylist at the edge and nothing else.

Do **not** split any service. Split files, consolidate protocol.

## Strengths

- **Service boundaries were called correctly.** Zero evidence of a wrong cut across 75 tickets —
  no service reaches into another's DB, no bidirectional chatty calls. ADR-002 stands.
- **The scalability claim is measured, not asserted.** `smoke/onsale_load_test.go` runs a staged
  on-sale with hard p99 SLOs (hold/finalize/confirm ≤ 1s, lifecycle ≤ 3s) and a 50k-unit oversell
  tail asserting *exactly* the boundary. Very few systems this age have this.
- **Contract-first is real.** Generated types both sides, spec served, drift fails closed (ADR-028).
- **`shared/go` was kept deliberately thin** and additions gated by ADR. That discipline is why
  there is no shared-kernel god package — the cost shows up as duplication instead (R3, R13),
  which is the better problem to have.
- **Out-of-band migrations (ADR-022)** and the storefront's single-flight page cache are both
  correct and were not obvious calls.
- **Learnings are promoted into `AGENTS.md`, not left in a doc.** The loop actually closes.

## Recommendations

### High Impact

- [ ] **R1** — **Gate OpenAPI response validation out of the hot path.**
  `responseValidated` buffers *every* response through an `httptest.ResponseRecorder`, then
  re-parses and schema-validates the body on every request, in every service — including
  inventory's claim path under on-sale load. Two full body copies plus a schema walk per request,
  unconditionally, with no env toggle. This is the largest self-inflicted cost against the
  scalability assumption. Keep ADR-028's fail-closed semantics; make it on-by-default in
  dev/CI/smoke and off-or-sampled in production. While in there: the validator calls
  `context.Background()` (drops trace context) and the recorder discards `http.Flusher`.
  Files: `shared/go/contract/http.go` (`responseValidated`, `requestValidator`), all five
  `services/*/internal/api/server.go`.

- [ ] **R2** — **Make the inventory transition boundary structural, not a string denylist.**
  `/holds/{id}/confirm|finalize|release` are mounted unwrapped in inventory
  (`services/inventory/internal/api/server.go:49-51`) — no `internalOnly`, unlike every
  `/internal/*` route beside them. The only thing stopping a buyer from driving a hold to
  `confirmed` without paying is a `strings.HasSuffix` denylist in the gateway
  (`gateway/cmd/gateway/main.go`, the `/api/inventory/` arm). That control spans three components
  that each normalize paths differently (net/http `ServeMux`, `httputil.ReverseProxy`, chi). Move
  the three routes under `/internal/` — which the gateway already blocks by construction — or wrap
  them with `internalOnly`. Then delete the suffix denylist.

- [ ] **R3** — **Move the event envelope into `shared/go/events`.**
  The envelope struct is declared independently in `services/catalog/internal/events/events.go:32`,
  `services/commerce/internal/events/events.go:17`, `services/access/internal/consumer/consumer.go`
  (×3), `services/inventory/internal/consumer/consumer.go` (×2), plus inline anonymous structs in
  `services/access/internal/store/{scan,lifecycle,reconcile}.go`. ADR-017's rules — dispatch on
  `schema` before decoding `data`, treat `schema <= 0` as a broken envelope — are re-derived at each
  site, which is exactly how TKT-61 shipped the same ordering bug twice. Ship one `Envelope`, one
  `DecodeEnvelope` that enforces the `schema`-first rule, and one dedup-key helper.

- [ ] **R4** — **Reconcile ADR-004 with what exists: there is no cache above the storefront.**
  Real caching is one in-process Astro SSR map (`web/storefront/src/lib/cache.ts` — correct, with
  single-flight). Everywhere else `Cache-Control` is a declaration nothing honors: the gateway is a
  bare `ReverseProxy` with no cache, and backoffice/scanner/service-to-service reads pass straight
  through. Either add the shared cache at the gateway, or narrow ADR-004 to state that the TTL tiers
  are a *contract for future caches* and today bind only the SSR tier. Right now the ADR reads as if
  a cache tier exists.

### Medium Impact

- [ ] **R5** — **Give the gateway an explicit `http.Transport`.**
  `&httputil.ReverseProxy{Rewrite: ...}` with no `Transport` uses `http.DefaultTransport`, whose
  `MaxIdleConnsPerHost` is **2**. Past two concurrent requests per upstream the gateway opens a
  fresh TCP connection per request — at the front door of the on-sale path. Set
  `MaxIdleConnsPerHost`/`MaxIdleConns` to something matching the SLO target.
  File: `gateway/cmd/gateway/main.go`.

- [ ] **R6** — **Split catalog's files by aggregate; keep the service whole.**
  Catalog is 13,046 LOC — larger than any other service — concentrated in
  `internal/store/postgres.go` (2,166 lines, 63 methods) and `internal/api/server.go` (1,330), with
  a 2,266-line test file. Those 63 methods cover four aggregates: venue+seat map, event+performance
  slot, series/season/festival grouping, ticket types. Split into `store/{venue,seatmap,slot,
  grouping}.go` behind the same `Postgres` type. The service boundary is fine — the file boundary
  is what's failing.

- [ ] **R7** — **Replace catalog's inline internal-token checks with middleware.**
  `if s.internalCredential == "" || r.Header.Get("X-Internal-Token") != s.internalCredential` is
  copy-pasted into five handlers (`getTicketType`, `getPublishedPerformance`, `getPoolOfferState`,
  `decodeBatchPin`, …) in `services/catalog/internal/api/server.go`. Inventory already does this
  right with `s.internalOnly(...)`. One handler added without the line is an open internal endpoint,
  and nothing in the gate would catch it.

- [ ] **R8** — **Collapse catalog's five publish paths into one.**
  `publishPerformancePublished`, `publishClosure`, `PerformanceArchived`, `SeatMapPublished` and the
  backfill variant each rebuild `nats.Msg` + `Nats-Msg-Id` + `json.Marshal(Envelope{...})`
  identically. One `publish(ctx, subject, id string, schema int, occurred time.Time, data any)`
  removes ~60 lines and one drift surface. File: `services/catalog/internal/events/events.go`.

- [ ] **R9** — **Eight Go modules for one repo is already costing version drift.**
  `otel` is pinned only by access and shared; `kin-openapi` only by catalog, shared and smoke — the
  other three services depend on shared's copy transitively. Nothing here needs independent
  versioning (single repo, single deploy unit, `go.work` everywhere). Collapse to one `go.mod`, or
  keep the split and add a drift check to `make check`.

- [ ] **R10** — **`docs/architecture.md` is 75 tickets stale.**
  It states it "reflects what is running before M2 capability work begins." It has no seat maps,
  holds, PSP/Stripe, back-office, lifecycle trail, channel allocations, or out-of-band migrations —
  all shipped. For an AI-assisted testbed this is the document agents read first; stale here is
  more expensive than stale elsewhere.

### Low Impact

- [ ] **R11** — **`AGENTS.md` now carries eight "read ADR-N first" imperatives against 32 ADRs.**
  Every agent pays for all eight on every ticket regardless of what it touches. Move the
  file-scoped ones (ADR-018/019/020 → catalog store, ADR-029/031 → seat maps, ADR-021 → access
  lifecycle) into path-scoped instructions and leave only the genuinely cross-cutting ones
  (ADR-017, money, the browser-submit rule) in `AGENTS.md`.

- [ ] **R12** — **`PageDataCache` never sweeps.** Expired entries are dropped only when the same key
  is read again; a long-lived SSR process over a large catalog grows monotonically. A size cap or a
  sweep on insert is a few lines. File: `web/storefront/src/lib/cache.ts`.

- [ ] **R13** — **Two independent implementations of the same durable-consumer protocol.**
  `services/inventory/internal/consumer` and `services/access/internal/consumer` each implement
  readiness latching, quarantine-on-unknown-schema, backoff policy and envelope dedup — same
  protocol, different code, different bugs. After R3 lands, lift the shared skeleton
  (consume loop, `waitConsume`, readiness latch, dedup) into `shared/go/events` and leave only the
  per-subject handlers in each service.

## Assumptions that held vs. drifted

| Initial assumption | Verdict at 75 tickets |
|---|---|
| Five services from day one (ADR-002) | **Held.** No boundary regretted, no cross-DB reach. |
| Postgres claim transaction, not Redis (ADR-010) | **Held**, and proven by the oversell tail test. |
| Contract-first with fail-closed drift (ADR-009/028) | **Held in intent, mispriced in placement** — see R1. |
| Cache-first read path (ADR-004) | **Drifted.** Declared everywhere, implemented in one process — see R4. |
| Thin `shared/go`, additions need an ADR | **Held too well.** The rule is right; the envelope and consumer protocol are the two things that earned their way in — R3, R13. |
| One binary per service, migrations out-of-band (ADR-022) | **Held.** |
