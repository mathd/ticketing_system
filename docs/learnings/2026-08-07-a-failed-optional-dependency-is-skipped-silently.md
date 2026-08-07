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
