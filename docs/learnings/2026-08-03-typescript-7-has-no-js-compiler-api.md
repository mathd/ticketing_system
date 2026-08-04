# TypeScript 7 blocks on tooling, not on our code

`typescript@7.0.2` (the Go port) is `latest` on npm as of 2026-08-03. All three web packages now
build against it. **Our source needed zero changes** — no syntax, no `lib`, no strictness fallout.
Everything that cost anything was tooling.

## What TS 7 does not ship

A JS compiler API. `require('typescript')` exports exactly two things:

```
> Object.keys(require('typescript'))
[ 'version', 'versionMajorMinor' ]
```

No `ts.sys`, no `ts.createProgram`, no `ts.createLanguageService`. The replacement lives behind
`typescript/unstable/{sync,async,fs,ast,...}` — a different shape, and (per the subpath name) not
frozen. Every tool that *drives* the compiler rather than shelling out to `tsc` therefore breaks on
import:

| Tool | Dies at |
| --- | --- |
| `astro check` → `@astrojs/language-server` → `@volar/kit` | `ts.sys.fileExists`, then `ts.useCaseSensitiveFileNames` — the whole checker, not one call |
| `openapi-typescript` | `dist/lib/ts.mjs` |

`@astrojs/check@0.9.10` (latest) still declares `peerDependencies.typescript: "^5.0.0 || ^6.0.0"`.
There is no TS7-capable Volar yet.

## What we did about each

**`openapi-typescript` moved to the workspace root**, which keeps its own `typescript@~6` — the
root package has no Astro and no TS 7. `make generate` calls `pnpm run generate:api` from the root
instead of `pnpm --filter <app> generate:api`; the emitted `api-types.gen.ts` is byte-identical.
(`pnpm dlx openapi-typescript@7` also works — isolated tree, own TS 5 — but it puts a network fetch
in the gate for no gain.)

**`astro check` was dropped**; the two Astro apps build with
`astro sync && tsc --noEmit && astro build`. `astro sync` must come first — it writes
`.astro/types.d.ts`, which the tsconfig `include`s, so on a clean clone `tsc` would otherwise run
against a missing file. `astro build` itself never needed the compiler API (Rust compiler + esbuild).

## The regression this bought, measured

`tsc --noEmit` does not read `.astro` files, so **frontmatter and template expressions are no
longer typechecked**. Probed in both directions on storefront:

- `const __probe: number = "not a number"` in `src/lib/api.ts` → `error TS2322`, exit non-zero. ✅
- the same line in the frontmatter of `src/components/PerformanceCard.astro` → **exit 0**. ❌

That is ~10 files across storefront and backoffice, and it is uncovered on exactly the SSR/form
paths `make check`'s smoke suite already cannot see (it renders back-office pages, never submits
them — see AGENTS.md). Until Volar ports, an `.astro` frontmatter type error reaches a browser.

## Two things that look like workarounds and aren't

- **pnpm `overrides` cannot pin a peer.** `'@astrojs/check>typescript': 6.0.2` reinstalls without
  error and changes nothing — the peer resolves from the *dependent's* own dependency set, and a
  `package.json` cannot list `typescript` twice. The failure afterwards is byte-identical.
- **`astro check --tsconfig` doesn't route around it.** Passing the path skips the
  `ts.findConfigFile` branch and the run gets exactly one frame further, into
  `@volar/kit/createChecker`. The dependency is on the API, not on one entry point.

## When to revisit

Watch `@astrojs/check` / `@volar/kit` peers for `^7`. When one lands, restore
`build: astro check && astro build` and delete this workaround — that closes the `.astro` gap
above. Moving `openapi-typescript` back into the apps needs the same thing from *its* maintainers;
until then the root pin is the cheap place for it.
