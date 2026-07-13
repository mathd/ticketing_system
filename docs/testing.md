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
- US-004 checkout — authoritative EUR price, capture+confirm, decline+release/reacquire
- ADR-003 journal — `payments verify-journal` runs against the populated smoke database
  before Compose teardown and fails the gate on a gap, hash or signature mismatch

The reproducible browser check is `scripts/verify-checkout-browser.py` against a running seeded
stack. It verifies checkout success, guest-ticket QR retrieval, and retriable decline; evidence
lives in `docs/verification/checkout/` and `docs/verification/ticket-delivery/`.

## The gate self-test

`scripts/gate-selftest.sh` proves the gate actually fails: it seeds, one at a time in a
disposable git worktree (never your tree, trap-cleaned), a Go lint violation, a failing Go
test, a Go compile error, a TS lint violation, a failing vitest test and a TS type error —
and requires the corresponding `make` stage to exit non-zero.

## Concurrency proofs (coming)

From US-003 the inventory no-oversell property gets an automated contention test inside
`make check`; load tests at the festival-scale NFR live in TKT-4/TKT-20/TKT-31/TKT-37.
