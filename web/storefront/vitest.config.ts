import { defineConfig } from 'vitest/config';

// Mirrors the backoffice config (TKT-174). Tests split by kind:
//  - pure-function tests (*.test.ts) are fetch-stubbed or pure → node env
//  - React component tests (*.test.tsx) need a DOM → jsdom, opted in per-file
//    via a `// @vitest-environment jsdom` docblock (vitest v4 removed
//    environmentMatchGlobs). node stays the default so the existing cache,
//    format and hold-countdown tests are unaffected.
export default defineConfig({
  test: {
    environment: 'node',
  },
});
