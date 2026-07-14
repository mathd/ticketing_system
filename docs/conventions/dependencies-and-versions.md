# Dependencies & versions

## Latest stable by default (owner directive, 2026-07-12)

This is a greenfield platform: every language, runtime and dependency targets its **latest stable
release**, looked up at implementation time — never assumed from memory. Applies to Go, Node,
TypeScript, Vite, PostgreSQL, Redis, NATS, GitHub Actions, linters, everything. Pinning behind
latest requires a stated compatibility reason, recorded next to the pin.

Current anchors (update as they move): Go 1.26.5 · Node 24.18.0 · pnpm 11 (pinned via `packageManager`,
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
