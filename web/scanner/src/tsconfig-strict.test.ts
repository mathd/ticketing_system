import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * TKT-302: the scanner had no `strict` key while both Astro apps extend
 * `astro/tsconfigs/strict`, so `strictNullChecks` and `noImplicitAny` were off
 * for the one app that runs at a live gate.
 *
 * This asserts the EFFECTIVE compiler configuration, not that the code compiles.
 * The distinction is the whole point: the source has no strict violation today,
 * so `tsc` exits 0 with or without the flag. A compile-only check would pass
 * just as happily with `strict` deleted — a green check that is about something
 * else. Delete `"strict": true` from tsconfig.app.json and this test goes red
 * while the build stays green.
 *
 * Reading the JSON file directly would be weaker: `strict` can arrive through
 * `extends`, and a future refactor that moved it into a base config should keep
 * this green rather than force an edit here. `--showConfig` resolves the chain.
 */
describe('scanner TypeScript configuration', () => {
  it('compiles under strict, resolved from the effective config', () => {
    const out = execFileSync(
      'node',
      ['./node_modules/typescript/bin/tsc', '--showConfig', '-p', 'tsconfig.app.json'],
      { cwd: resolve(dirname(fileURLToPath(import.meta.url)), '..'), encoding: 'utf8' },
    );
    const config = JSON.parse(out) as { compilerOptions?: Record<string, unknown> };
    expect(config.compilerOptions?.strict).toBe(true);
  });
});
