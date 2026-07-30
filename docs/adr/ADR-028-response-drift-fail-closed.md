# ADR-028: Response-schema drift fails closed with 500

Date: 2026-07-17

## Status

Accepted (TKT-47; decision taken under the owner-waived plan gate of that run, recorded on the ticket)

**Amended by TKT-125 (2026-07-27)** — *placement only*, under that run's owner-waived gates. The
fail-closed **semantics** below are unchanged wherever validation runs; what changed is that
"wherever" is now a deployment choice rather than "always" — and the measurement that prompted the
change found no cost worth paying it for, so every environment still runs it. See
§ Amendment (TKT-125).

## Context

Since TKT-46, every service response is validated at runtime against the committed OpenAPI
document (`shared/go/contract`): the middleware buffers the handler response and runs
`openapi3filter.ValidateResponse` before anything reaches the client. Runtime validation is the
only coupling between the strict response schemas and the hand-built handler payloads — the
non-codegen services (inventory, commerce, payments, access) build responses by hand, and even
catalog's codegen types cannot express constraints like enums, minimums, or required locales.

When a response violates its schema, the middleware currently replaces it with a generic
HTTP 500 (`{"error":"response violates OpenAPI contract"}`). TKT-47 asked for an explicit,
recorded choice: keep that fail-closed behavior in production, or log the violation and pass
the drifted payload through. Constraints: the storefront is public and unauthenticated;
commerce and payments carry money paths (ADR-011, ADR-012); the 500 had no log line, so a
contract 500 was indistinguishable from a store 500 for an operator.

## Possible Solutions

- **Option 1 — fail closed (500) + structured drift log (chosen):**
    - Pros: a drifted payload never reaches a client, so a detectable server bug cannot become
      a silent client-side failure after money moved; the existing 500 body is generic and
      non-leaking; a structured log line (method, path, status, validation error, trace ids via
      the request context) makes the 500 diagnosable and alertable.
    - Cons: availability cost — a benign drift (e.g. an added-but-undocumented field under a
      closed schema) takes down that operation until spec or handler is fixed.
- **Option 2 — log and pass through:**
    - Pros: no availability impact from benign drift.
    - Cons: the schema still drifts in production while no client fails loudly — the first
      signal becomes a downstream reconciliation discrepancy or a support ticket; on a money
      path this is the worst failure shape (the buyer acted on a payload the contract says is
      wrong). The "pro" is also the trap: nothing forces the drift to ever be fixed.
- **Option 3 — do nothing (fail closed, no logging):**
    - Cons: rejected — indistinguishable 500s violate the ticket's diagnosability condition.

## Decision

We keep response-schema drift **fail-closed with HTTP 500** in production, and every rejection
emits one structured error log through the service's logger: message
`response violates OpenAPI contract`, with `method`, `path`, `status` (the drifted response's
status) and `error` (the validator detail). The logger is threaded from each service's `main`
into the contract middleware; catalog additionally wires the shared `ResponseValidator` so all
five services enforce the same policy.

## Consequences

- **Positive:**
    - Contract violations surface immediately and loudly, before a client can act on them.
    - Contract 500s are now diagnosable and distinguishable from store 500s in the logs, with
      trace correlation.
    - Every documented 2xx operation is pinned by a happy-path test that drives the real
      handler through this middleware (unit-level for catalog, smoke-level for the DB-backed
      services), with coverage enforced by a post-run gate — drift shows up in the gate, not
      in production. Scope of that gate: it sees only traffic routed through the validating
      test helpers, counts an operation covered by any one of its documented 2xx statuses,
      and exempts operations documented solely via `default:` (none exist today) — see the
      scope notes in the two coverage_test.go files.
    - An undocumented response *status* is treated as drift as well
      (`IncludeResponseStatus`): a handler returning a status its spec does not commit is
      failed closed like a body mismatch. This guards what the inner handler writes;
      statuses written by request-rejection short-circuits (the request validator's own
      error responses) sit outside the response wrap and are not checked **in the four
      non-codegen services** — `shared/go/contract.requestValidator` nests
      `responseValidated` *inside* the request validator, so a rejection short-circuits
      before the wrap is reached. **The carve-out does not hold for catalog** (corrected by
      TKT-110): catalog's `NewRouter` wraps `contract.ResponseValidator` around the whole
      generated `HandlerWithOptions` from the outside, so response validation is its
      **outermost** layer and every rejection written beneath it — the request validator's
      *and* the oapi-codegen binder's — is checked. That is not a theoretical difference:
      nine lifecycle operations declared no `'400'`, and the binder's 400 for a malformed
      path UUID reached clients as this decision's generic 500 (plus an ERROR log) until
      TKT-110 declared it. **The rule for catalog is therefore stricter than this bullet
      originally implied: any status the binder or the request validator can write must be
      declared.** `TestUUIDPathOperationsDeclareBadRequest` enforces the `format: uuid` case
      of that rule at the spec level.
- **Negative:**
    - A schema mistake (over-strict spec) is a production outage for that operation until
      corrected; the tests above are the mitigation, not a guarantee.
    - Response buffering (`httptest.Recorder`) stays on every documented route — acceptable at
      this testbed's payload sizes; revisit if streaming or large responses arrive.

## Amendment (TKT-125, 2026-07-25) — where fail-closed applies

### What changed, and why the placement moved

The decision above says *what* happens to a drifted response. It said nothing about *where* the
check runs, so it ran everywhere: `shared/go/contract.responseValidated` wrapped every handler in
all five services and, for every request, buffered the whole response into an
`httptest.ResponseRecorder`, re-routed the request against the OpenAPI document
(`router.FindRoute`), re-parsed the recorded body and walked it against the response schema, then
replayed headers and body onto the real `ResponseWriter`.

That is the cost profile of a **test** placed in a **hot path**. It ran on `POST /holds` and the
three hold transitions — the claim path ADR-010 already serializes under a row lock, and the exact
path `smoke/onsale_load_test.go` holds to a p99 SLO.

`OPENAPI_RESPONSE_VALIDATION_ENABLED` (`runtimecfg.ResponseValidationFromEnv`, **default `true`**)
now selects it. **Request** validation is untouched and remains unconditional: it is a trust
boundary, and its cost is bounded by the request body.

### The measurement — taken, and it found nothing

`smoke/onsale_load_test.go` at the `nfr-3000pm` stage (3,000 attempts/min × 180 s = 9,000
lifecycles per run), validation on vs off, on a quiet host, with the off arm's setting verified
inside the running container:

| Run | Validation | hold p99 | lifecycle p99 |
|---|---|---|---|
| A | on | 4.7 ms | 9.1 ms |
| B | on | 8.1 ms | 17.0 ms |
| C | **off** | 5.1 ms | 10.6 ms |

**The off run falls between the two on runs.** Two runs of the same configuration differ by +72% on
hold p99, so the off arm's 5.1 ms is 8.5% slower than one on-run and 37% faster than the other — the
sign of the "effect" depends on which same-configuration run you compare against. Against the
materiality bar pre-registered before any run (≥10% or ≥50 ms), the result is **not material**.

So the placement change ships as a **lever, not a remedy**. `OPENAPI_RESPONSE_VALIDATION_ENABLED`
defaults to `true` and **no environment sets it to false**, including production: this ADR records
no measured justification for disabling response validation anywhere. The switch exists so a future
topology that *does* measure a cost has somewhere to turn it off, and so ADR-028's runtime scope is
an explicit decision rather than an accident of where the middleware was mounted.

Full method, the discarded runs, and a stated flaw in the pre-registered bar (its 10% relative arm
is unusable when the whole p99 is under 10 ms): `docs/evidence/TKT-125/README.md`. Per-mutation
finalize/confirm p99 are not reported separately there — the harness emits them only in a JSON
report that a late-stage abort makes unreachable; lifecycle p99 covers them in aggregate.

### What is and is not guaranteed — and against whom

Stated to the discipline [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) requires of this
repo, because "the responses are validated" is exactly the kind of sentence that sounds like a
security property and is not one:

- This is an **honest-implementation consistency control, not tamper-evidence.** It catches a
  handler whose *own* output disagrees with the committed spec. It constrains a **bug**, never an
  adversary.
- It does **not** constrain anyone who can change service code, change the served OpenAPI document,
  set this variable, or produce data that is malicious but schema-valid. All of those defeat it
  completely, and none of them is detectable by it.
- It says nothing about **semantic** correctness. A response with the right shape and the wrong
  numbers passes.
- **Where enabled**, the guarantee is exactly: a response reaching this middleware that fails the
  committed schema or status validation is replaced by the generic 500 before reaching the client,
  and the violation is logged.
- **Where disabled**, there is **no runtime response-contract guarantee at all** — a drifted payload
  reaches the client unchanged, and nothing is logged. This is the property being traded away, and
  it should be written down as a loss rather than described as an optimisation.

The **gate-time** guarantee is unaffected by the switch (it is on by default, and neither `make
check` nor CI overrides it), but it was never as broad as it sounds and the amendment does not widen
it: catalog's per-operation coverage is unit-level against a fake store
([ADR-030](./ADR-030-catalog-coverage-gate-scope.md)); the other four services rely on the smoke
coverage gate, which counts an operation covered by any **one** of its documented 2xx statuses.
Neither proves every documented status, and neither exercises production data.

### Buffering and streaming

The original ADR listed response buffering as an accepted cost and said "revisit if streaming or
large responses arrive". That revisit is **not** this ticket, and the cost is now conditional:

- **Enabled**: still buffered, and therefore still incompatible with streaming — deliberately.
  Fail-closed *requires* withholding the response until the validator has accepted it; a handler
  that flushed would have already sent bytes the validator might reject. The recorder also hides
  `http.Flusher` and `http.Hijacker` from the handler, so nothing behind it can stream today.
- **Disabled**: the handler is given the real `ResponseWriter`, so those interfaces are restored.

A streaming route therefore needs a route-level exclusion or a different validation contract, decided
when one exists. It is not an adapter fix.

## References

- TKT-47, TKT-46 (PR #10 review finding); TKT-125 (the placement amendment)
- ADR-009 (contract-first APIs) — this ADR fixes the runtime response half of that regime.
- ADR-021 — the claim discipline the amendment's guarantee section follows.
- ADR-030 — the scope limit on catalog's half of the gate-time guarantee.
