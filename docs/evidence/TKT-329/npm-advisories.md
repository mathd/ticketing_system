# TKT-329 npm advisory evidence

## Status

Dependency resolution and candidate verification are pending. The two Astro declarations and
the `js-yaml` override now target patched ranges, but `pnpm-lock.yaml` is unchanged. The targeted
update stopped before resolution because pnpm could not open its SQLite store under
`/home/mathieu/.local/share/pnpm`. This path is outside the writable workspace. No alternate
store or network route was tried.

The next required command, in an environment where pnpm can write its configured store, is:

```sh
pnpm --filter storefront --filter backoffice update astro
```

Then inspect every copy with `pnpm why` and update the lockfile only for those app importers and
remaining vulnerable paths. Candidate audit, Trivy scan, app builds, typechecks, frozen install,
browser probes, and full gates are pending. No candidate result is claimed here.

## Advisory baseline

The supplied baseline captures report five advisories. Versions below are lockfile resolutions
from before this change. Patched versions are the fixes reported by those captures or the Astro
7.3.5 registry metadata at `/tmp/tkt329-4n2mhh_t/astro-target.json`.

| Advisory | Package | Baseline | Patched version |
| --- | --- | ---: | ---: |
| [GHSA-26w7-cxv4-gfx2](https://github.com/advisories/GHSA-26w7-cxv4-gfx2) | `astro` | 7.2.0 | 7.2.8 or later; target 7.3.5 |
| [GHSA-rgj7-g3m4-5g8c](https://github.com/advisories/GHSA-rgj7-g3m4-5g8c) | `sharp` | 0.35.3 | 0.35.4 or later |
| [GHSA-2883-xcg3-v3hh](https://github.com/advisories/GHSA-2883-xcg3-v3hh) | `js-yaml` | 4.3.1 | 4.3.2 or later |
| [GHSA-w27v-7q3p-w38r](https://github.com/advisories/GHSA-w27v-7q3p-w38r) | `svgo` | 4.0.2 | 4.1.0 or later |
| [GHSA-7w5x-hrqm-74c2](https://github.com/advisories/GHSA-7w5x-hrqm-74c2) | `smol-toml` | 1.7.0 | 1.7.1 or later |

Astro 7.3.5 declares `sharp` at `^0.35.4`, `svgo` at `^4.1.0`, `js-yaml` at `^4.3.2`, and
`smol-toml` at `^1.8.0`. The pre-change `pnpm why` output also shows `smol-toml@1.7.0` under
`@astrojs/internal-helpers@0.10.1`, used by the unchanged `@astrojs/node` and `@astrojs/react`
versions. Check whether targeted resolution lifts that copy before adding a vulnerable-range
override. It also shows `js-yaml` under root `openapi-typescript`; keep that parent on major 4.

## Commands and results

- `pnpm --version` returned `11.12.0`.
- `pnpm --filter storefront --filter backoffice update astro` exited 1 with
  `[ERR_SQLITE_ERROR] unable to open database file` while opening pnpm's configured store. It did
  not update the lockfile.
- `git diff --check` passed for the scoped changes.
- Baseline `pnpm audit` capture: `/tmp/tkt329-4n2mhh_t/audit-before.json`, exit 1, five
  advisories.
- Baseline Trivy lockfile capture: `/tmp/tkt329-4n2mhh_t/trivy-lockfile-before.json`, exit 1,
  five findings. The log is `/tmp/tkt329-4n2mhh_t/trivy-lockfile-before.log`.
- The baseline reports do not establish candidate status. The orchestrator is expected to run
  candidate scanners and gates.

The root TypeScript resolution remains 6.0.3 and both web TypeScript resolutions remain 7.0.2 in
the unchanged lockfile. The only direct dependency declarations changed are the two Astro ranges.

## Image-path review of the baseline deployment

This is a limited hygiene assessment for the current deployment and an ordinary unauthenticated
caller. It does not establish safety against a compromised container, an administrator who can
change files, or future configuration changes.

- The baseline Astro 7.2.0 storefront image was built and probed. Its client directory contained
  three JavaScript files and no image asset. The runtime process used uid 1000 and could not write
  to `dist/client`. The Dockerfile copies only `dist` into the runtime image, runs as `node`, and
  defines no app volume in `compose.yaml`.
- The built Astro Node image endpoint loads local files from `dist/client` with a parent-bound
  path. Both Astro configs omit image allowlists, so the built default has empty `domains` and
  `remotePatterns`. The app source contains no filesystem-write call, upload route, or AVIF asset.
- The baseline storefront probe reached `GET /_image`. An empty request returned 400, a remote
  AVIF URL returned 403 before fetch or transform, and a missing local AVIF returned 500 at the
  route's local-file checks. These responses do not prove that `sharp` is unavailable or never
  runs for valid input.
- The back-office probe without a session redirected `/admin/_image` to `/admin/login`. A
  separate isolated seeded-session probe returned 403 for that path. It did not exercise the
  actual catalog login flow.

No test asset was added and no image configuration was weakened. The reviewed paths provide no
identified way for an ordinary unauthenticated caller to supply AVIF bytes to the current
deployment. This is a scoped finding, not a claim that the critical advisory is unreachable.
