// Tests for scripts/scan-allowlist.mjs (TKT-335, ADR-081).
//
// Each refusal has its own case. The case starts from a valid repository and
// changes one thing, so a case can only go red through the predicate it names.
// The unit cases inject the clock. The CLI cases use the real clock, so their
// dates are calculated at run time and never expire.
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

import { checkRepository, syncWorkspace } from './scan-allowlist.mjs';

const SCRIPT = fileURLToPath(new URL('./scan-allowlist.mjs', import.meta.url));
const DAY = 86_400_000;
// The injected clock: noon UTC, so "today" is the same date in every zone test.
const NOW = new Date(Date.UTC(2030, 5, 15, 12, 0, 0));

const dirs = [];
after(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
});

function day(offset, from = NOW) {
  const start = Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate());
  return new Date(start + offset * DAY).toISOString().slice(0, 10);
}

const WORKSPACE_HEAD = `packages:
  - web/one
# A comment that sync must keep.
overrides:
  'left-pad@<1.3.0': '^1.3.0'
`;

function workspace(ghsas) {
  const list = ghsas.length === 0 ? ' []\n' : `\n${ghsas.map((g) => `    - ${g}\n`).join('')}`;
  return `${WORKSPACE_HEAD}auditConfig:\n  ignoreGhsas:${list}`;
}

function entry(fields = {}) {
  const base = {
    id: 'CVE-2026-85730',
    statement: '"No patched parent release yet; remediation TKT-999."',
    owner: 'mathd',
    approved_at: day(-10),
    expired_at: day(10),
    pnpm_ids: '\n      - GHSA-7w5x-hrqm-74c2',
    ...fields,
  };
  const lines = [];
  for (const [key, value] of Object.entries(base)) {
    if (value === undefined) continue;
    lines.push(`${key}:${value.startsWith('\n') ? '' : ' '}${value}`);
  }
  return `  - ${lines.join('\n    ')}\n`;
}

function allowlist(...entries) {
  return entries.length === 0 ? 'vulnerabilities: []\n' : `vulnerabilities:\n${entries.join('')}`;
}

function repo(files) {
  const dir = mkdtempSync(join(tmpdir(), 'scan-allowlist-'));
  dirs.push(dir);
  for (const [name, content] of Object.entries(files)) {
    writeFileSync(join(dir, name), content);
  }
  return dir;
}

const VALID = {
  '.trivyignore.yaml': allowlist(entry()),
  'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2']),
};

function problems(files, { now = NOW, expiry = true } = {}) {
  return checkRepository(repo({ ...VALID, ...files }), { now, expiry });
}

function passes(files, options) {
  assert.deepEqual(problems(files, options), []);
}

function refuses(files, pattern, options) {
  const found = problems(files, options);
  assert.ok(
    found.some((p) => pattern.test(p)),
    `want a problem matching ${pattern}, got:\n  ${found.join('\n  ') || '(none)'}`,
  );
}

describe('valid allowlists pass', () => {
  it('an empty allowlist with an empty mirror passes', () => {
    passes({ '.trivyignore.yaml': allowlist(), 'pnpm-workspace.yaml': workspace([]) });
  });

  it('a valid entry passes, with and without the expiry check', () => {
    passes({});
    passes({}, { expiry: false });
  });

  it('a Go CVE with no pnpm alias passes with an empty mirror', () => {
    passes({
      '.trivyignore.yaml': allowlist(entry({ id: 'CVE-2026-78669', pnpm_ids: undefined })),
      'pnpm-workspace.yaml': workspace([]),
    });
  });

  it('a GHSA id feeds the mirror by itself', () => {
    const ghsaEntry = entry({ id: 'GHSA-26w7-cxv4-gfx2', pnpm_ids: undefined });
    passes({
      '.trivyignore.yaml': allowlist(ghsaEntry),
      'pnpm-workspace.yaml': workspace(['GHSA-26w7-cxv4-gfx2']),
    });
    refuses(
      { '.trivyignore.yaml': allowlist(ghsaEntry), 'pnpm-workspace.yaml': workspace([]) },
      /mirror/,
    );
  });

  it('a lifetime of exactly 30 days passes', () => {
    passes({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(-15), expired_at: day(15) })) });
  });

  it('approval today passes', () => {
    passes({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(0), expired_at: day(5) })) });
  });
});

describe('expiry (the clock-dependent check)', () => {
  const expired = { '.trivyignore.yaml': allowlist(entry({ approved_at: day(-20), expired_at: day(-1) })) };

  it('an expired entry fails with the expiry check, even with no scanner result', () => {
    refuses(expired, /expired/);
  });

  it('the same entry passes the deterministic check', () => {
    passes(expired, { expiry: false });
  });

  it('an entry that expires today has expired (expiry is the start of the date, UTC)', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(-5), expired_at: day(0) })) }, /expired/);
  });

  it('the boundary is the first millisecond of the expiry date', () => {
    const files = { '.trivyignore.yaml': allowlist(entry({ approved_at: day(-5), expired_at: day(1) })) };
    const midnight = new Date(Date.UTC(2030, 5, 16));
    refuses(files, /expired/, { now: midnight });
    passes(files, { now: new Date(midnight.getTime() - 1) });
  });
});

describe('lifetime and approval date', () => {
  it('31 days fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(-15), expired_at: day(16) })) }, /lifetime/);
  });

  it('zero days fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(0), expired_at: day(0) })) }, /lifetime/, {
      expiry: false,
    });
  });

  it('a negative lifetime fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(-2), expired_at: day(-5) })) }, /lifetime/, {
      expiry: false,
    });
  });

  it('an approval date in the future fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(1), expired_at: day(5) })) }, /future/);
  });
});

describe('required fields', () => {
  for (const field of ['id', 'statement', 'owner', 'approved_at', 'expired_at']) {
    it(`a missing ${field} fails`, () => {
      refuses(
        { '.trivyignore.yaml': allowlist(entry({ [field]: undefined })) },
        new RegExp(`missing "${field}"`),
      );
    });
  }

  it('a blank statement fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ statement: '"   "' })) }, /"statement" is blank/);
  });

  it('a blank id fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ id: '""' })) }, /malformed id/);
  });

  it('an owner outside the allowed set fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ owner: 'someone' })) }, /not in the allowed set/);
  });

  it('a non-string statement fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ statement: '42' })) }, /"statement" must be a string/);
  });
});

describe('dates', () => {
  it('a quoted expired_at fails (Trivy stops with a fatal error on it)', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ expired_at: `"${day(10)}"` })) }, /expired_at.*unquoted/);
  });

  it('a quoted approved_at fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ approved_at: `'${day(-10)}'` })) }, /approved_at.*unquoted/);
  });

  it('an impossible calendar date fails (no roll-over to the next month)', () => {
    const feb = new Date(Date.UTC(2030, 1, 20, 12));
    refuses(
      { '.trivyignore.yaml': allowlist(entry({ approved_at: '2030-02-15', expired_at: '2030-02-30' })) },
      /not a calendar date/,
      { now: feb },
    );
  });

  it('a non-canonical date fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ expired_at: '2030-6-25' })) }, /expired_at.*YYYY-MM-DD/);
  });

  it('a timestamp with an offset fails', () => {
    refuses(
      { '.trivyignore.yaml': allowlist(entry({ expired_at: `${day(10)}T00:00:00+14:00` })) },
      /expired_at.*YYYY-MM-DD/,
    );
  });
});

describe('identifiers and duplicates', () => {
  it('a malformed CVE id fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ id: 'cve-2026-85730' })) }, /malformed id/);
  });

  it('a malformed GHSA alias fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ pnpm_ids: '\n      - GHSA-7W5X-HRQM-74C2' })) }, /malformed GHSA/);
  });

  it('a CVE in pnpm_ids fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ pnpm_ids: '\n      - CVE-2026-85730' })) }, /malformed GHSA/);
  });

  it('pnpm_ids that is not a list fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ pnpm_ids: 'GHSA-7w5x-hrqm-74c2' })) }, /pnpm_ids.*list/);
  });

  it('a duplicate id fails', () => {
    refuses(
      {
        '.trivyignore.yaml': allowlist(entry(), entry({ pnpm_ids: '\n      - GHSA-w27v-7q3p-w38r' })),
        'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2', 'GHSA-w27v-7q3p-w38r']),
      },
      /duplicate id/,
    );
  });

  it('a GHSA mapped by two entries fails', () => {
    refuses(
      {
        '.trivyignore.yaml': allowlist(entry(), entry({ id: 'CVE-2026-84370' })),
      },
      /duplicate GHSA/,
    );
  });

  it('a GHSA id repeated in its own pnpm_ids fails', () => {
    refuses(
      {
        '.trivyignore.yaml': allowlist(
          entry({ id: 'GHSA-26w7-cxv4-gfx2', pnpm_ids: '\n      - GHSA-26w7-cxv4-gfx2' }),
        ),
        'pnpm-workspace.yaml': workspace(['GHSA-26w7-cxv4-gfx2']),
      },
      /duplicate GHSA/,
    );
  });
});

describe('shape and YAML features', () => {
  it('an unsupported entry field fails (paths)', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ paths: '[web/**]' })) }, /unsupported field "paths"/);
  });

  it('an unsupported entry field fails (purls)', () => {
    refuses(
      { '.trivyignore.yaml': allowlist(entry({ purls: '[pkg:npm/smol-toml@1.7.0]' })) },
      /unsupported field "purls"/,
    );
  });

  for (const section of ['secrets', 'misconfigurations', 'licenses', 'policy']) {
    it(`a "${section}" section fails`, () => {
      refuses(
        { '.trivyignore.yaml': `${allowlist(entry())}${section}:\n  - id: X\n` },
        new RegExp(`unsupported section "${section}"`),
      );
    });
  }

  it('an anchor fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ owner: '&o mathd' })) }, /anchor/);
  });

  it('an alias fails', () => {
    refuses(
      {
        '.trivyignore.yaml': allowlist(
          entry({ approved_at: `&a ${day(-10)}` }),
          entry({ id: 'CVE-2026-84370', approved_at: '*a', pnpm_ids: '\n      - GHSA-w27v-7q3p-w38r' }),
        ),
        'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2', 'GHSA-w27v-7q3p-w38r']),
      },
      /alias/,
    );
  });

  it('an explicit tag fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ expired_at: `!!str ${day(10)}` })) }, /tag/);
  });

  it('a second document fails', () => {
    refuses({ '.trivyignore.yaml': `${allowlist(entry())}---\nvulnerabilities: []\n` }, /document/);
  });

  it('a YAML directive fails', () => {
    refuses({ '.trivyignore.yaml': `%YAML 1.1\n---\n${allowlist(entry())}` }, /directive/);
  });

  it('a duplicate key fails', () => {
    refuses(
      { '.trivyignore.yaml': allowlist(entry({ expired_at: `${day(10)}\n    expired_at: ${day(20)}` })) },
      /DUPLICATE_KEY|duplicate key/i,
    );
  });

  it('a merge key fails', () => {
    refuses({ '.trivyignore.yaml': allowlist(entry({ '<<': '{owner: mathd}' })) }, /unsupported field "<<"/);
  });

  it('malformed YAML fails', () => {
    refuses({ '.trivyignore.yaml': 'vulnerabilities: [\n' }, /parse/);
  });

  it('an empty file fails', () => {
    refuses({ '.trivyignore.yaml': '' }, /empty|document/);
  });

  it('a missing allowlist fails', () => {
    const dir = repo({ 'pnpm-workspace.yaml': workspace([]) });
    const found = checkRepository(dir, { now: NOW, expiry: true });
    assert.ok(found.some((p) => /\.trivyignore\.yaml.*(missing|cannot read)/.test(p)), found.join('\n'));
  });

  it('a top level that is not a map fails', () => {
    refuses({ '.trivyignore.yaml': '- id: CVE-2026-85730\n' }, /top level must be a map/);
  });

  it('a list that is not a sequence fails', () => {
    refuses({ '.trivyignore.yaml': 'vulnerabilities: none\n' }, /must be a list/);
  });

  it('a null list fails', () => {
    refuses({ '.trivyignore.yaml': 'vulnerabilities:\n' }, /must be a list/);
  });

  it('an entry that is not a map fails', () => {
    refuses({ '.trivyignore.yaml': 'vulnerabilities:\n  - CVE-2026-85730\n' }, /must be a map/);
  });
});

describe('the pnpm mirror', () => {
  it('a missing auditConfig fails', () => {
    refuses({ 'pnpm-workspace.yaml': WORKSPACE_HEAD }, /auditConfig/);
  });

  it('a mirror with an extra GHSA fails', () => {
    refuses({ 'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2', 'GHSA-w27v-7q3p-w38r']) }, /mirror/);
  });

  it('a mirror without the derived GHSA fails', () => {
    refuses({ 'pnpm-workspace.yaml': workspace([]) }, /mirror/);
  });

  it('a mirror with a different case fails (sync writes the canonical form)', () => {
    refuses({ 'pnpm-workspace.yaml': workspace(['ghsa-7w5x-hrqm-74c2']) }, /mirror/);
  });

  it('an unsorted mirror fails', () => {
    const two = allowlist(entry(), entry({ id: 'CVE-2026-84370', pnpm_ids: '\n      - GHSA-w27v-7q3p-w38r' }));
    passes({ '.trivyignore.yaml': two, 'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2', 'GHSA-w27v-7q3p-w38r']) });
    refuses({ '.trivyignore.yaml': two, 'pnpm-workspace.yaml': workspace(['GHSA-w27v-7q3p-w38r', 'GHSA-7w5x-hrqm-74c2']) }, /mirror/);
  });

  it('another auditConfig key fails', () => {
    refuses(
      { 'pnpm-workspace.yaml': `${workspace(['GHSA-7w5x-hrqm-74c2'])}  ignoreCves:\n    - CVE-2026-1\n` },
      /auditConfig.*"ignoreCves"/,
    );
  });

  it('an ignoreGhsas that is not a list fails', () => {
    refuses({ 'pnpm-workspace.yaml': `${WORKSPACE_HEAD}auditConfig:\n  ignoreGhsas: GHSA-7w5x-hrqm-74c2\n` }, /ignoreGhsas.*list/);
  });

  it('an anchor in auditConfig fails', () => {
    refuses({ 'pnpm-workspace.yaml': `${WORKSPACE_HEAD}auditConfig: &a\n  ignoreGhsas:\n    - GHSA-7w5x-hrqm-74c2\n` }, /anchor/);
  });
});

describe('other suppression files', () => {
  for (const name of ['trivy.yaml', 'trivy.yml', 'trivy-secret.yaml', '.trivyignore']) {
    it(`a committed ${name} fails`, () => {
      refuses({ [name]: 'x\n' }, new RegExp(`${name.replace('.', '\\.')}.*refused`));
    });
  }
});

describe('sync', () => {
  it('writes the derived mirror, keeps the rest of the file, and is idempotent', () => {
    const two = allowlist(
      entry({ id: 'CVE-2026-84370', pnpm_ids: '\n      - GHSA-w27v-7q3p-w38r' }),
      entry(),
      entry({ id: 'GHSA-26w7-cxv4-gfx2', pnpm_ids: undefined }),
    );
    const dir = repo({ '.trivyignore.yaml': two, 'pnpm-workspace.yaml': workspace([]) });
    syncWorkspace(dir, { now: NOW });
    const once = readFileSync(join(dir, 'pnpm-workspace.yaml'), 'utf8');
    assert.equal(once, workspace(['GHSA-26w7-cxv4-gfx2', 'GHSA-7w5x-hrqm-74c2', 'GHSA-w27v-7q3p-w38r']));
    syncWorkspace(dir, { now: NOW });
    assert.equal(readFileSync(join(dir, 'pnpm-workspace.yaml'), 'utf8'), once);
    assert.deepEqual(checkRepository(dir, { now: NOW, expiry: true }), []);
  });

  it('adds auditConfig to a workspace that has none', () => {
    const dir = repo({ '.trivyignore.yaml': allowlist(), 'pnpm-workspace.yaml': WORKSPACE_HEAD });
    syncWorkspace(dir, { now: NOW });
    assert.equal(readFileSync(join(dir, 'pnpm-workspace.yaml'), 'utf8'), workspace([]));
  });

  it('refuses to write a mirror from an invalid allowlist', () => {
    const dir = repo({ '.trivyignore.yaml': allowlist(entry({ owner: 'someone' })), 'pnpm-workspace.yaml': workspace([]) });
    assert.throws(() => syncWorkspace(dir, { now: NOW }), /not in the allowed set/);
    assert.equal(readFileSync(join(dir, 'pnpm-workspace.yaml'), 'utf8'), workspace([]));
  });
});

describe('command line (real clock)', () => {
  const today = new Date();
  function cli(args, files) {
    const dir = repo(files);
    return spawnSync(process.execPath, [SCRIPT, ...args, '--root', dir], { encoding: 'utf8' });
  }
  const live = {
    '.trivyignore.yaml': allowlist(entry({ approved_at: day(-3, today), expired_at: day(7, today) })),
    'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2']),
  };
  const lapsed = {
    '.trivyignore.yaml': allowlist(entry({ approved_at: day(-10, today), expired_at: day(-1, today) })),
    'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2']),
  };

  it('check exits 0 on a valid repository', () => {
    const run = cli(['check'], live);
    assert.equal(run.status, 0, run.stderr);
  });

  it('check --now exits 0 on a live entry', () => {
    const run = cli(['check', '--now'], live);
    assert.equal(run.status, 0, run.stderr);
  });

  it('check exits 0 on a lapsed entry, and check --now exits 1 and names it', () => {
    assert.equal(cli(['check'], lapsed).status, 0);
    const run = cli(['check', '--now'], lapsed);
    assert.equal(run.status, 1);
    assert.match(run.stderr, /CVE-2026-85730.*expired/);
  });

  it('check exits 1 on a refused repository', () => {
    const run = cli(['check'], { ...live, 'trivy.yaml': 'x\n' });
    assert.equal(run.status, 1);
    assert.match(run.stderr, /trivy\.yaml/);
  });

  it('an unknown command or option exits 2', () => {
    assert.equal(cli(['frobnicate'], live).status, 2);
    assert.equal(cli(['check', '--later'], live).status, 2);
  });

  it('sync writes the mirror that check then accepts', () => {
    const dir = repo({ ...live, 'pnpm-workspace.yaml': workspace([]) });
    execFileSync(process.execPath, [SCRIPT, 'sync', '--root', dir]);
    assert.equal(readFileSync(join(dir, 'pnpm-workspace.yaml'), 'utf8'), workspace(['GHSA-7w5x-hrqm-74c2']));
    const run = spawnSync(process.execPath, [SCRIPT, 'check', '--now', '--root', dir], { encoding: 'utf8' });
    assert.equal(run.status, 0, run.stderr);
  });
});
