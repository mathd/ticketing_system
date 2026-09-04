# Testing

The gate is `make check` = **deps → generated API drift → dependency drift → build-list lag →
workflow checks (security, hermetic) → ADR numbering → markdown links → hook self-test → Go
toolchain check → lint → standalone Go modules → test → build → smoke**. CI runs exactly the
same target (`.github/workflows/check.yaml`) plus the gate self-test. Quality gates per story: PRD
§Quality gates (contract tests per touched boundary from US-002; journal invariants from US-004;
browser evidence on UI stories).

## Stages

| Stage | Go | TS |
|---|---|---|
| deps | — | `pnpm install --frozen-lockfile` |
| dep-drift | one version per dependency across the eight modules — manifest-only and offline ([ADR-035](adr/ADR-035-go-module-dependency-declarations.md)) | — |
| build-list-lag | no module declares **below** the version the workspace selects ([ADR-035](adr/ADR-035-go-module-dependency-declarations.md) §Amendment). Resolves the module graph via `go list -m`, so unlike dep-drift it **needs the module cache or network**; fails closed (exit 2) when the graph cannot be resolved | — |
| lint | golangci-lint (pinned) per module (`--build-tags smoke`) | `oxlint --deny-warnings` |
| standalone | workspace-disabled, network-disabled readonly package loading for every Go module | — |
| test | `go test` per module | `vitest run` (jsdom + testing-library) |
| build | `go build` + `go vet` per module | Scanner: `tsc -b && vite build`; Storefront and Back Office: `astro sync && tsc --noEmit && astro build` |
| smoke | `smoke/` suite via `scripts/smoke.sh` | — |

## Smoke build paths (TKT-42)

Per-PR/local smoke packages **host-built artifacts** into the images: static Go binaries
(`make build-gate-linux`, `CGO_ENABLED=0 GOOS=linux`) and the Scanner, Storefront, and Back Office
`dist` directories (from `build-ts`). `compose.smoke.yaml` selects the packaging-only
`build/go-bin.Dockerfile` and each web application's `Dockerfile.smoke`. This removes the
in-Docker compiles that dominated gate time (CI daemons are cold; `RUN` cache mounts don't
persist).

The **hermetic** in-Docker build path (`build/go.Dockerfile` and each web application's
`Dockerfile`) is still what `make up` uses. `.github/workflows/hermetic.yaml` exercises it
end-to-end with `make smoke-hermetic`, weekly on main and on any PR touching the build files
(Dockerfiles, Compose files, `.dockerignore`, or `go.work*`).

Gate timings measured on TKT-42 (before → after):

| run | before | after |
|---|---|---|
| CI `make check` | 10m32 | 5m14 (cold caches; warm is lower) |
| CI `gate-selftest` | 2m32 | 45s |
| local `make check` (warm) | ~10m | 1m29 |
| hermetic smoke | in every PR run | 6m20, weekly + build-file PRs only |

## The smoke seam

`smoke/smoke_test.go` (build tag `smoke`) is black-box against the composed stack through
the gateway, plus named infra assertions:

- `/healthz/all` — all five services up
- storefront and scanner served through the gateway
- trace propagation — a caller-chosen trace id appears in gateway **and** service JSON logs
- JetStream — the `PLATFORM` stream exists from stack init (nats-init) + publish/durable consume
- NATS publisher ACLs — real-broker enforcement of unauthenticated publish refusal, cross-prefix
  publication refusal, payments zero-rights connection, and residual credentialed forgery pinning
  (ADR-072, `smoke/nats_acl_test.go`). Operator-identity isolation is asserted on the CONTAINER
  ENVIRONMENT, not on a broker refusal: the property is "no other component holds this password",
  and a broker cannot report who else was given one
- DB credential isolation — service A's role cannot connect to service B's database
- metrics ingestion — `http_server_*` series queryable in Prometheus after traffic
- US-004/006 checkout and gate scan — authoritative EUR price, capture+confirm,
  issuance/delivery, accepted scan, trace-derived duplicate rejection, and a concurrent
  real-PostgreSQL redemption race
- ADR-003 journal — `payments verify-journal` runs against the populated smoke database
  before Compose teardown and fails the gate on a gap, hash or signature mismatch

Run `make browser` for the reproducible browser check. The checkout spec creates a published offer,
submits successful and declined payments, opens the guest ticket page, pairs a scanner, and checks
accepted and duplicate scans. It requires real Google Chrome and `zbarimg` from the host's ZBar
command-line tools. The spec decodes the PNG the ticket page loaded and submits that decoded value,
so a broken image route cannot pass through a direct ticket-bundle fallback. The screenshots in `docs/verification/checkout/` and
`docs/verification/ticket-delivery/` record earlier manual proof runs; the automated check does not
rewrite tracked evidence.

## The gate self-test

`scripts/gate-selftest.sh` proves the gate actually fails: it seeds, one at a time in a
disposable git worktree (never your tree, trap-cleaned), a Go lint violation, a failing Go
test, a Go compile error, a TS lint violation, a failing vitest test and a TS type error —
and requires the corresponding `make` stage to exit non-zero.

## Concurrency proofs

From US-003 the inventory no-oversell property has automated contention tests inside
`make check` (`smoke/inventory_contention_test.go`). From TKT-82 the **on-sale load proof**
(`smoke/onsale_load_test.go` + `smoke/internal/loadtest`) adds a sustained open-loop profile
against the real gateway→inventory hold/finalize/confirm path; read-path and observability
load profiles remain TKT-31/TKT-37.

**Two profiles, one harness** (parameters only, `ONSALE_PROFILE`):

- **Gate** (in `make check`): one capacity-500 pool; 5/s warm-up, a 25 attempts/s × 10s
  sustained window (1,500 attempts/min), an exact fill to capacity, a 25-attempt rejection
  tail. Budget: hard 30s deadline on the load portion (adds ~15–30s to smoke).
  **Correctness-fatal, throughput-advisory**: it fails on oversell, DB accounting mismatch,
  missing `claim_history` rows, and errors of either class — but records (never asserts)
  latency percentiles and dropped arrivals, so a slow runner can't turn the pool's
  deliberate ADR-010 serialization into a flake. Errors are split (TKT-92): transport-level
  failures with no delivered status (or a truncated success body) are **client-side** and
  fail the run as *inconclusive* (generator health, rerun); delivered unexpected
  statuses/5xx/malformed bodies are **server-side** and fail it as a correctness finding —
  a forbidden status decides on its own, truncated body or not.
- **Full NFR** (on-demand): `make onsale-load-full`. 3,000 attempts/min sustained for 3
  minutes against a 100k pool (SLO: per-mutation p99 ≤ 1s, lifecycle p99 ≤ 3s), a per-pool
  ceiling sweep (75→3,000 attempts/s, fresh pool per rate, stop at first unstable — unstable
  = drops, server errors, rejections, <99% delivery **or** lifecycle p99 over the 3s SLO;
  client-side errors instead abort the stage as inconclusive; published
  as a highest-stable/first-unstable bracket, or a lower bound if no knee is observed), and a
  quantity-50 oversell tail on a 50k pool. Evidence is written to
  `docs/evidence/TKT-82/full-profile.json`; the published per-pool ceiling (the number
  TKT-20's waiting room must respect) lives in `docs/verification/on-sale-load/README.md`.

The zero-oversell verdict is **database-side**: post-drain accounting over
`inventory_pools`/`claims`/`claim_history` with ADR-010's live-claims predicate — the client
only offers load. The `claim_history` INSERT overhead inside the pool-locked transaction
(ADR-023 amendment) is reported per-mutation from `pg_stat_statements`, preloaded in smoke
stacks via `compose.onsale-load.yaml` (aggregate DB execution time, not a causal
with/without delta). Latency samples cover mutations only — availability is a cached read
(ADR-004) and never appears in them.
