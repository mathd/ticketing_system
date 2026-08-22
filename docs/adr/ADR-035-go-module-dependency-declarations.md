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

### Amendment (TKT-265, 2026-08-21): the vertical direction is now gated too

The decision above closes **horizontal** drift — two modules declaring different versions of the
same dependency. It is silent on the **vertical** direction, and that silence was a real gap:
MVS can raise a *selected* version through a transitive requirement without any `go.mod` line
changing, leaving every manifest internally consistent while describing a build that is not what
happens. `go mod tidy` does not correct it, and the horizontal checker cannot see it — the
versions agree with each other, they just all agree on the wrong number.

Measured at `afa333f4`, on a tree where `make check` was green: **14 lagging declarations across
seven modules** — `x/crypto` v0.54.0 (selected v0.55.0) in six, `x/text` v0.40.0 (selected v0.41.0)
in seven, `x/net` v0.56.0 (selected v0.58.0) in `shared/go`.

`scripts/check-go-build-list-lag.sh` now runs as a second `make check` prerequisite and fails when
a module declares a version **below** the one the workspace selects. Absent is not lagging: a
module that does not declare a dependency at all remains correct, for the same reason the
horizontal check ignores single-module declarations.

**It is a separate script, and it is NOT offline.** The horizontal checker's contract is
manifest-only and offline (`go mod edit -json` never resolves the workspace); this one must
resolve the build list, so it uses `go list -m`, which walks the module graph and **cannot run
under `GOPROXY=off` on a cold cache** (measured: 0.18s warm; 22s and ~315MB on a cold cache with
network; fails outright offline). Folding the two together would falsify the horizontal checker's
stated contract, so they stay separate.

That cost is smaller than it first appears, which is why the trade was taken: **`make check`
already requires the network on a cold machine** — its first prerequisite is
`deps: pnpm install --frozen-lockfile`, whose own comment reads *"clean clone needs nothing
pre-installed"*, and `check-generate` then runs `go tool oapi-codegen`. What is spent here is one
script's offline capability, not the gate's. No CI job, Make target or developer document in this
repo sets `GOPROXY=off` or depends on an air-gapped run.

Two of the three grounds on which `go work sync && git diff --exit-code` was rejected above do
**not** apply to this check: it never mutates the working tree, and it never consults `git`, so
untracked sum files cannot be invisible to it. The third ground — the untracked `gateway/go.sum` —
is now **historical**: that file was committed by this ADR's original change and is tracked today.

`go work sync` remains the **repair** operation, not the enforcement one. The 14 lagging
declarations were realigned with it before the check was added; that realignment raised every
affected version to what the workspace already selected, lowered nothing, moved no major version,
and left the selected build list **byte-identical** (verified by diffing `go list -m all` across
the change). It has zero runtime effect — it makes the manifests describe a build that was already
happening. The indirect-requirement churn it also produced (`services/access` gaining seven
requirements, `services/commerce` gaining `gorilla/mux` and dropping `kr/pretty`) was committed
rather than left, so that a future `go work sync` does not regenerate an unexplained diff.

### What this guarantees — and against whom

Stated in ADR-021's register, because "the gate enforces one version" is exactly the kind of claim
that overreaches:

- **Closed:** an *honest* maintainer — or Dependabot — can no longer leave two modules declaring
  different versions of a shared dependency without the gate failing, **nor leave a module
  declaring a version below the one the workspace selects**. Both directions of accidental
  declaration drift are now loud at the commit that introduces them. The vertical half is closed
  only when the module graph can actually be resolved: an unresolvable graph, an unreadable
  manifest or a truncated parse **refuses to report a verdict** (exit 2) rather than reporting no
  lag.
- **Not closed, and not closeable here:** anyone editing a `go.mod` and the checker in the same
  commit. The check lives in the repo it checks; it constrains mistakes, not intent. The same
  applies to the workspace inputs — someone who edits `go.work` and the checker together can make
  the repository accept its own chosen answer.
- **Not claimed:** that either check fixed a runtime version split. There has never been one.
  Both the original realignment and TKT-265's left the workspace build list **byte-identical** —
  verified by comparing `go list -m` before and after. Nor is it claimed that every selected
  dependency appears in every manifest: only a *declared* version below the selected one is
  rejected, and an absent declaration stays valid. Nor that the gate is offline — the vertical
  check needs the module cache or the network.

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
