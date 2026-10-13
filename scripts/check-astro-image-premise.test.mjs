// Tests for scripts/check-astro-image-premise.mjs (TKT-504, CVE-2026-93748).
//
// Each case starts from a clean fixture repository and changes one thing. A
// refusal case asserts that its own rule is the ONLY rule that fires, so a case
// can only go red through the rule it names. The cases run the real CLI with
// --root, so the argument parsing and the exit status are part of what they test.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const SCRIPT = fileURLToPath(new URL('./check-astro-image-premise.mjs', import.meta.url));
const REPO = fileURLToPath(new URL('..', import.meta.url));
const APPS = ['web/backoffice', 'web/storefront'];

const dirs = [];
after(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
});

// A plain object, so the fixture needs no node_modules. It has the same shape
// as the real configs after defineConfig, react() and node().
function config(fields = {}) {
  const base = {
    output: "'server'",
    adapter: "{ name: '@astrojs/node' }",
    integrations: "[{ name: '@astrojs/react' }]",
    ...fields,
  };
  const lines = Object.entries(base)
    .filter(([, value]) => value !== undefined)
    .map(([key, value]) => `  ${key}: ${value},`);
  return `export default {\n${lines.join('\n')}\n};\n`;
}

const CLEAN_SOURCE = "import { useState } from 'react';\nexport const x = 1;\n";

// files: { 'web/backoffice/src/a.ts': '...' }. null removes a default file.
function fixture(files = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'astro-image-premise-'));
  dirs.push(dir);
  const all = {};
  for (const app of APPS) {
    all[`${app}/astro.config.mjs`] = config();
    all[`${app}/src/pages/index.astro`] = '---\nconst title = "x";\n---\n<img src="/a.png" alt="" />\n';
    all[`${app}/src/lib/clean.ts`] = CLEAN_SOURCE;
  }
  Object.assign(all, files);
  for (const [name, content] of Object.entries(all)) {
    if (content === null) continue;
    mkdirSync(dirname(join(dir, name)), { recursive: true });
    writeFileSync(join(dir, name), content);
  }
  return dir;
}

function run(args, options = {}) {
  const result = spawnSync(process.execPath, [SCRIPT, ...args], { encoding: 'utf8', ...options });
  return { status: result.status, stdout: result.stdout, stderr: result.stderr };
}

// The diagnostic lines that name a rule.
function ruleLines(stderr) {
  return stderr.split('\n').filter((line) => / rule /.test(line));
}

function passes(files) {
  const result = run(['--root', fixture(files)]);
  assert.equal(result.status, 0, `want exit 0, got ${result.status}:\n${result.stderr}`);
}

// Exactly one rule fires, and it is the named one at the named place.
function refusesOnly(files, where, rule, root = fixture(files)) {
  const result = run(['--root', root]);
  assert.equal(result.status, 1, `want exit 1, got ${result.status}:\n${result.stdout}${result.stderr}`);
  const lines = ruleLines(result.stderr);
  assert.equal(lines.length, 1, `want exactly one rule line, got:\n  ${lines.join('\n  ') || '(none)'}`);
  const [line] = lines;
  assert.ok(line.includes(`${where}: rule ${rule}:`), `want "${where}: rule ${rule}:", got:\n  ${line}`);
  assert.ok(line.includes('CVE-2026-93748'), `the diagnostic must name the CVE:\n  ${line}`);
  assert.ok(line.includes('TKT-504'), `the diagnostic must name the ticket:\n  ${line}`);
}

describe('clean trees pass', () => {
  it('a clean fixture passes', () => {
    passes({});
  });

  it('the real repository passes', () => {
    const result = run([], { cwd: REPO });
    assert.equal(result.status, 0, `want exit 0, got ${result.status}:\n${result.stderr}`);
  });

  it('empty image.domains and image.remotePatterns pass', () => {
    passes({ 'web/backoffice/astro.config.mjs': config({ image: '{ domains: [], remotePatterns: [] }' }) });
  });

  it('a nested integrations array with only allowed names passes', () => {
    passes({ 'web/storefront/astro.config.mjs': config({ integrations: "[[{ name: '@astrojs/react' }], false, null]" }) });
  });

  it('a config with no adapter passes', () => {
    passes({ 'web/backoffice/astro.config.mjs': config({ adapter: undefined }) });
  });

  it('a config comment that says image passes', () => {
    passes({ 'web/backoffice/astro.config.mjs': `// the runtime image ships dist only\n${config()}` });
  });

  it('a marker in a file that is not source passes', () => {
    passes({ 'web/storefront/notes.txt': "import { Image } from 'astro:assets';\n" });
  });

  for (const skipped of ['node_modules', 'dist', '.astro']) {
    it(`a marker under ${skipped} passes`, () => {
      passes({ [`web/storefront/${skipped}/x/index.js`]: "import { getImage } from 'astro:assets';\n" });
    });
  }
});

describe('configuration rules refuse', () => {
  it('a non-empty image.domains refuses', () => {
    refusesOnly(
      { 'web/backoffice/astro.config.mjs': config({ image: "{ domains: ['cdn.example.com'] }" }) },
      'web/backoffice/astro.config.mjs',
      'image.domains',
    );
  });

  it('a non-empty image.remotePatterns refuses', () => {
    refusesOnly(
      { 'web/storefront/astro.config.mjs': config({ image: "{ remotePatterns: [{ protocol: 'https' }] }" }) },
      'web/storefront/astro.config.mjs',
      'image.remotePatterns',
    );
  });

  it('a set image.service refuses', () => {
    refusesOnly(
      { 'web/storefront/astro.config.mjs': config({ image: "{ service: { entrypoint: './src/lib/image-service.mjs' } }" }) },
      'web/storefront/astro.config.mjs',
      'image.service',
    );
  });

  it('an integration outside the allowlist refuses', () => {
    refusesOnly(
      { 'web/backoffice/astro.config.mjs': config({ integrations: "[{ name: '@astrojs/react' }, { name: '@astrojs/mdx' }]" }) },
      'web/backoffice/astro.config.mjs',
      'integrations',
    );
  });

  it('a disallowed integration inside a nested array refuses', () => {
    refusesOnly(
      { 'web/storefront/astro.config.mjs': config({ integrations: "[[{ name: '@astrojs/react' }, [{ name: '@astrojs/markdoc' }]]]" }) },
      'web/storefront/astro.config.mjs',
      'integrations',
    );
  });

  it('an adapter outside the allowlist refuses', () => {
    refusesOnly(
      { 'web/backoffice/astro.config.mjs': config({ adapter: "{ name: '@astrojs/vercel' }" }) },
      'web/backoffice/astro.config.mjs',
      'adapter',
    );
  });
});

describe('import marker rules refuse', () => {
  it('astro:assets refuses', () => {
    refusesOnly(
      { 'web/storefront/src/components/Hero.astro': "---\nimport { Image as Img } from 'astro:assets';\n---\n<Img />\n" },
      'web/storefront/src/components/Hero.astro:2',
      'astro:assets',
    );
  });

  it('astro/assets refuses, with a subpath', () => {
    refusesOnly(
      { 'web/backoffice/src/lib/img.ts': "// helper\nexport { imageConfig } from 'astro/assets/utils';\n" },
      'web/backoffice/src/lib/img.ts:2',
      'astro/assets',
    );
  });

  it('virtual:astro:get-image refuses', () => {
    refusesOnly(
      { 'web/storefront/src/lib/x.mjs': "const m = await import('virtual:astro:get-image');\n" },
      'web/storefront/src/lib/x.mjs:1',
      'virtual:astro:get-image',
    );
  });

  it('virtual:image-service refuses', () => {
    refusesOnly(
      { 'web/backoffice/test/svc.test.ts': "\n\nimport svc from 'virtual:image-service';\n" },
      'web/backoffice/test/svc.test.ts:3',
      'virtual:image-service',
    );
  });

  it('a direct Image.astro component import refuses', () => {
    refusesOnly(
      { 'web/storefront/src/pages/a.astro': "---\nimport I from 'astro/components/Image.astro';\n---\n" },
      'web/storefront/src/pages/a.astro:2',
      'astro/components',
    );
  });

  it('a direct ResponsivePicture.astro component import refuses', () => {
    refusesOnly(
      { 'web/backoffice/src/new-file.tsx': "import P from 'astro/components/ResponsivePicture.astro';\n" },
      'web/backoffice/src/new-file.tsx:1',
      'astro/components',
    );
  });

  it('a marker in a Markdown file refuses', () => {
    refusesOnly(
      { 'web/storefront/src/content/a.mdx': "import { Picture } from 'astro:assets';\n" },
      'web/storefront/src/content/a.mdx:1',
      'astro:assets',
    );
  });
});

describe('the guard fails closed', () => {
  for (const app of APPS) {
    it(`a missing ${app} refuses`, () => {
      const root = fixture();
      rmSync(join(root, app), { recursive: true, force: true });
      const result = run(['--root', root]);
      assert.equal(result.status, 1);
      assert.match(result.stderr, new RegExp(`${app}: rule app-missing:`));
    });

    it(`a missing ${app}/astro.config.mjs refuses`, () => {
      const result = run(['--root', fixture({ [`${app}/astro.config.mjs`]: null })]);
      assert.equal(result.status, 1);
      assert.match(result.stderr, new RegExp(`${app}/astro.config.mjs: rule config-unreadable:`));
    });
  }

  it('a symlink in the source tree refuses, because the scan does not follow it', () => {
    const root = fixture();
    symlinkSync(join(root, 'web/storefront/src/lib'), join(root, 'web/storefront/src/linked'));
    refusesOnly({}, 'web/storefront/src/linked', 'symlink', root);
  });

  it('a config that throws on import refuses', () => {
    const result = run(['--root', fixture({ 'web/storefront/astro.config.mjs': "throw new Error('boom');\n" })]);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /web\/storefront\/astro\.config\.mjs: rule config-unreadable:.*boom/);
  });

  it('a config with no default export object refuses', () => {
    const result = run(['--root', fixture({ 'web/backoffice/astro.config.mjs': 'export const x = 1;\n' })]);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /web\/backoffice\/astro\.config\.mjs: rule config-unreadable:/);
  });

  it('integrations that are not an array refuse', () => {
    refusesOnly(
      { 'web/backoffice/astro.config.mjs': config({ integrations: "{ name: '@astrojs/react' }" }) },
      'web/backoffice/astro.config.mjs',
      'integrations',
    );
  });

  it('an unknown option is a usage error', () => {
    const result = run(['--nope']);
    assert.equal(result.status, 2);
  });
});
