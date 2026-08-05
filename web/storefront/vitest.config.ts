import { getViteConfig } from 'astro/config';

// Mirrors the backoffice config (TKT-174). Tests split by kind:
//  - pure-function tests (*.test.ts) are fetch-stubbed or pure → node env
//  - React component tests (*.test.tsx) need a DOM → jsdom, opted in per-file
//    via a `// @vitest-environment jsdom` docblock (vitest v4 removed
//    environmentMatchGlobs). node stays the default so the existing cache,
//    format and hold-countdown tests are unaffected.
//
// getViteConfig rather than plain defineConfig (TKT-208): the SSR call-budget
// test renders real `.astro` page modules, and only Astro's own vite plugin can
// transform them. Without it vitest cannot parse a page at all, and the budget
// would have to be asserted against something other than the page — which is
// the thing that test exists not to do.
export default getViteConfig({
  test: {
    environment: 'node',
  },
});
