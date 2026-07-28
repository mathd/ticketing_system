# ADR-035: Eight Go modules stay; a gate enforces one version per shared dependency

Date: 2026-07-28

## Status

Accepted

## Context

The repository has **eight** Go modules — five services, the gateway, `shared/go` and `smoke` —
under one `go.work`, in one repo, shipping as **one deploy unit** (ADR-001, ADR-002). Nothing here
is independently versioned or independently consumed by anyone. The 2026-07-25 architecture review
(recommendation R9) flagged the layout as cost without benefit, and pointed at visible dependency
drift as the evidence.

Measured at `d28caf1`, four dependencies were declared at two different versions, `smoke` being the
sole outlier in every case:

| Dependency | `smoke` | every other module declaring it |
|---|---|---|
| `github.com/gorilla/mux` | `v1.8.0` | `v1.8.1` |
| `golang.org/x/crypto` | `v0.54.0` | `v0.52.0` |
| `golang.org/x/sys` | `v0.47.0` | `v0.45.0` |
| `golang.org/x/text` | `v0.40.0` | `v0.37.0` |

The review also named `otel` and `kin-openapi` as drifted. They are not: they are declared by only
some modules (otel by 2 of 8, kin-openapi by 3 of 8), at **one** version each. A module that does
not import a package has no business pinning it — that is correct, not drift.

### What is actually broken — say it precisely

**This is declaration drift, not runtime drift.** Every supported build already resolves a single
MVS list, because every Go command in this repo discovers the root `go.work`:

- From `services/inventory`, `go list -m golang.org/x/text` reports **`v0.40.0`** — `smoke`'s
  version — while that module's own `go.mod` declares `v0.37.0`.
- The `Makefile`'s per-module `cd $$m && go test ./...` loops are **not** isolated builds; `cd` does
  not leave the workspace.
- `build/go.Dockerfile` copies `go.work` before building, so container builds resolve the same way.
- With `GOWORK=off`, the five services do not build at all: they never `require ticketing/shared`.

So no two deployed binaries are running different versions of anything today. What is broken is
that **the manifests no longer describe what is built**: a reviewer reading `services/access/go.mod`
sees `x/text v0.37.0` and cannot see that a bump in `smoke` moved access's actual build to `v0.40.0`.
The blast radius of a dependency change is invisible at the place a reviewer looks for it, and the
divergence becomes real the moment anything is built outside the workspace.

**And it does not self-heal.** `go mod tidy` run across all eight modules on the drifted tree was a
complete no-op: zero diff, all four drift groups intact. The repository was simultaneously *fully
tidy* and *fully drifted*. Normal maintenance would never have corrected it, and nothing in the gate
noticed. `go work sync` is the only Go-provided operation that aligns manifests to the workspace
build list.

## Possible Solutions

- **Option 1: Collapse to one `go.mod` at the repo root.**
    - Pros:
        - Drift becomes impossible **by construction** — one manifest, one version of everything.
          No check to write, no check to maintain, no way to regress.
        - Honest about the topology: one repo, one deploy unit, one dependency set.
        - Removes `go.work`, `go.work.sum` and eight `go.mod`/`go.sum` pairs.
    - Cons:
        - **61 changed paths.** A root module named `ticketing` maps `shared/go/domainevent` to
          `ticketing/shared/go/domainevent`, so all **28** files importing `ticketing/shared/...`
          need rewriting. Physically moving `shared/go` to `shared/` would preserve those imports,
          but 13 non-Go files reference the `shared/go` path — including `.github/dependabot.yml`,
          `.github/workflows/security.yaml`, the `Makefile`, `scripts/gate-selftest.sh`,
          `docs/architecture.md` and ADR-028/033/034 — so it trades 28 import edits for a larger,
          messier diff that also relocates a kernel two ADRs just placed.
        - Touches `.github/dependabot.yml` and all three workflows.
        - **Buys no runtime correctness today**, because the workspace already selects one graph.
        - A half-collapsed build is worse than either end state, so it cannot be staged.

- **Option 2: Keep the eight modules; fail the gate when two declare different versions.**
    - Pros:
        - **21 changed paths**, 14 of them mechanical `go.mod`/`go.sum` alignment.
        - Preserves ADR-002's five binaries, ADR-022's per-binary `migrate` entrypoints and Compose
          jobs, ADR-009's catalog-owned `tool` directive, and the `shared/go` kernel location that
          ADR-033/034 established.
        - No Dockerfile, Compose, workflow, `go.work` or import-path change at all. All twelve
          Compose `PKG` build args stay valid.
        - Reversible: deleting one script, one Make prerequisite and four documentation paragraphs
          undoes it.
    - Cons:
        - A check to maintain, and a failure mode (drift) that remains *possible* rather than
          *unrepresentable*.
        - Dependabot will now open PRs that fail the gate — see Consequences.

- **Option 3: Do nothing.**
    - Pros: zero diff.
    - Cons: the measurement above shows the drift is invisible to every existing tool, is preserved
      by `go mod tidy`, and grew unnoticed. "Nothing notices" is the defect.

## Decision

**Option 2.** The eight modules stay. `scripts/check-go-dependency-drift.sh` runs as a `make check`
prerequisite and fails when any dependency declared by two or more modules carries more than one
version. The four drifted declarations were realigned with `go work sync`.

The check reads manifests only, through `go mod edit -json` — no package loading, no downloads, no
workspace resolution. Eight modules parse in ~0.2s, so the ~2min gate TKT-42 bought is not spent
here. A dependency declared by exactly one module is ignored: forcing every module to pin what it
does not import would fight `go mod tidy` and misdescribe package ownership.

Rejected as the implementation: **`go work sync && git diff --exit-code`**, which mirrors the
existing `check-generate` idiom in two lines and runs in 0.19s warm. It loses on three measured
counts — it took ~5 minutes on a cold module cache and can require the network; it mutates the
working tree during the gate; and `git diff` cannot see the untracked `gateway/go.sum` that
`go work sync` creates.

Collapse is not rejected forever. Reconsider it if a module ever needs to be independently
consumed or versioned, or if the check's maintenance cost exceeds the 61-path move.

### What this guarantees — and against whom

Stated in ADR-021's register, because "the gate enforces one version" is exactly the kind of claim
that overreaches:

- **Closed:** an *honest* maintainer — or Dependabot — can no longer leave two modules declaring
  different versions of a shared dependency without the gate failing. Accidental drift is now
  loud at the commit that introduces it.
- **Not closed, and not closeable here:** anyone editing a `go.mod` and the checker in the same
  commit. The check lives in the repo it checks; it constrains mistakes, not intent.
- **Not claimed:** that this fixed a runtime version split. There was none. Realigning the
  manifests left the workspace build list **byte-identical** — verified by comparing `go list -m`
  before and after.

## Consequences

- `make check` gains a `check-dep-drift` stage before `lint`. CI is unchanged: it runs `make check`
  verbatim, so the gate and CI stay mirrored by construction (COS 3).
- `scripts/gate-selftest.sh` gains a seeded case — drop access's `otel` requirement to `v1.43.0`,
  assert the target fails — preceded by a **positive control** asserting the clean baseline passes.
  Without that control an always-failing checker would satisfy its own negative test. This is the
  first seed that mutates an existing file rather than adding one, which is why the control is new.
- **Dependabot now generates gate failures, by design.** `.github/dependabot.yml` runs `gomod`
  weekly over all eight module directories with no `groups:` key, so it opens one PR per module. A
  bump to a dependency a sibling also declares will fail `check-dep-drift` until the sibling is
  raised too. The fix on such a PR is a follow-up `go work sync` commit, and the checker's failure
  message says so. Dependabot grouping was deliberately **not** added in the same change: it is a
  separate judgement about update batching, and it should be made against real friction rather than
  a prediction of it.
- Realignment raised eight declarations (`gorilla/mux`, `x/crypto`, `x/mod`, `x/net`, `x/sync`,
  `x/sys`, `x/text`, `x/tools`) and added five test-only indirect requirements that `go work sync`
  pulls from the workspace build list (`go-spew`, `go-difflib`, `go-cmp`, `kr/pretty`,
  `rogpeppe/go-internal`). **No dependency changed major version** — no module path's `vN` suffix
  moved. All eight raises were to versions the workspace was *already* selecting.
- `gateway/go.sum` is created (empty — the gateway has no dependencies) and committed, so that
  `go work sync` is idempotent against a clean tree.
