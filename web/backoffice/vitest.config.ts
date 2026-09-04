import { getViteConfig } from 'astro/config';

// The back office's tests split by kind:
//  - lib/api client tests (*.test.ts) are fetch-stubbed pure functions → node env
//  - React component tests (*.test.tsx) need a DOM → jsdom, opted in per-file via
//    a `// @vitest-environment jsdom` docblock (vitest v4 removed
//    environmentMatchGlobs).
// Astro's Vite plugin is required by the mutation-page tests, which render the
// real `.astro` modules. Node remains the default environment.
export default getViteConfig({
  test: {
    environment: 'node',
  },
});
