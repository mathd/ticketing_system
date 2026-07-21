# ADR-030: Catalog stays out of the smoke happy-path coverage gate

Date: 2026-07-21

## Status

Accepted (TKT-109; decision taken under the owner-waived gates of that run, recorded on the ticket)

## Context

The smoke suite enforces a happy-path coverage gate (`uncovered2xxOps`, smoke/coverage_test.go):
after an unfiltered run, every documented 2xx operation of the DB-backed services
(inventory, commerce, payments, access) must have been exercised through the running stack, or
the suite fails. Catalog has been excluded from that loop since the gate's creation (TKT-47):
its per-operation coverage gate lives in its unit suite
(services/catalog/internal/api/coverage_test.go), where a fake store makes driving all
operations cheap, and every unit-test response still passes through the real
response-validation middleware.

TKT-106 added catalog to the smoke response-validator allowlist, so catalog responses that a
smoke request *does* exercise are now contract-validated end-to-end. Both TKT-106 review arms
independently flagged the residual gap: a new catalog operation added to
services/catalog/api/openapi.yaml with no smoke test passes the whole suite without end-to-end
contract validation. TKT-109 asked for an explicit decision: which layer owns catalog's
per-operation coverage?

Measured state at decision time (verified at file:line during TKT-109 plan review):

- Catalog's contract documents **35 operations**; **29 already reach `validateServiceResponse`**
  through smoke's validating helpers.
- The 5 never exercised by smoke — `editSeatMap`, `updateVenueGaCapacity`, `listVenueSeatMaps`,
  `getPublicSeatMapGeometry`, `listSeatMapVersions` — are precisely the seat-map surface shipped
  by TKT-104 (ADR-029), so the gap is recent and live, not hypothetical.
- `getOpenAPISpec` is exercised but never recorded: `validateServiceResponse` deliberately
  early-returns on the spec path. The smoke suite instead asserts the served spec is
  byte-identical to the committed file, which strictly dominates schema validation.
- The risk is **end-to-end only**, not "un-validated": catalog's unit gate fails closed on any
  new documented 2xx operation without a driving test, and those unit responses flow through the
  real validation middleware. What a unit-only operation skips is the real store, gateway
  routing, and migration-backed data — the layer the TKT-105 learning warns is where fake-store
  validation goes blind.

## Possible Solutions

- **Option 1 — do nothing (exclusion stays implicit):**
    - Pros: zero work.
    - Cons: the exclusion reads as an oversight; the next reviewer re-litigates it (TKT-106's
      review already did); nothing stops a well-meaning edit from half-joining catalog and
      shipping an immediately-red gate (see `getOpenAPISpec` below).
- **Option 2 — catalog joins `uncovered2xxOps` (chosen-against):**
    - Pros: one uniform gate; the 5 uncovered ADR-029 operations gain real-stack, real-store
      end-to-end validation; future catalog operations cannot ship without a smoke happy path.
    - Cons / cost (the real flip-cost, kept current so this decision stays cheap to reverse):
      5 new smoke happy-paths — three are trivial GETs against the seat-map fixture
      smoke already builds, `updateVenueGaCapacity` and `editSeatMap` need write fixtures
      (`editSeatMap` drags in ADR-029 pin semantics) — **plus** a `coverageAllowlist` entry for
      `getOpenAPISpec` (justified by the byte-identity assertion; without it the gate is red on
      the first unfiltered run). Ongoing: every future catalog operation pays a smoke fixture,
      duplicating the unit gate's coverage obligation.
- **Option 3 — exclusion stays, made explicit and pinned (chosen):**
    - Pros: records the layering decision where the gate lives; a pinned service list
      (`smokeCoverageGatedServices` + `TestCatalogCoverageGateIsDeliberatelyUnitScoped`) makes
      scope changes deliberate — the pin fails until this ADR is amended; zero behavior change;
      most reversible.
    - Cons: the ADR-029 seat-map surface keeps unit-only per-operation coverage until someone
      pays Option 2's flip-cost; a reader must follow the ADR reference to learn why.

## Decision

We keep catalog's per-operation coverage gate in its unit suite and pin its exclusion from the
smoke gate. The smoke gate's scope is the explicit `smokeCoverageGatedServices` list
(inventory, commerce, payments, access), asserted exactly by
`TestCatalogCoverageGateIsDeliberatelyUnitScoped`; changing the scope requires amending this ADR
and that pin together. Smoke continues to contract-validate every catalog route it exercises
(29/35 today, per TKT-106).

Why over Option 2: both TKT-109 plan drafters independently recommended keeping the unit layer;
the corrected numbers make the residual gap small and named (5 operations, one surface); the
unit gate already fails closed on new operations, so the marginal value of joining is the
real-store/e2e delta on future unexercised routes — real, but not worth a per-operation smoke
fixture obligation today. Option 2's flip-cost is recorded above so a future ticket can take it
deliberately.

The TKT-106 pin `TestValidateServiceResponseCoversCatalog` needs no rework under either option:
it re-records the coverage entry it deletes via its own `validateServiceResponse` call, so it is
gate-safe even if catalog later joins.

## Consequences

- **Positive:**
    - The layering decision is recorded and mechanically pinned; "is catalog missing from the
      smoke gate?" now has a greppable answer ending in this ADR instead of a re-litigated
      review thread.
    - The smoke gate's scope has a single point of truth; adding any service is a one-line
      change that a failing pin forces through an ADR amendment.
- **Negative:**
    - `editSeatMap`, `updateVenueGaCapacity`, `listVenueSeatMaps`, `getPublicSeatMapGeometry`,
      and `listSeatMapVersions` (the ADR-029 surface) keep unit-only per-operation coverage; a
      real-stack regression there (routing, store, migration) is invisible until a smoke test
      happens to exercise it. Driving those five through smoke remains worthwhile independent of
      which layer owns the gate.
    - Reversal requires an ADR amendment plus the flip-cost above — deliberate friction, by
      design.

## References

- TKT-109 (this decision), TKT-106 (validator allowlist + review finding), TKT-104 / ADR-029
  (the uncovered surface), TKT-47 (gate design; fake-store rationale)
- ADR-009 (contract-first APIs), ADR-028 (response drift fails closed)
- docs/learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md (TKT-105 — the
  limits of non-e2e verification, the counterweight this decision accepts)
