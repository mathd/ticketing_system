import { defineConfig } from 'vitest/config';

// The backoffice is an Astro app; its unit tests split by kind:
//  - lib/api client tests (*.test.ts) are fetch-stubbed pure functions → node env
//  - React component tests (*.test.tsx) need a DOM → jsdom, opted in per-file via
//    a `// @vitest-environment jsdom` docblock (vitest v4 removed
//    environmentMatchGlobs). node stays the default so the client tests are
//    unaffected (Fable plan risk note).
export default defineConfig({
  test: {
    environment: 'node',
  },
});
