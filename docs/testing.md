# Testing

The gate is `make check` = **lint → test → build → smoke**; CI runs exactly the same target
(`.github/workflows/check.yaml`) plus the gate self-test. Quality gates per story: PRD
§Quality gates (contract tests per touched boundary from US-002; journal invariants from US-004;
browser evidence on UI stories).

## Stages

| Stage | Go | TS |
|---|---|---|
| deps | — | `pnpm install --frozen-lockfile` |
| lint | golangci-lint (pinned) per module (`--build-tags smoke`) | `oxlint --deny-warnings` |
| test | `go test` per module | `vitest run` (jsdom + testing-library) |
| build | `go build` + `go vet` per module | `tsc -b && vite build` |
| smoke | `smoke/` suite via `scripts/smoke.sh` | — |

## Smoke build paths (TKT-42)

Per-PR/local smoke packages **host-built artifacts** into the images: static Go binaries
(`make build-gate-linux`, `CGO_ENABLED=0 GOOS=linux`) and the scanner `dist` (from
`build-ts`), selected via `compose.smoke.yaml` + the packaging-only Dockerfiles
(`build/go-bin.Dockerfile`, `web/scanner/Dockerfile.smoke`). This removes the in-Docker
compiles that dominated gate time (CI daemons are cold; `RUN` cache mounts don't persist).

The **hermetic** in-Docker build path (`build/go.Dockerfile`, `web/scanner/Dockerfile`) is
still what `make up` uses, and is exercised end-to-end by `make smoke-hermetic` in
`.github/workflows/hermetic.yaml` — weekly on main **and** on any PR touching the build
files (Dockerfiles, `compose*.yaml`, `.dockerignore`, `go.work*`), so hermetic regressions
cannot merge silently through the fast path.

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
- DB credential isolation — service A's role cannot connect to service B's database
- metrics ingestion — `http_server_*` series queryable in Prometheus after traffic
- US-004/006 checkout and gate scan — authoritative EUR price, capture+confirm,
  issuance/delivery, accepted scan, trace-derived duplicate rejection, and a concurrent
  real-PostgreSQL redemption race
- ADR-003 journal — `payments verify-journal` runs against the populated smoke database
  before Compose teardown and fails the gate on a gap, hash or signature mismatch

The reproducible browser check is `scripts/verify-checkout-browser.py` against a running seeded
stack. It verifies checkout success, guest-ticket QR retrieval, pasted credential acceptance,
duplicate rejection, and retriable decline; evidence lives in `docs/verification/checkout/`,
`docs/verification/ticket-delivery/`, and `docs/verification/gate-scan/`.

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
  failures (`err != nil`) are **client-side** and fail the run as *inconclusive* (generator
  health, rerun); unexpected statuses/5xx/malformed bodies are **server-side** and fail it
  as a correctness finding.
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
