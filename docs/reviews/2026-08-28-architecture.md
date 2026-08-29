# Architecture Review

**Date**: 2026-08-28
**Scope**: Full system — five services, gateway, shared/go, event architecture, API construction, ADR conformance (spot-checked: 017, 018, 021, 022, 029, 060, 062-068).

## Summary

For a system carrying ~70 ADRs and 280+ tickets, the architecture is unusually coherent. The ADR-001/002/007 service split is intact and actively defended: every sampled cross-service touchpoint states its boundary rationale in code, the shared envelope package makes schema-before-data dispatch structurally unavoidable, and migrations, generated code, and the lifecycle chain are all gate-checked. ADR conformance spot-checks found no drift on 017, 018, 021, 022, 029, 060, 064, or 062-068.

The risks are of a different kind than erosion-by-entropy. First, commerce is quietly becoming the orchestration hub: it calls all four other services synchronously, runs six background loops, and holds four structurally near-identical leased-runner packages whose copy-don't-share justification has now been accepted three times — and the code review found exactly the failure mode that justification risks (a lease-sizing fix that reached two of three copies). Second, one convenience hardened into a wrong boundary: commerce validates the fake PSP's token vocabulary, foreclosing ADR-032's provider neutrality from another service. Third, a few speculative artifacts (an orphaned event, a consumer skeleton that was named and never built) should be adopted or deleted so the recorded intent matches the system.

## Strengths

- The shared envelope package (`shared/go/domainevent/envelope.go`): `Raw` cannot judge `data` before `schema`, and the `schema <= 0` poison line lives in one place. The correct response to TKT-61 shipping twice.
- Gateway minimalism (`gateway/cmd/gateway/main.go`): one explicit route table, internal surfaces edge-denied by construction, encoded-separator refusal instead of normalization. Nothing to remove.
- Contract enforcement without generated servers: `shared/go/contract/http.go` validates every request against the embedded spec and fails response drift closed (ADR-028); `make check-generate` keeps all five `openapi_gen.go` files and the web types honest.
- ADR-022 migrations verified in compose: `x-migrate-job` anchor, five one-shot jobs, `service_completed_successfully` gating.
- Deliberate, argued credential duplication: per-service staff-write credentials each carry the argument for why they are not reused, pinned by enumeration tests. This is duplication as a security property — do not "DRY" it.
- shared/go is not overgrown: ~10 small packages, additions ADR-gated, `durableconsumer` deliberately holding only what is uniform.

## Recommendations

### High Impact

- [ ] **R1** — Commerce validates payments' PSP token vocabulary, and it's the fake's: public checkout rejects any `payment_token` failing `fakepsp.ValidToken()` (`services/commerce/internal/api/server.go:1548`, `shared/go/fakepsp/tokens.go`), and exchange upgrades hard-code `"fake-ok"` (`exchanges.go:580`). Payments' own port declares the token opaque and builds a real Stripe adapter when configured, but no Stripe token can survive commerce's gate. The fake adapter's vocabulary escaped into `shared/` and became a cross-service contract. Commerce should treat the token as opaque and let payments refuse unknown tokens; record the upgrade-payment deferral in an ADR either way.

### Medium Impact

- [ ] **R2** — Commerce is the accreting orchestration hub: synchronous dependencies on all four other services, a 715-line `main.go` with six background loops, a ~15k-line `internal/api`, and four leased-runner packages (~6,100 lines with tests) sharing an identical claim/lease/release lifecycle. The copy-don't-share argument has been accepted three times and its predicted failure mode has now occurred twice (lease sizing missed in one copy, attempt-accounting convention missed in two). Extract the lease-loop skeleton (claim, lease-expiry, release, gauge) into a shared package; the per-runner state machines stay put.
- [ ] **R3** — `platform.catalog.seat_map.published` is an orphaned event (`services/catalog/internal/events/events.go:24-28`): emitted for future readers (TKT-104/35/80) that all shipped and all chose synchronous pulls instead. It is pure emission cost plus a standing invitation to build a consumer that races the sync path. Adopt it or retire it with a short ADR note.
- [ ] **R4** — Two API construction styles with no recorded decision: catalog generates its chi server (`services/catalog/api/codegen.yaml`), the other four generate models only and hand-wire routes. The runtime validator closes most drift risk, but path/method wiring drift in four services is caught only at runtime, and contributors must learn which style a service uses. Converge, or record the asymmetry as deliberate — no ADR currently does.

### Low Impact

- [ ] **R5** — The scanner sits outside the contract-generation discipline: `web/scanner/src/App.tsx` hand-rolls untyped fetches against access's API while the other frontends consume gate-checked generated types. Access has no generated TS types anywhere (see code review R36); fixing that should include the scanner.
- [ ] **R6** — The consumer skeleton (TKT-127) that `shared/go/domainevent/envelope.go:28-34` defers the `id`/`type` checks to was never built, and inventory still dispatches on NATS subject ignoring `type` (documented, TKT-133). Tolerable with two consumers; the third will re-derive the discipline by hand — the exact failure the envelope package was built to end. Build the skeleton before the third consumer, or delete the promise.
- [ ] **R7** — State the system's real integration style in `docs/architecture.md`: NATS carries exactly two load-bearing flows; everything else is synchronous HTTP plus a reconciling sweep, and symmetric problems (refund voiding vs exchange switch) deliberately use different shapes, each argued in its ADR. Writing the "sync + reconciler, events only where a durable trigger is essential" norm down stops someone adding events for symmetry.
- [ ] **R8** — Set a norm for internal-peer read defenses before the current ceiling metastasizes: `services/inventory/internal/consumer/catalog.go` carries ~600 lines of fail-closed decode against catalog's authenticated internal API, every check traceable to a cited finding, but the pattern is already generating follow-on work about inputs nothing in this system can produce (TKT-143). Recommend a stated bound for internal-peer reads (size cap, single-value checks, id echo — and stop there).
