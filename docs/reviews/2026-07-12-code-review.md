# Code Review

**Date**: 2026-07-12
**Scope**: PR #7 — "TKT-28 Checkout with fake PSP and signed journal" (branch `TKT-28-checkout-journal`, 34 files, +1393/−63)

## Summary

Solid walking-skeleton checkout: the money-path invariants (server-authoritative pricing, integer
minor units, per-organizer hash-chained + HMAC-signed journal, append-only triggers, PII split into
a deletable table) are all real and enforced at the DB layer, not just in prose. The main problems
are in the recovery half of the protocol: ADR-011 promises "an exact checkout replay resumes the
idempotent protocol", but several crash/replay states cannot actually be resumed, and one decline
path can panic. The 342-line commerce coordinator — the most intricate state machine in the PR —
ships with no unit tests.

## Strengths

- Journal design is genuinely tamper-evident: per-organizer chain head locked `FOR UPDATE`,
  entry + head advance in one transaction, `verify-journal` wired into the smoke gate before
  teardown, and DB triggers rejecting UPDATE/DELETE/TRUNCATE.
- The microsecond-truncation of `occurred_at` before signing (matching timestamptz precision) is
  exactly the kind of round-trip bug most implementations hit later; caught up front with a comment.
- Prices resolved from catalog server-side; browser amounts never trusted; quantity-overflow
  guard on `amount * quantity`.
- Journal payload validation is default-deny (only `order_id` allowed) with an explicit PII
  denylist on top.
- Inventory capacity math consistently counts `finalizing` claims in both `CreateHold` and
  `Availability`.

## Recommendations

### Critical (Must Fix)

- [x] **R1 — Checkout replay cannot resume from several states, contradicting ADR-011**: In
  `checkout`, only `orderStatus == "completed"` short-circuits; every other replay re-runs the full
  flow starting at hold finalize. Two broken cases: (a) replay of a **declined** checkout — the hold
  is `released` (terminal), so finalize returns 409 and the client gets `hold expired` instead of
  the stored 402 result; (b) crash **after hold confirm succeeds but before the completion persist**
  — the hold is `confirmed`, finalize returns `ErrConflict`, and the order is permanently stuck in
  `created`/`finalizing` with money captured. Fix: branch on the stored order status in `checkout`
  (replay terminal results directly; for in-flight states, resume at the correct step instead of
  from the top), and/or make inventory `finalize` idempotent when the claim is already `confirmed`.
  File(s): `services/commerce/internal/api/server.go` (checkout, claimOrder),
  `services/inventory/internal/store/store.go` (Transition).

- [x] **R2 — Stuck `payment_operations` are unrecoverable ("in progress" forever)**: `BindOperation`
  inserts the row, then journal appends and `CompleteOperation` each run in their own transactions.
  A crash or error between bind and complete leaves `status IS NULL` permanently; every replay then
  returns "payment operation in progress" (409), commerce maps it to `payment_unknown`, and no path
  ever completes the operation — the retry-driven recovery ADR-011 relies on cannot converge. Fix:
  run bind + append(s) + complete in a single transaction (the appends are already idempotent by
  fact ID, so this is safe), or make the "in progress" check lease-based so an exact replay can take
  over and finish a dangling operation.
  File(s): `services/payments/internal/store/store.go` (BindOperation, CompleteOperation),
  `services/payments/internal/api/server.go` (charge).

- [x] **R3 — nil-map panic on the decline/timeout response path**: In `checkout`, after a 402/408
  from payments: `var out map[string]any; _ = json.Unmarshal(body, &out); out["order_id"] = order`.
  If the payments body is empty or not a JSON object (proxy error, truncated response), the ignored
  unmarshal error leaves `out` nil and the assignment panics — and there is no Recoverer middleware
  on this router. Fix: check the unmarshal error and initialize the map (`out = map[string]any{}`)
  on failure. File(s): `services/commerce/internal/api/server.go`.

### Important (Should Fix)

- [x] **R4 — Unauthenticated "internal" catalog endpoint is publicly reachable**:
  `GET /internal/ticket-types/{id}` is registered with no auth, and the gateway proxies
  `/api/catalog/*` with the prefix stripped, so `GET /api/catalog/internal/ticket-types/{id}` works
  from the public internet. Payments guards its internal routes with `X-Internal-Token`; catalog
  should do the same (commerce already has the token — pass `internal: true` on that call), or the
  gateway should refuse to proxy `/internal/` paths. File(s):
  `services/catalog/internal/api/server.go`, `services/commerce/internal/api/server.go`,
  `gateway/cmd/gateway/main.go`.

- [x] **R5 — No tests for the checkout coordinator or journal tamper detection**: The commerce
  coordinator (multi-step state machine, the highest-risk code in the PR) has zero unit tests; smoke
  covers only success + decline. Untested: `fake-timeout`, transport-error → `payment_unknown`,
  `confirmation_pending`, idempotent replay of each terminal state, conflicting key/fingerprint
  reuse (409), and `Journal.Verify` never sees a tampered/gapped chain — the only verification
  exercised is the happy path. Add commerce handler tests with fake downstream servers
  (`httptest`), and a store-level Verify test that mutates an entry/sequence and asserts failure.
  File(s): `services/commerce/internal/api/`, `services/payments/internal/store/store_test.go`,
  `smoke/us004_test.go`.

- [ ] **R6 — Commerce 409s leak internal error strings**: `claimOrder` errors are written verbatim
  (`err.Error()`) to clients. When the same organizer reuses an idempotency key across different
  reservations, the derived order UUID collides and the raw Postgres duplicate-key error is
  returned to the browser. Map to a stable, safe message and log the detail. File(s):
  `services/commerce/internal/api/server.go` (checkout → 409 branch).

### Suggestions

- [ ] **R7 — Drop the custom `reserveRequest.UnmarshalJSON`**: It only copies fields a plain
  struct with json tags would handle, and it silently defeats the outer decoder's
  `DisallowUnknownFields`. Use a plain struct for the body and keep the key out-of-band. File(s):
  `services/commerce/internal/api/server.go`.

- [ ] **R8 — Money display in the storefront**: `{reservation.amount / 100}` renders raw float
  division (e.g. `12.1000000...` risk classes, no currency formatting). Use
  `Intl.NumberFormat(locale, {style:'currency', currency})` on the minor units. Also remove the
  now-unused `slotId` prop from `Props`/the Astro call site. File(s):
  `web/storefront/src/components/HoldPicker.tsx`,
  `web/storefront/src/pages/[locale]/events/[eventId].astro`.

- [ ] **R9 — Duplicate concurrent journal appends surface as errors instead of replays**: In
  `Journal.Append`, the fact-ID existence check runs before the head lock, so two concurrent
  appends of the same fact both pass the check and the loser hits the `fact_id` PK violation,
  which commerce reports as 503 "journal unavailable". Self-heals on retry, but re-checking (or
  catching the unique violation and re-reading) inside the locked section would make it a clean
  replay. File(s): `services/payments/internal/store/store.go`.
