# A failed optional dependency is skipped silently, and fails later as someone else's bug

TKT-227. Filed because `make up` died on the scanner image:

```
> [scanner build 6/6] RUN pnpm install --frozen-lockfile && pnpm --filter scanner build
  at getExePath (…/typescript@7.0.2/node_modules/typescript/lib/getExePath.js:53:19)
[ERR_PNPM_RECURSIVE_RUN_FIRST_FAIL] scanner@0.0.0 build: `tsc -b && vite build`
```

Four tickets (TKT-220…223) worked around it by verifying against the smoke-shaped stack
(`compose.smoke.yaml`, host-built artifacts) instead — which is exactly the path that does *not*
exercise the in-Docker build.

## It does not reproduce

- `docker build -f web/scanner/Dockerfile .` from the repo root: **green**, on a cold
  `pnpm install` layer (`downloaded 161, reused 0` — no cached store to hide behind).
  `tsc -b && vite build` both ran, on `node:26.5.0-alpine3.23`, linux/arm64, musl.
- A full `make up` stack has been Up and healthy for 37 hours on the same host that filed
  this, `ticketing-scanner-1` included.

## What it was not

`getExePath.js:53` is one throw: *"Unable to resolve @typescript/typescript-linux-arm64.
Either your platform is unsupported, or you are missing the package on disk."* TypeScript 7 is a
native Go compiler shipped as one **optionalDependency per platform**, and the JS wrapper resolves
the matching package at run time. Three plausible causes, all checked and all false:

- **Not musl.** `@typescript/typescript-linux-arm64@7.0.2` declares `os: [linux]`, `cpu: [arm64]`
  and **no `libc`** — nothing excludes Alpine.
- **Not a missing lockfile entry.** It has been in `pnpm-lock.yaml` since the TypeScript 7
  migration (#156), the same commit that introduced `typescript@7.0.2`.
- **Not the workspace shape.** The scanner image copies only `web/scanner/`, but the build
  succeeds with the other two workspace packages absent.

## The lesson

**pnpm does not fail an install when an optionalDependency fails to fetch — it skips it.** That is
correct for a genuinely optional package and wrong for this one: `tsc` is not optional to
`tsc -b`, it is merely *distributed* as an optional dep so that twenty platform binaries can share
one manifest. So a transient registry hiccup during a cold in-container install produces a green
`pnpm install`, and the failure surfaces minutes later inside an unrelated tool with a message
about the *platform* — which is what sent this ticket after musl and the lockfile.

When a build tool resolves its own binary at run time, "install succeeded" says nothing about
whether the binary is there. Read the throw before believing the diagnosis it suggests.

## Disposition

Landed as a diagnosis, not a repair (TKT-227 permits this explicitly). Nothing to fix while it
does not reproduce, and a retry wrapper around a one-off network failure is machinery earning its
keep zero times a year. If it returns, the first question is whether
`node_modules/.pnpm/@typescript+typescript-linux-arm64@7.0.2` exists in the failed layer — not
whether Alpine is supported.

Note that `scripts/browser.sh` (TKT-228) uses the smoke fast path **on purpose** and says so; it
is not another instance of this workaround.

Re-confirmed at closeout on linux/**amd64** and emulated linux/**arm64**, `--no-cache` both times,
and again at `a9b81de5` — the exact commit that was HEAD when the ticket was filed, ruling out a
later accidental fix. `@typescript/typescript-linux-x64@7.0.2` installs and `tsc --version` reports
7.0.2 inside the image.

## The second defect, which is what actually broke `make up`

The ticket stayed open, and by the time it was worked `make up` was broken again — **for an
unrelated, fully reproducible reason that had nothing to do with TypeScript.** It never reached an
image build:

```
error while interpolating services.backoffice.environment.INVENTORY_STAFF_WRITE_TOKEN:
required variable INVENTORY_STAFF_WRITE_TOKEN is missing a value:
no default is shipped - run 'make up' once to generate a local credential
```

**TKT-244** made that credential mandatory in `compose.yaml` (`${VAR:?}`, at two services) and did
not extend `scripts/env-bootstrap.sh`, so the error told the developer to run the command that had
just failed. The dates rule out any connection to the TypeScript report:

| date | event |
|---|---|
| 2026-08-06 | TKT-227 filed — scanner build fails on arm64 macOS |
| 2026-08-07 | this document: does not reproduce; landed as a diagnosis |
| 2026-08-10 | `docs/demo-readiness.md`: `make up` **worked**, scanner image built cleanly |
| 2026-08-15 | TKT-244 (`720bfc58`) adds the `:?` requirement, not the generation |
| 2026-08-17 | `make up` fails on interpolation; TKT-227 closed on *this* |

### Why the gate never saw it

`make check` was green throughout — verified, not assumed: it was run to completion on the unfixed
tree and exited 0. Its smoke stage takes its environment from `scripts/stack-env.sh`, which **does**
generate the token (and documents it as a deliberate fifth independent draw). So the two
environment paths — `env-bootstrap.sh` for `make up`, `stack-env.sh` for smoke — drifted apart, and
only the one nothing in the gate exercises was wrong.

**The general shape: a gate that supplies its own version of a shared input cannot notice that the
real one is missing.** The smoke path was not wrong to build its own environment; it was wrong that
nothing compared the two. `scripts/check-required-env.sh` now does, deriving the expected set from
the requirement (`compose.yaml`'s `:?` markers) rather than from what the bootstrap emits — an
expectation read off the behaviour would have called the defect correct.

Two smaller traps met while building that guard, both instances of rules already in `AGENTS.md`:

- The obvious sandbox is `git worktree add HEAD`, and it is wrong twice: it reports on committed
  state while an uncommitted repair sits in the tree, and `gate-selftest.sh` seeds its mutations
  *inside its own worktree*, so re-checking-out HEAD would quietly undo the seed and the
  `expect_fail` case could never fail.
- A checker that parses a file for a marker must **refuse when it finds implausibly few**, or a
  broken parse reports "nothing missing" and passes. The floor is asserted, and tested by removing
  every `:?` from a throwaway copy and confirming the guard refuses rather than going green.
