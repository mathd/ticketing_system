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

## The smoke seam

`smoke/smoke_test.go` (build tag `smoke`) is black-box against the composed stack through
the gateway, plus named infra assertions:

- `/healthz/all` — all five services up
- storefront and scanner served through the gateway
- trace propagation — a caller-chosen trace id appears in gateway **and** service JSON logs
- JetStream — the `PLATFORM` stream exists from stack init (nats-init) + publish/durable consume
- DB credential isolation — service A's role cannot connect to service B's database
- metrics ingestion — `http_server_*` series queryable in Prometheus after traffic

## The gate self-test

`scripts/gate-selftest.sh` proves the gate actually fails: it seeds, one at a time in a
disposable git worktree (never your tree, trap-cleaned), a Go lint violation, a failing Go
test, a TS lint violation, a failing vitest test and a TS type error — and requires the
corresponding `make` stage to exit non-zero.

## Concurrency proofs (coming)

From US-003 the inventory no-oversell property gets an automated contention test inside
`make check`; load tests at the festival-scale NFR live in TKT-4/TKT-20/TKT-31/TKT-37.
