# Dependencies & versions

## Latest stable by default (owner directive, 2026-07-12)

This is a greenfield platform: every language, runtime and dependency targets its **latest stable
release**, looked up at implementation time — never assumed from memory. Applies to Go, Node,
TypeScript, Vite, PostgreSQL, Redis, NATS, GitHub Actions, linters, everything. Pinning behind
latest requires a stated compatibility reason, recorded next to the pin.

Current anchors (update as they move): Go 1.27.0 · Node 24.18.0 · pnpm 11 (pinned via `packageManager`,
which also enforces pnpm's supply-chain `minimumReleaseAge` policy) · PostgreSQL 18.4 · Vite 8 ·
React 19 · TypeScript 6.

## Executable dependency pins

Container base images use an exact release tag and multi-architecture manifest digest. GitHub
Actions use a full commit SHA with the corresponding release tag in a comment; tools installed by
an action also declare their exact version. This makes a reviewed commit reproducible even when an
upstream tag moves.

Dependabot checks all eight Go modules, the root pnpm workspace, GitHub Actions, Compose, and every
Dockerfile directory weekly. An update PR must preserve both the human-readable version and its
matching immutable digest or SHA. Verify the pair against the upstream project's official registry
or release before merging; do not copy a digest between architecture-specific and multi-platform
manifests.

## One version per shared Go dependency (TKT-129, [ADR-035](../adr/ADR-035-go-module-dependency-declarations.md))

The eight Go modules ship as one deploy unit, so any dependency declared by two or more of them
declares **one** version. `make check` enforces it (`check-dep-drift`); a dependency only one module
imports is exempt. Realign with `go work sync` and commit the resulting `go.mod`/`go.sum` changes —
`go mod tidy` will not do it, having been measured as a no-op on a drifted tree.

**A manifest must also not declare *below* what the workspace selects** (TKT-265, ADR-035
§Amendment). MVS can raise a selected version through a transitive requirement without any `go.mod`
line changing, which leaves every manifest internally consistent while describing a build that is
not what happens — `check-dep-drift` cannot see it, because the versions all agree with each other
and simply agree on the wrong number. `make check` enforces this second direction separately
(`check-build-list-lag`). The repair is the same `go work sync`; what differs is that this check
resolves the module graph, so it **needs the module cache or the network** and fails closed rather
than reporting "no lag" when it cannot. An *absent* declaration is not lag — a module that does not
import a dependency should not pin it.

Dependabot opens one PR per module directory, so a bump to a shared dependency lands in one module
at a time and **will fail the gate** until the siblings are raised. That is the check working: add
a `go work sync` commit to the PR.

## Go HTTP conventions (TKT-25 plan gate, 2026-07-12)

- **Services route with chi** (v5+): net/http-compatible, stdlib handlers, standard middleware
  (incl. otelhttp) works unmodified; subrouters + per-group middleware cover what stdlib ServeMux
  lacks (tenant extraction, auth groups, ADR-004 cache-header layers).
- **The gateway stays bare stdlib** (`httputil.ReverseProxy` + an explicit route table) — it is a
  route table, not an API.
- Rejected: Fiber (fasthttp breaks the net/http + OTel ecosystem), Gin/Echo (custom context types,
  no added value over OpenAPI codegen).
- All cross-service calls go through `obs.Client()` (W3C trace propagation); all servers wrap
  `obs.Middleware` + `obs.RequestLogger`.
- **Every outbound client is bounded, and the bounds nest** (TKT-116). Innermost first:
  recovery → payments **10s** (`services/commerce/cmd/commerce/main.go`), payments → Stripe **15s**
  (`psp.NewStripe`'s default), any service → peer **30s** (`obs.Client()`), and commerce's recovery
  grace period **120s** (`services/commerce/internal/store/recovery.go`). The grace period is the
  one that matters: an internal call that outlives it lets a live checkout and the recovery runner
  act on the same order. A call site needing something stricter still passes a per-request context —
  the gateway's health fan-out uses 2s. Never hand a money-path adapter `http.DefaultClient`; it has
  no timeout.
