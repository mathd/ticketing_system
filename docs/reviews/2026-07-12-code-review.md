# Code Review — PR #7 (TKT-28 Checkout with fake PSP and signed journal)

**Date**: 2026-07-12
**Scope**: PR #7 diff (`TKT-28-checkout-journal` vs `main`) — commerce checkout coordinator,
payments journal + fake PSP, inventory `finalizing` state, catalog internal offer lookup,
gateway internal-route blocking, storefront checkout form, smoke/US-004 coverage.

## Summary

Strong PR overall: the journal store is careful (canonical bytes signed at persisted µs
precision, replay re-checked under the per-organizer lock, corruption drills in the smoke
gate), the finalize protocol matches ADR-011, and the smoke test exercises success, decline,
fingerprint conflict, timeout-release and crash-recovery replay. The issues found are
concentrated in commerce's failure/concurrency paths: unguarded order-status writes that can
regress a completed order, and an unvalidated payment token that can wedge inventory in a
never-expiring `finalizing` state.

## Strengths

- Journal `Append` double-checks for an existing fact under the `journal_heads FOR UPDATE`
  lock, so concurrent identical appends replay cleanly (verified by `verify-concurrent-append`
  in the smoke gate).
- `smoke.sh` corrupts a real PostgreSQL journal (hash, sequence gap, head) and asserts the
  verifier fails — the verifier is itself tested, not just trusted.
- Prices are resolved server-side from catalog; the browser never supplies an amount, and the
  hold fingerprint includes unit amount + currency so a price change breaks idempotent replay
  instead of silently repricing.
- Gateway 404s `/api/*/internal/` and the smoke test asserts it; catalog additionally requires
  `X-Internal-Token`, so the internal offer route is defended twice.
- Expired-but-still-`held` claims are lazily transitioned to `expired` before any state change
  (`services/inventory/internal/store/store.go:170`), closing the finalize-after-expiry
  oversell path.

## Recommendations

### Critical (Must Fix)

- [x] **R1** — Guard order/reservation status writes against regression: every failure-path
  write in `checkout` is unconditional (`UPDATE orders SET status='payment_unknown' WHERE
  id=$1` at `services/commerce/internal/api/server.go:296`, similarly lines 295, 318, 328–329,
  335). A concurrent duplicate request with the *same* idempotency key (network/proxy retry)
  passes `claimOrder` while the first request is mid-flight, hits payments' operation lease,
  gets 409 "payment operation in progress", and stamps `payment_unknown` — possibly *after*
  the first request stamped `completed`. A captured, confirmed order then reads
  `payment_unknown` until some client happens to replay the exact checkout. Fix: add state
  guards (`... WHERE id=$1 AND status='created'`, reservations analogously), and distinguish
  payments' in-progress/idempotency 409 from a genuinely unknown outcome (return 409/retryable
  instead of recording `payment_unknown`). File: `services/commerce/internal/api/server.go`.

### Important (Should Fix)

- [x] **R2** — Unknown payment token permanently consumes inventory: commerce only checks
  `PaymentToken != ""` (`services/commerce/internal/api/server.go:244`), then finalizes the
  hold *before* charging. Payments rejects an unrecognized token with 400
  (`services/payments/internal/api/server.go:135`), which commerce treats as unknown outcome:
  order `payment_unknown`, reservation `unknown`, hold stuck `finalizing` — and `finalizing`
  never expires. Exact replays re-hit the same 400 forever; a corrected-token retry is a 409
  fingerprint conflict. One bad API call permanently removes capacity from the pool with no
  recovery path. Fix: validate the token against the three fake tokens before finalizing
  (cheap now), or treat payments 400 as provably-no-side-effect and release like a decline.
  Files: `services/commerce/internal/api/server.go`, `services/payments/internal/api/server.go`.

- [x] **R3** — Public unauthenticated `finalize` removes the hold TTL backstop: the gateway
  exposes `POST /api/inventory/holds/{id}/finalize` with no credential. Anyone who creates a
  hold can finalize it and hold capacity forever without paying — before this PR, abuse was at
  least bounded by hold expiry. `confirm`/`release` being public is pre-existing (and the
  smoke test relies on public `confirm`), but `finalize` should be an internal, token-gated
  transition like catalog's `/internal/ticket-types` — or at minimum tracked as an explicit
  follow-up ticket before anything internet-facing. Files:
  `services/inventory/internal/api/server.go:29`, `gateway/cmd/gateway/main.go`.

### Suggestions

- [ ] **R4** — Don't map every reservation-load DB error to 404: `load` failure returns
  "reservation not found" (`services/commerce/internal/api/server.go:248-251`), so a transient
  DB blip mid-checkout tells the buyer their reservation doesn't exist. Branch on
  `sql.ErrNoRows` → 404, everything else → 500/503. Same pattern in `getOrder`
  (`server.go:365-368`).

- [ ] **R5** — Make the catalog internal credential a required constructor parameter:
  `NewServer(st, pub, log, internalCredential ...string)`
  (`services/catalog/internal/api/server.go:42`) uses a variadic to dodge updating call sites.
  There are only two production call sites plus tests; a plain fourth parameter is clearer and
  prevents silently constructing a server whose internal route always 401s.

- [ ] **R6** — Allowlist fields in `paymentFailureResponse`: it copies *every* key from the
  payments response body into the public checkout response
  (`services/commerce/internal/api/server.go:159-168`). Harmless with the fake PSP, but once a
  real PSP adapter sits behind `/internal/charges`, this passes provider internals straight to
  the browser. Copy only `status`/`replay` (plus whatever the storefront actually needs).

- [ ] **R7** — `formatMoney` hardcodes `/ 100` minor-unit scaling
  (`web/storefront/src/components/HoldPicker.tsx:14`). Correct for EUR, wrong for JPY (0
  decimals) or BHD (3). EUR-only is enforced server-side today, so this is future-proofing:
  derive the exponent from the currency or add a comment/assert pinning EUR.

- [ ] **R8** — Test gap: no test covers finalize-on-expired-hold rejection (the guard at
  `services/inventory/internal/store/store.go:170`) or the R1 duplicate-request race. The
  expiry guard is the oversell backstop for the whole protocol — worth a store-level test with
  a short TTL. File: `services/inventory/internal/store/` (new test).

*Sequential numbering R1–R8 across all categories.*
