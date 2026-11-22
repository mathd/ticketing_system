# ADR-088: Back Office upstream calls refuse redirects

Date: 2026-11-22

## Status

Accepted (TKT-602; plan approved under `config.gates: autonomous`, decision log on the ticket).
Closes the S-4 final review finding IMPL2-1.

Scope: the Back Office (`web/backoffice`) only. This ADR is the Back Office counterpart of the
TKT-594 rule for the Commerce refund-outcome clients. It does not state a rule for other
components. The known open instances elsewhere are listed under [Known exceptions](#known-exceptions).

## Context

The Back Office server calls Catalog, Commerce, Inventory and Access. Some calls go through the
gateway (`GATEWAY_URL`). Others go directly to a service under `/internal/` (`CATALOG_URL`,
`COMMERCE_URL`, `INVENTORY_URL`, `ACCESS_URL`). Many calls carry a credential in a custom header:
`X-Catalog-Staff-Write-Token`, `X-Commerce-Staff-Write-Token`, `X-Inventory-Staff-Write-Token`,
`X-Access-Staff-Write-Token`, and the organizer assertion `X-Catalog-Organizer-Assertion`. Staff
sign-in also sends the password in the body. The public ticket-bundle read carries the guest
retrieval reference in its path. That reference is a bearer capability (ADR-012).

Before TKT-602, no Back Office call set a redirect policy. The default `fetch` follows a redirect.
On a redirect to another origin it removes only a small set of standard headers, so the custom
credential headers go to the new address. A 307 or 308 also sends the request body again. TKT-602
executed this on the pinned Node 26.13.1: in each of the ten wire cases of
`web/backoffice/test/upstream-redirects.test.ts`, the redirect target received the request with the
synthetic credential, the password or the guest reference.

A redirect also changed decided outcomes. A redirected refund POST could return the target's
`409 refund_abandoned`, and a redirected abandonment lookup could return the target's confirmation.
Either answer retired the saved retry key of a refund whose outcome was still unknown, against
[ADR-087](ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md) §16.

Precedents:

- TKT-594 refuses redirects in the Commerce refund-outcome clients. Its
  `exactcall.RedirectError` holds only the peer and the status. TKT-594 added no ADR.
- [ADR-039](ADR-039-exchange-settles-the-difference.md), "What counts as an Access answer": the
  Commerce admission caller never follows a redirect, and the redirect target receives no request.
- The scanner-outcomes bundle sends a manual redirect, an opaque response or status 0 to U03
  ([outcome tables](scanner-outcomes/outcome-tables.md), [state model](scanner-outcomes/state-model.md)).

## Possible Solutions

- **Option 1: keep the default redirect policy.**
    - Pros: no change.
    - Cons: credentials and the guest reference can go past the configured first hop. A redirect
      target can settle a saved refund key.
- **Option 2: set a redirect option at each call site.**
    - Pros: a small diff per call.
    - Cons: a new call site can omit it. No check can prove that every call has it.
- **Option 3: a shared helper for credentialed calls only.**
    - Pros: covers the calls that send a credential header.
    - Cons: the public Access ticket-bundle read carries a capability in its path and would stay
      outside. A check based on header names misses a computed name such as Inventory's
      `[headerName]`.
- **Option 4: one manual-mode helper for every Back Office upstream call.**
    - Pros: one rule. A structural check can require exactly one `fetch` in the source tree.
    - Cons: public routes also need a final upstream URL.

## Decision

We adopt Option 4.

1. **One transport.** Every Back Office upstream request uses `fetchUpstream` in
   `web/backoffice/src/lib/upstream.ts`. It contains the only direct `fetch` in
   `web/backoffice/src`. This includes the public Catalog venue and seat-map reads, the Commerce
   order-state read and the Access ticket-bundle read.
2. **Manual mode, refusal before the body.** `fetchUpstream` sends `redirect: 'manual'`, set after
   the caller's `init`, so a caller cannot turn following back on. It refuses a response with type
   `opaqueredirect` or a status from 300 to 399 before any code reads the body. It cancels a present
   body and ignores a failed cancel, so the redirect error reaches the caller. On Node 26.13.1,
   manual mode returns the 3xx itself with type `basic`, so the status range is the guard that
   applies. The `opaqueredirect` check is a low-cost defense against a runtime that returns status 0.
   All other responses go to the existing decoders under the existing deadline.
3. **Status-only error.** The refusal is an `UpstreamRedirectError` with a fixed message and a
   numeric `status`. It holds no URL, `Location` value, header or body, so a guest capability in a
   URL cannot reach a log through this error. `exactcall.RedirectError` is the model.
4. **Caller outcomes stay with the callers**. A refused redirect is a failure to answer:
    - A read is unavailable. The Catalog and Inventory reads throw into their existing unavailable
      paths. The Access ticket bundle is `unavailable`, never `not-found`.
    - Staff sign-in throws, and the login page shows "temporarily unavailable", never "wrong
      password". A direct 401 still means wrong credentials.
    - A write through `fetchMutation` (Catalog POST, Catalog channel PUT, Inventory allocation PUT)
      is an `AmbiguousMutationError` with the redirect as its cause. The first hop received the
      write, so its outcome is unknown.
    - The staff refund POST is `ambiguous`, and the page keeps the original key and its terms.
    - The abandonment lookup returns `false`, so the page keeps the saved key and offers only the
      retry form. A refused redirect is a failed read in the sense of ADR-087 §16. Only a direct
      Commerce answer can settle the key.
    - The ticket resend is `ambiguous` and keeps its retry key.
5. **Existing messages**. A redirect shows the call's existing unreachable or ambiguous
   message, for example "Commerce could not be reached." Known limit: the resend message "access did
   not answer in time." names a timeout also when the cause is a redirect.
6. **Final URLs.** Set `GATEWAY_URL`, `CATALOG_URL`, `COMMERCE_URL`, `INVENTORY_URL` and
   `ACCESS_URL` to their final service URLs with no trailing slash
   ([configuration](../configuration.md#back-office-staff-sign-in)). The clients build paths as
   `${BASE}/path`, so a trailing slash gives `//`. A Go `ServeMux` answers a non-canonical path with
   a redirect; this is standard-library behavior, not a probe of this repository. The call then
   fails closed.
7. **The synchronous boundary stays.** `fetchUpstream` is a regular function that returns the fetch
   promise. A synchronous `fetch` failure still throws before a promise exists, so `fetchMutation`
   keeps a local construction failure definite and a later rejection ambiguous.
8. **Enforcement.** `web/backoffice/test/upstream-policy.test.ts` scans every TypeScript,
   JavaScript and Astro module in `web/backoffice/src`, `.tsx` islands included. It requires exactly
   one direct `fetch`, inside `fetchUpstream`, and reports the path and line of any other. It
   detects bare, qualified (`globalThis.fetch(`) and literal bracket forms. It does not detect an
   alias (`const f = fetch`) or `fetch.call(...)`. The text `fetch(` in a comment or a string also
   counts, which errs toward a false failure.
9. **Islands.** `upstream.ts` reads `process.env` and is a server-only module. A browser island
   that needs its own `fetch` requires an amendment of this ADR.

### Known exceptions

These instances of the same class are outside this rule and have their own tickets:

- Storefront server calls that send the customer assertion: TKT-603.
- The Payments Go Stripe adapter's exact-decoding gap: TKT-604.
- The Commerce Go transports that still follow redirects: TKT-589 and TKT-600.

## Consequences

- **Positive:**
    - A Back Office credential, password, organizer assertion or guest reference goes only to the
      configured first hop.
    - A redirect target cannot settle a saved refund key, confirm an abandonment, or turn a possibly
      committed write into a decided refusal.
    - One function and one structural test hold the rule for future call sites.
- **Negative:**
    - A base URL that redirects, including one with a trailing slash, makes every call through it
      fail: reads are unavailable, writes are ambiguous and staff sign-in is unavailable.
    - The rule bounds where a request goes. It does not authenticate the configured first hop. A
      hostile or compromised first hop still receives what this process sends to it.
    - The structural test has the alias and `fetch.call` gaps stated above.

## References *(optional)*

- TKT-602 (this decision; base-red, mutation and repeat evidence on the ticket), TKT-594 (Go
  counterpart), TKT-580 (S-4 final review, IMPL2-1).
- [ADR-002](ADR-002-services-from-day-one.md) (gateway routes and direct internal calls),
  [ADR-012](ADR-012-ticket-issuance-and-qr-credentials.md) (guest reference),
  [ADR-042](ADR-042-staff-identity-and-backoffice-sessions.md) (staff authentication and sessions),
  [ADR-087](ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md) §16 (saved retry key).
- `web/backoffice/src/lib/upstream.ts`, `web/backoffice/test/upstream-redirects.test.ts`,
  `web/backoffice/test/upstream-policy.test.ts`, `web/backoffice/test/mutation-pages.test.ts`.
