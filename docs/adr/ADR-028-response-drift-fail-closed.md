# ADR-028: Response-schema drift fails closed with 500

Date: 2026-07-17

## Status

Accepted (TKT-47; decision taken under the owner-waived plan gate of that run, recorded on the ticket)

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
      error responses) sit outside the response wrap and are not checked.
- **Negative:**
    - A schema mistake (over-strict spec) is a production outage for that operation until
      corrected; the tests above are the mitigation, not a guarantee.
    - Response buffering (`httptest.Recorder`) stays on every documented route — acceptable at
      this testbed's payload sizes; revisit if streaming or large responses arrive.

## References

- TKT-47, TKT-46 (PR #10 review finding)
- ADR-009 (contract-first APIs) — this ADR fixes the runtime response half of that regime.
