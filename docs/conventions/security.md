# Security guidelines

This page separates controls enforced in the current repository from future production requirements.
The system is a local Docker Compose testbed, not a production deployment.

## Implemented controls and repository rules

This list combines runtime controls with rules checked by tests, gates, or review. A rule that does
not name a mechanical check still depends on review.

- Secrets and local environment files are ignored. No service credential is checked in:
  `INTERNAL_SERVICE_TOKEN` has no default, `make up` generates a local one into `.env`, and
  server mode refuses the retired historical value `local-service-token` everywhere, dev
  included (TKT-83).
- Each Go service has a separate PostgreSQL database and role. CONNECT is revoked across service
  databases and the smoke suite verifies credential isolation (ADR-007).
- Base `compose.yaml` publishes only the gateway. Development and test overlays publish
  infrastructure and service ports on `127.0.0.1` for local diagnostics.
- The gateway exposes an explicit route table; `/internal/` service routes are denied publicly.
- Go service images use a distroless non-root runtime. Dependency lockfiles and module checksums are
  committed. Container images are pinned by exact tag and multi-platform digest; workflow actions
  are pinned by full commit SHA.
- OpenAPI request validation guards declared public operations. Service tests validate response
  conformance; the uniform bounded, strict JSON decoding policy remains future work.
- Catalog owns staff accounts. The back office requires a session for `/admin/*`, enforces the
  admin, box-office, and finance role matrix, scopes its session cookie to `/admin`, and checks the
  public origin on unsafe requests. Unsafe catalog operations require a catalog-only credential;
  tenant-scoped writes also use a signed organizer assertion minted from the staff session
  (ADR-042, ADR-058).
- Inventory allocation writes and Commerce refunds use their own staff credentials. Their
  organizer and actor values come from the authenticated back-office session, not the submitted
  form (ADR-057, ADR-042 as amended by TKT-194).
- Access requires an enrolled scanner-device token for connected scans and reconciliation, and
  checks that the device belongs to the ticket's organizer. Only a paired browser scanner can
  capture offline occurrences; it stores them locally until an authenticated reconciliation
  request succeeds (ADR-025).
- In-process rate limiters bound staff sign-in, public customer identity and recovery operations,
  and the public reservation/order write path. These limits constrain one scripted source against
  one replica. They reset on restart and are not a distributed waiting room (ADR-051, ADR-055).
- Payments appends money facts to its journal. Access sends every lifecycle event through its
  chained append path. These controls detect the modification and insertion threats defined in
  ADR-021; they do not close its documented rollback or current-key-compromise gaps.
- Repository rule: logs must not contain payment tokens, QR credentials, raw email addresses, or
  hostile failed-event bodies. Access logs only recipient/link hashes and publishes sanitized
  failure records.
- `make check` runs lint, tests, builds, and real-stack smoke checks in CI and locally.
- The `security` workflow runs on every pull request, weekly, and on demand. It fails on reachable Go
  vulnerabilities, high-or-critical pnpm advisories, or high-or-critical Trivy filesystem findings
  across dependencies, IaC configuration, and detected secrets.
- An accepted Trivy or pnpm finding goes in `.trivyignore.yaml`, with a reason, an owner and an
  expiry of 30 days or less, and only with the owner's approval. An expired entry fails the
  `pnpm-audit` job when it runs: on every scheduled run, and on a pull request that changes
  dependency files or `.trivyignore.yaml`. Trivy drops an expired entry silently, so
  `repository-scan` fails again only if the advisory still exists. govulncheck has no exception
  path yet (TKT-524). See
  [ADR-081](../adr/ADR-081-reviewed-expiring-vulnerability-scan-exceptions.md).

## Input and data handling

- Use parameterized SQL through `database/sql`/pgx; never interpolate untrusted values into SQL.
- Until the repository has a uniform bounded, strict JSON decoder, keep each endpoint's existing
  size, shape, and status controls explicit. Adopt the shared policy when it becomes available
  without changing endpoint-specific status semantics.
- Money is integer minor units plus currency. Do not log or serialize money through floating point.
- Keep PII in mutable service-owned stores and identifiers in immutable trails (ADR-003).
- Treat guest ticket references and QR payloads as bearer capabilities. Never include them in logs,
  metrics labels, failed-event records, or analytics events.

## Dependencies and executable inputs

Review maintenance, license, scope, and advisories before adding a dependency. Pin application
dependencies through Go modules and `pnpm-lock.yaml`; pin executable container/workflow inputs as
described in [`dependencies-and-versions.md`](dependencies-and-versions.md). Dependabot proposes
weekly updates but does not bypass review or the quality gates. Security findings are blocking at
the thresholds above; suppressions require a narrow, documented rationale with an expiry or removal
condition. Suppress a Trivy or pnpm vulnerability only with a reviewed `.trivyignore.yaml` entry
([ADR-081](../adr/ADR-081-reviewed-expiring-vulnerability-scan-exceptions.md)). `make check` refuses
a committed `trivy.yaml`, `trivy.yml`, `trivy-secret.yaml`, `.trivyignore`, `.pnpmfile.*` or
`.npmrc`, a hand-made `auditConfig` ignore list, and a `pnpm-workspace.yaml` setting outside the
allowed set. The `pnpm-audit` job runs `pnpm audit` through `scan-allowlist.mjs audit`, with a
minimal environment, the pnpmfile hooks off, and the registry and the lockfile directory pinned,
so no runner setting can change what it reports. Trivy reads `TRIVY_*` variables from the runner;
GitHub-hosted runners set none, and a self-hosted runner that sets one is outside this control.

### Accepted risk: CVE-2026-93748 in `http-cache-semantics` (TKT-504)

This risk has no `.trivyignore.yaml` entry. No scanner reports it, so an entry has nothing to
suppress. An entry with no finding would also hide a later change to the advisory range.

- **The advisory.** CVE-2026-93748 is GHSA-ch52-4w7c-c8xp. It concerns the `max-stale` handling.
  The advisory lists affected versions through 4.2.0 and lists no patched version. (`pnpm audit`
  shows ">=4.2.1" as patched. It derives that value from the affected range.) Astro 7.3.8 resolves
  4.3.0. The scanners do not report 4.3.0. The TKT-504 code review executed a probe against 4.3.0.
  With `max-stale` in the request, the cache policy returned the cached response, with its
  `Set-Cookie` headers, although the response had a zero cache lifetime. We treat 4.3.0 as
  vulnerable. No fixed release exists.
- **The only consumer.** Astro imports the package in one file:
  `astro/dist/assets/build/remote.js`. That file is the remote image loader. It runs at build time.
  Astro's image pipeline calls the loader only for a remote image that `image.domains` or
  `image.remotePatterns` allows. The loader itself does not check the initial URL. It checks only
  the redirect targets. The TKT-504 code review executed the loader with empty allowlists: it
  fetched the URL and made a `CachePolicy`. Astro calls only `new CachePolicy()`, `storable()` and
  `timeToLive()`. It does not call `evaluateRequest()`, which holds the `max-stale` logic. We read
  this in the Astro 7.3.8 and `http-cache-semantics` 4.3.0 code. We did not execute it.
- **The runtime images.** The built `dist` of the back office and the storefront contains no copy of
  the package. The runtime images ship `dist` only.
- **The guard.** `make check-astro-image-premise` runs `scripts/check-astro-image-premise.mjs` in
  `make check`. The guard evaluates each app config two times: with `NODE_ENV` unset, and with
  `NODE_ENV=production`, which `astro build` sets before it loads the config. The guard fails if an
  app allows remote images, sets an image service, adds an integration other than `@astrojs/react`,
  or uses an adapter other than `@astrojs/node`. It also fails if app source imports the Astro image
  pipeline, imports `http-cache-semantics`, or imports a module under `astro/dist/`. The last rule
  exists because the loader does not check the initial URL. The guard does not run the hooks of the
  allowed integration and adapter. We read the `updateConfig` calls in `@astrojs/react` 6.0.6 and
  `@astrojs/node` 11.1.7. They change only the Vite config and the image endpoint. They add no
  remote image domains or patterns. We did not execute them. The guard does not read dependency
  code. The script lists its other limits.
- **Removal.** Remove this paragraph when a fixed release of `http-cache-semantics` is available
  and installed. Review the risk again when the guard fails, and at each Astro upgrade.

## Not yet production-ready

The implemented identity and rate-limit controls fit a single-replica Compose testbed. Production
still needs a durable shared staff session store, an external identity policy such as SSO and MFA,
distributed abuse controls, and a waiting room for high-contention sales. Ticket expiry,
revocation, and admission-window policy also remain incomplete.

Payments has a Stripe adapter for test-mode keys, but this repository rejects live keys. Managed
secrets, production PSP operation, service-to-service identity beyond the current bearer-token
model, cloud network isolation, backups, disaster recovery, a WAF, and an application release
pipeline do not exist. Settle those controls before external operation. Never reuse local Compose
passwords or signing seeds outside development.

## AI-assisted development

- Do not place real secrets, customer data, or production credentials in prompts or fixtures.
- Review generated edits and commands before accepting them; the human owner retains accountability.
- Preserve repository and tool permission boundaries. Destructive or external actions require the
  authority defined by the active workflow.
- Validate security suggestions against current ADRs and code; generic framework advice is not a
  project control.
