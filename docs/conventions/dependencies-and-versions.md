# Dependencies and versions

## Latest stable by default (owner directive, 2026-07-12)

This is a greenfield platform: every language, runtime and dependency targets its **latest stable
release**, looked up at implementation time — never assumed from memory. Applies to Go, Node,
TypeScript, Vite, PostgreSQL, Redis, NATS, GitHub Actions, linters, everything. Pinning behind
latest requires a stated compatibility reason, recorded next to the pin.

Current anchors (update as they move): Go 1.27.3 · Node 26.13.0 · pnpm 12 (pinned via `packageManager`,
which also enforces pnpm's supply-chain `minimumReleaseAge` policy) · PostgreSQL 18.6 · Vite 8 ·
React 19 · TypeScript `~7.0.2`. Read the manifests and lockfile for exact resolutions.

- **Hold: the TypeScript 6 API for `openapi-typescript`.** Every compiler in the repository is
  TypeScript 7. TypeScript 7 has no JavaScript compiler API, and `openapi-typescript` (latest
  7.13.0, peer `typescript: ^5.x`) calls that API. The root `typescript` dependency is therefore
  the alias `npm:@typescript/typescript6@~6.0.2`, Microsoft's side-by-side package for the TS 6
  API. It has no `tsc` binary. pnpm resolves a peer from the dependent's parent, so
  `packageExtensions` and `overrides` cannot point the peer elsewhere. `@hey-api/openapi-ts`
  0.99.0 also calls the TS JS API, so a generator change does not remove the hold. Remove the
  alias when an `openapi-typescript` release accepts `typescript@7`.
- **Local Node selection.** Run `nvm use` from the repo to select the Node 26.13.0 release
  pinned in `.nvmrc`.
- **Node 26 in CI and images.** CI and the web images use Node 26.13.0. Node 25+ has no Corepack.
  The web images install pnpm with npm. CI runs `corepack enable` before `setup-node`, with the
  runner's preinstalled Node; the pnpm shim then runs under Node 26.

### Go directive and current holds (TKT-523)

- **The `go` directive in `go.work` and in every `go.mod` names the patch release, 1.27.3.** With
  `go 1.27.0`, the local `GOTOOLCHAIN=auto` selection can run 1.27.0, which has the standard-library
  advisories, while CI runs 1.27.3. The directive is a minimum, not a lock: a newer local Go can
  still run. `go.work` must be at least every module's directive.
- **A Go patch release can break the linter.** golangci-lint v2.13.2 cannot read the go1.27.2
  export data. `check-go-toolchain` compares minor versions only, so it stays green for that pair.
  After a Go bump, run `make lint-go`.
- **Hold: `github.com/dprotaso/go-yit` in `services/catalog/go.mod`.** It stays at the last commit
  on `gopkg.in/yaml.v3` (`v0.0.0-20250513224043-18a80f8f6df4`). Later commits use another yaml
  module, and `vmware-labs/yaml-jsonpath` v0.3.2 (its latest release) then stops compiling inside
  `go tool oapi-codegen`. The reason is also above the pin. `go get -u tool` removes the hold, so
  give the pinned version again in that command. Remove the hold when yaml-jsonpath uses the same
  yaml module as go-yit.
- **Speakeasy dependencies share the `go-yit` hold.** `github.com/speakeasy-api/jsonpath`
  stays at v0.6.3 and `github.com/speakeasy-api/openapi` at v1.25.6. Their v0.7.0 and
  v1.26.3 releases require `go-yit@v0.0.0-20250804204205-7afe4252a1dd`, which conflicts
  with the held version. Recheck these upgrades when the generator hold is removed.

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
