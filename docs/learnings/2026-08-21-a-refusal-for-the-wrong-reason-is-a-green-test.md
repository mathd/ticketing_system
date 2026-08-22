# A refusal for the wrong reason is a green test that cannot fail

**TKT-257, 2026-08-21.** Four defect classes, all in test code, none visible to `lint`+`test`+`build`.

## The shape

TKT-257's load-bearing acceptance criterion was *"write-path tests where the Stripe response
DISAGREES with the request, asserting fail-closed"*. Three of those tests asserted the charge endpoint
answers **502**. They passed review, passed vetting, and were pushed.

In CI they failed with **400** — because the settlement plan in the request body was written as an
array and the real schema wants an object. The plan is validated against the OpenAPI schema *before*
the provider is ever called, so the request never reached the guard the test existed to prove. Had the
assertion been `res.Code != http.StatusOK` — a shape that is one careless edit away — the test would
have been **green**, permanently, while proving nothing.

This is the fixture-cannot-reach-the-failing-state rule
([2026-08-10](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md)) with a new face. There
the fixture was too *small*; here it was **invalid**, and the invalidity produced a refusal that
*looked exactly like the guard working*. A test that asserts "the write was refused" cannot tell you
**which** refusal it observed, and on a path with input validation in front of the guard, the
validator will answer first every time.

## The rule

**A fail-closed test needs an agreement twin over the same request body.** The twin is what fails
when the body is malformed: it asks for a 200 and gets the 400, immediately and unambiguously. Without
it, the disagreement tests are unfalsifiable — every possible defect in the fixture produces the same
non-200 the test was hoping for.

Corollary, and the reason this generalises past HTTP: **when the mechanism under test sits behind a
validator, a refusal is ambiguous evidence.** Assert the *specific* status, and assert what the write
left behind — the journal rows, the settlement rows, the operation's status — not merely that
something was refused.

## The other three, briefly

All four were found by CI in two rounds, and all four were invisible to the local gate because this
machine has no Docker and no PostgreSQL server:

- **An ambiguous SQL parameter.** A test seed derived two confirmation columns in SQL from an amount
  parameter, reusing `$7` (a `varchar(3)`) in a second position with a different target type.
  PostgreSQL refused the whole statement — `inconsistent types deduced for parameter $7` — taking
  down **twelve** tests, five of which predated the ticket. Resolve such values in Go and pass plain
  parameters; a `CASE` expression over a reused parameter is a type puzzle the planner may refuse.
- **A missing grant.** The migration tests created their own throwaway databases. The `payments` role
  has no `CREATEDB`, so all three failed in CI while passing every local check. The repo already had
  the answer — `settlement_legacy_smoke_test.go` uses a database `scripts/smoke.sh` provisions as
  superuser and passes in by env var, for exactly this need (a database migrated only part-way).
  **Look for the existing precedent before building the mechanism**; the version that built its own
  mechanism also cost two full review passes over a DSN-rewriting helper that the precedent made
  unnecessary.
- **An undeclared response code.** The new fail-closed refusal answers 502, which `/internal/charges`
  never declared. The response validator rejected it and the caller got a 500 reading *"response
  violates OpenAPI contract"*. That validator only runs against a live server — no amount of unit
  testing reaches it. **A new status code on a contract-first endpoint is a spec change, not just a
  handler change.**

## Why it is worth a note

The AGENTS.md rule about the smoke and browser tiers already exists. This is the strongest concrete
evidence for it so far: the *product* change in TKT-257 was correct on the first push and never
changed. Every CI failure was in the harness — and a harness that cannot run is a harness whose
defects are indistinguishable from success.
