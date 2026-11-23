# ADR-088: Back Office upstream calls refuse redirects

Date: 2026-11-22

## Status

Accepted (TKT-602; plan approved under `config.gates: autonomous`, decision log on the ticket).
Closes the S-4 final review finding IMPL2-1.

Scope: the Back Office (`web/backoffice`) only. This ADR is the Back Office counterpart of the
TKT-594 rule for the Commerce refund-outcome clients. It does not state a rule for other
components. The known open instances elsewhere are listed under [Known exceptions](#known-exceptions).

**Amended by TKT-607 (2026-11-23)** — *the known exceptions list only.*

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
   `web/backoffice/src/lib/upstream.ts`. It contains the only reference to `fetch` in the
   first-party code of `web/backoffice`. This includes the public Catalog venue and seat-map reads, the Commerce
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
8. **Enforcement.** `web/backoffice/test/upstream-policy.test.ts` scans all first-party runtime
   and configuration code of `web/backoffice`: every TypeScript, JavaScript and Astro file in the
   package, including `src` with its `.tsx` islands, the production entry point `start.mjs`,
   `credentials.mjs`, `astro.config.mjs` and any new directory. Only `test`, `dist`,
   `node_modules` and the generated `.astro` directory at the package root are outside. A
   directory with one of these names deeper down, such as `src/lib/test`, is scanned. The scan
   parses each file with
   the parsers that the build uses: the Astro compiler for `.astro` files (frontmatter, template
   expressions and `<script>` bodies) and the rolldown parser for scripts. Comments, page text and
   JSX text therefore cannot hide or fake a reference. It requires exactly one reference to
   `fetch`, inside `fetchUpstream`, and reports the path and line of any other. A reference is an
   identifier named `fetch` or a string or plain template literal equal to `fetch`. This covers
   direct and optional calls (`fetch?.(`), `globalThis.fetch` and `globalThis['fetch']`,
   `fetch.call`, `fetch.apply` and `fetch.bind`, an alias (`const f = fetch`), `fetch` passed as a
   value and an imported `fetch`. An object key or a type named `fetch` also counts, which errs
   toward a false failure. A file that does not parse fails the scan. Each form has a case in the
   test.

   The same test also refuses every other outbound transport in first-party code. No file may load
   `http`, `https`, `http2`, `net` or `tls`, with or without the `node:` prefix, `undici`, or a
   common HTTP client package (`axios`, `got`, `node-fetch`, `ky`, `superagent` and the others in
   the test's list), also by a subpath. The scan finds a static import or re-export, a side-effect
   import (`import 'node:https'`), `import x = require()`, a dynamic `import()`, `require()`,
   `module.require()`, `createRequire(...)()` and `process.getBuiltinModule()`, also with the
   member name in brackets (`process['getBuiltinModule']()`). It also refuses a module load whose
   name is not a literal, because it cannot tell what that load gives. This computed-load rule
   fails closed for the whole package, also for a load that is not about HTTP; the decision is
   recorded on the ticket. A declaration that the build removes loads nothing and does not count:
   `import type`, `export type ... from` and `export type * from`. An import whose every specifier
   is a type (`import { type Agent } from 'node:https'`) still counts: the package's TypeScript
   configuration sets `verbatimModuleSyntax`, so the build keeps it as `import 'node:https'`. A
   string that only equals a module name, such as `'https'`, is not a load and does not count. Each
   form has a case in the test.

   Known limits: the scan does not see a name built at run time (`globalThis['fe' + 'tch']`), a
   loader reached through an alias (`const r = require`), or a transport that is not in its list.
   It does not see a request that a dependency makes with its own `fetch` or HTTP client.
9. **Islands.** `upstream.ts` reads `process.env` and is a server-only module. A browser island
   that needs its own `fetch` requires an amendment of this ADR.

### Known exceptions

These instances of the same class are outside this rule and have their own tickets:

- Storefront server calls that send the customer assertion: TKT-603.
- The Payments Go Stripe adapter's exact-decoding gap: TKT-604.
- TKT-589 tracks the Commerce exchange unwind client that still follows redirects.
- TKT-600 moves the Commerce exchange abandonment and Access admission callers to `exactcall`.
  Both callers already stop at the first redirect response and then read its body. `exactcall`
  refuses a redirect before it reads the body.

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
    - The structural test does not see a name built at run time, a module loader reached through an
      alias, a transport outside its list, or a request made inside a dependency, as stated above.

## References *(optional)*

- TKT-602 (this decision; base-red, mutation and repeat evidence on the ticket), TKT-594 (Go
  counterpart), TKT-580 (S-4 final review, IMPL2-1).
- [ADR-002](ADR-002-services-from-day-one.md) (gateway routes and direct internal calls),
  [ADR-012](ADR-012-ticket-issuance-and-qr-credentials.md) (guest reference),
  [ADR-042](ADR-042-staff-identity-and-backoffice-sessions.md) (staff authentication and sessions),
  [ADR-087](ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md) §16 (saved retry key).
- `web/backoffice/src/lib/upstream.ts`, `web/backoffice/test/upstream-redirects.test.ts`,
  `web/backoffice/test/upstream-policy.test.ts`, `web/backoffice/test/mutation-pages.test.ts`.
- `test/browser/refund-redirected.mjs`: host Chrome submits the saved refund retry while the Back
  Office's refund POST is redirected, and looks the order up while its staff order detail read is
  redirected. The page keeps the saved retry with its original key and terms, and the redirect
  target receives no request.
