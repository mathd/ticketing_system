// Tests for scripts/scan-allowlist.mjs (TKT-335, ADR-081).
//
// Each refusal has its own case. The case starts from a valid repository and
// changes one thing, so a case can only go red through the predicate it names.
// The unit cases inject the clock. The CLI cases use the real clock, so their
// dates are calculated at run time and never expire.
import assert from 'node:assert/strict';
import { execFileSync, spawn, spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  AUDIT_TIMEOUT_MS,
  MARGIN_MS,
  auditRepository,
  checkRepository,
  checkWiring,
  runAudit,
  syncWorkspace,
} from './scan-allowlist.mjs';

const SCRIPT = fileURLToPath(new URL('./scan-allowlist.mjs', import.meta.url));
const REPO = fileURLToPath(new URL('..', import.meta.url));
const PACKAGE_MANAGER = JSON.parse(readFileSync(join(REPO, 'package.json'), 'utf8')).packageManager;
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

  it('an entry is refused from one hour before the start of its expiry date (the margin)', () => {
    const files = { '.trivyignore.yaml': allowlist(entry({ approved_at: day(-5), expired_at: day(1) })) };
    const midnight = Date.UTC(2030, 5, 16);
    refuses(files, /expired/, { now: new Date(midnight - 3_600_000) });
    passes(files, { now: new Date(midnight - 3_600_000 - 1) });
  });
});

describe('the verdict of the plain check does not depend on the clock', () => {
  it('a valid repository gives the same result at any clock value', () => {
    for (const now of [new Date(0), NOW, new Date(8.64e15)]) passes({}, { now, expiry: false });
  });

  it('an approval date in the future passes without the clock checks', () => {
    passes({ '.trivyignore.yaml': allowlist(entry({ approved_at: day(1), expired_at: day(5) })) }, { expiry: false });
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

  it('an approval date in the future fails with the clock checks', () => {
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
  for (const name of [
    'trivy.yaml',
    'trivy.yml',
    'trivy-secret.yaml',
    '.trivyignore',
    '.pnpmfile.cjs',
    '.pnpmfile.mjs',
    '.pnpmfile.js',
    '.npmrc',
  ]) {
    it(`a committed ${name} fails`, () => {
      refuses({ [name]: 'x\n' }, new RegExp(`${name.replace('.', '\\.')}.*refused`));
    });
  }
});

describe('pnpm settings outside the mirror', () => {
  // pnpm reads every top-level key of pnpm-workspace.yaml as a setting, and some of
  // them change what `pnpm audit` reports. Only the keys this repository uses pass.
  for (const [key, value] of [
    ['ignore', '\n  - GHSA-w27v-7q3p-w38r'],
    ['ignoreUnfixable', ' true'],
    ['ignoreRegistryErrors', ' true'],
    ['registry', ' https://127.0.0.1:9/'],
    ['auditLevel', ' critical'],
    ['pnpmfile', ' hooks/x.cjs'],
    ['globalPnpmfile', ' hooks/x.cjs'],
    ['production', ' true'],
  ]) {
    it(`a top-level "${key}" setting fails`, () => {
      refuses(
        { 'pnpm-workspace.yaml': `${workspace(['GHSA-7w5x-hrqm-74c2'])}${key}:${value}\n` },
        new RegExp(`top-level key "${key}" is not allowed`),
      );
    });
  }

  it('a key that pnpm expands from the environment fails (top level)', () => {
    refuses(
      {
        'pnpm-workspace.yaml': `${workspace(['GHSA-7w5x-hrqm-74c2'])}"audit\${TKT335_UNSET:-Config}":\n  ignoreGhsas:\n    - GHSA-w27v-7q3p-w38r\n`,
      },
      /\$\{.*expand/,
    );
  });

  it('a key that pnpm expands from the environment fails (nested)', () => {
    refuses(
      { 'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2']).replace("'left-pad@<1.3.0'", "'left-pad@${X:-<1.3.0}'") },
      /\$\{.*expand/,
    );
  });

  it('an anchor anywhere in pnpm-workspace.yaml fails', () => {
    refuses({ 'pnpm-workspace.yaml': workspace(['GHSA-7w5x-hrqm-74c2']).replace('packages:\n', 'packages: &p\n') }, /anchor/);
  });

  it('a pnpm field in the root package.json fails', () => {
    refuses({ 'package.json': '{"name":"x","pnpm":{"auditConfig":{"ignoreGhsas":[]}}}\n' }, /package\.json.*"pnpm"/);
  });

  it('a root package.json without a pnpm field passes', () => {
    passes({ 'package.json': '{"name":"x","private":true}\n' });
  });
});

describe('workflow wiring (structural)', () => {
  const REAL = readFileSync(join(REPO, '.github/workflows/security.yaml'), 'utf8');
  function mutated(old, replacement) {
    assert.ok(REAL.includes(old), `fixture text not found: ${old}`);
    return REAL.replace(old, replacement);
  }
  function refusesWiring(text, pattern) {
    const found = checkWiring(text);
    if (!pattern.source.includes('parse error')) {
      assert.ok(!found.some((p) => /parse error/.test(p)), `the mutated fixture does not parse:\n  ${found.join('\n  ')}`);
    }
    assert.ok(found.some((p) => pattern.test(p)), `want ${pattern}, got:\n  ${found.join('\n  ') || '(none)'}`);
  }
  const WRAPPER = '      - run: node scripts/scan-allowlist.mjs audit\n';
  const TIMEOUT = '    timeout-minutes: 15\n';
  const JOB_HEAD = '  pnpm-audit:\n    needs: changes\n';
  const TRIVY_WITH = '          exit-code: "1"\n';

  it('the real workflow passes', () => {
    assert.deepEqual(checkWiring(REAL), []);
  });

  it('the wrapper step only as text in a job name fails', () => {
    refusesWiring(
      mutated(JOB_HEAD, `  pnpm-audit:\n    name: |\n${WRAPPER.replace('      - ', '      ')}    needs: changes\n`).replace(WRAPPER, ''),
      /pnpm-audit: the last step/,
    );
  });

  it('a quoted "if" key on the wrapper step fails', () => {
    refusesWiring(mutated(WRAPPER, `${WRAPPER}        "if": false\n`), /step key "if"/);
  });

  it('a continue-on-error on the wrapper step fails', () => {
    refusesWiring(mutated(WRAPPER, `${WRAPPER}        continue-on-error: true\n`), /step key "continue-on-error"/);
  });

  it('an env on the wrapper step fails', () => {
    refusesWiring(mutated(WRAPPER, `${WRAPPER}        env:\n          npm_config_registry: https://127.0.0.1:9/\n`), /step key "env"/);
  });

  it('a pnpm-audit job that is always skipped fails', () => {
    refusesWiring(
      mutated("      github.event_name != 'pull_request' || needs.changes.outputs.node == 'true'\n", '      false\n'),
      /pnpm-audit.*"if"/,
    );
  });

  it('a pnpm-audit job with continue-on-error fails', () => {
    refusesWiring(mutated(JOB_HEAD, `${JOB_HEAD}    continue-on-error: true\n`), /pnpm-audit.*key "continue-on-error"/);
  });

  it('the wrapper step missing fails', () => {
    refusesWiring(mutated(WRAPPER, ''), /pnpm-audit: the last step/);
  });

  it('the wrapper replaced by a plain pnpm audit fails', () => {
    refusesWiring(mutated(WRAPPER, '      - run: pnpm audit --audit-level high\n'), /pnpm-audit: the last step/);
  });

  it('a step after the wrapper fails', () => {
    refusesWiring(mutated(WRAPPER, `${WRAPPER}      - run: echo done\n`), /pnpm-audit: the last step/);
  });

  it('another pnpm audit step before the wrapper fails', () => {
    refusesWiring(mutated(WRAPPER, `      - run: pnpm audit --ignore GHSA-w27v-7q3p-w38r\n${WRAPPER}`), /pnpm-audit: the last step/);
  });

  it('the wrapper with an extra argument fails', () => {
    refusesWiring(mutated(WRAPPER, '      - run: node scripts/scan-allowlist.mjs audit || true\n'), /pnpm-audit: the last step/);
  });

  it('a missing timeout-minutes fails', () => {
    refusesWiring(mutated(TIMEOUT, ''), /pnpm-audit: "timeout-minutes"/);
  });

  it('a timeout-minutes above the bound fails', () => {
    refusesWiring(mutated(TIMEOUT, '    timeout-minutes: 120\n'), /pnpm-audit: "timeout-minutes"/);
  });

  it('a timeout-minutes that is an expression fails', () => {
    refusesWiring(mutated(TIMEOUT, "    timeout-minutes: ${{ fromJSON('600') }}\n"), /pnpm-audit: "timeout-minutes"/);
  });

  it('a pnpm-audit job that needs another job fails', () => {
    refusesWiring(mutated(JOB_HEAD, '  pnpm-audit:\n    needs: govulncheck\n'), /pnpm-audit: "needs"/);
  });

  it('a workflow-level env fails', () => {
    refusesWiring(mutated('permissions:\n  contents: read\n', 'permissions:\n  contents: read\n\nenv:\n  npm_config_registry: https://127.0.0.1:9/\n'), /workflow key "env"/);
  });

  it('a workflow-level defaults fails', () => {
    refusesWiring(mutated('permissions:\n  contents: read\n', 'permissions:\n  contents: read\n\ndefaults:\n  run:\n    shell: bash\n'), /workflow key "defaults"/);
  });

  it('.trivyignore.yaml missing from the changes filter fails', () => {
    refusesWiring(mutated('pnpm-workspace\\.yaml|\\.trivyignore\\.yaml)$', 'pnpm-workspace\\.yaml)$'), /changes.*\.trivyignore/);
  });

  it('a non-pull-request run that does not set node=true fails', () => {
    refusesWiring(mutated("printf 'go=true\\nnode=true\\n'", "printf 'go=true\\nnode=false\\n'"), /changes: a run that is not a pull request/);
  });

  it('a continue-on-error on the Trivy step fails', () => {
    refusesWiring(mutated('      - uses: aquasecurity/trivy-action@', '      - continue-on-error: true\n        uses: aquasecurity/trivy-action@'), /Trivy step key "continue-on-error"/);
  });

  it('a step-level if on the Trivy step fails', () => {
    refusesWiring(mutated('      - uses: aquasecurity/trivy-action@', '      - if: false\n        uses: aquasecurity/trivy-action@'), /Trivy step key "if"/);
  });

  it('exit-code "0" fails', () => {
    refusesWiring(mutated(TRIVY_WITH, '          exit-code: "0"\n'), /Trivy input "exit-code"/);
  });

  it('a severity change fails', () => {
    refusesWiring(mutated('          severity: HIGH,CRITICAL\n', '          severity: CRITICAL\n'), /Trivy input "severity"/);
  });

  it('a missing trivyignores input fails', () => {
    refusesWiring(mutated('          trivyignores: .trivyignore.yaml\n', ''), /Trivy input "trivyignores"/);
  });

  it('an extra Trivy input fails (skip-files)', () => {
    refusesWiring(mutated(TRIVY_WITH, `${TRIVY_WITH}          skip-files: pnpm-lock.yaml\n`), /Trivy input "skip-files"/);
  });

  it('a job-level if on repository-scan fails', () => {
    refusesWiring(mutated('  repository-scan:\n    runs-on: ubuntu-latest\n', '  repository-scan:\n    if: false\n    runs-on: ubuntu-latest\n'), /repository-scan.*key "if"/);
  });

  it('a duplicate key fails', () => {
    refusesWiring(mutated(TRIVY_WITH, `${TRIVY_WITH}          exit-code: "0"\n`), /parse error/);
  });

  it('a run step in repository-scan fails', () => {
    refusesWiring(mutated('      - uses: aquasecurity/trivy-action@', '      - run: printf "x" > trivy.yaml\n      - uses: aquasecurity/trivy-action@'), /repository-scan: a run step/);
  });

  it('a second Trivy step fails', () => {
    const start = REAL.indexOf('      - uses: aquasecurity/trivy-action@');
    const end = REAL.indexOf('\n\n', start);
    const step = REAL.slice(start, end + 1);
    refusesWiring(mutated(step, `${step}${step}`), /exactly one aquasecurity\/trivy-action step/);
  });

  it('a Trivy version that is not pinned fails', () => {
    refusesWiring(mutated('          version: v0.72.0\n', '          version: latest\n'), /Trivy input "version"/);
  });
});

describe('the audit wrapper with a recording pnpm (offline)', () => {
  // A fake `pnpm` first on PATH records what the wrapper gave it and exits with a
  // chosen status. This is the tier where the wrapper builds the command, the
  // environment and the exit status, so it is asserted here.
  function fixture({ exit = 1, body = '' } = {}) {
    const dir = repo(VALID);
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    writeFileSync(
      join(bin, 'pnpm'),
      `#!/bin/sh\nprintf '%s\\n' "$@" > "${dir}/argv"\nenv > "${dir}/env"\npwd > "${dir}/cwd"\nfind "$HOME" -type f > "${dir}/home"\n${body}exit ${exit}\n`,
      { mode: 0o755 },
    );
    const caller = {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      HOME: join(dir, 'callerhome'),
      PNPM_CONFIG_REGISTRY: 'http://127.0.0.1:9/',
      npm_config_lockfile_dir: '/elsewhere',
      TKT335_LEAK: 'yes',
    };
    mkdirSync(caller.HOME);
    writeFileSync(join(caller.HOME, '.npmrc'), 'registry=http://127.0.0.1:9/\n');
    return { dir, caller };
  }
  function read(dir, name) {
    return readFileSync(join(dir, name), 'utf8');
  }
  function envOf(dir) {
    return Object.fromEntries(
      read(dir, 'env')
        .trim()
        .split('\n')
        .map((line) => [line.slice(0, line.indexOf('=')), line.slice(line.indexOf('=') + 1)]),
    );
  }

  it('runs exactly pnpm audit --audit-level high --ignore-pnpmfile with the registry and the lockfile pinned', () => {
    const { dir, caller } = fixture();
    runAudit(dir, { env: caller, stdio: 'ignore' });
    assert.deepEqual(read(dir, 'argv').trim().split('\n'), [
      'audit',
      '--audit-level',
      'high',
      '--ignore-pnpmfile',
      '--registry=https://registry.npmjs.org/',
      `--lockfile-dir=${dir}`,
    ]);
  });

  it('runs in the repository root', () => {
    const { dir, caller } = fixture();
    runAudit(dir, { env: caller, stdio: 'ignore' });
    assert.equal(read(dir, 'cwd').trim(), dir);
  });

  it('passes no npm or pnpm setting and no other caller variable', () => {
    const { dir, caller } = fixture();
    runAudit(dir, { env: caller, stdio: 'ignore' });
    const env = envOf(dir);
    assert.deepEqual(Object.keys(env).filter((k) => /^(npm|pnpm)_config_/i.test(k)), []);
    assert.equal(env.TKT335_LEAK, undefined);
  });

  it('gives pnpm a fresh HOME with no file in it, and XDG directories under it', () => {
    const { dir, caller } = fixture();
    runAudit(dir, { env: caller, stdio: 'ignore' });
    const env = envOf(dir);
    assert.notEqual(env.HOME, caller.HOME);
    assert.equal(read(dir, 'home').trim(), '');
    for (const key of ['XDG_CONFIG_HOME', 'XDG_CACHE_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME']) {
      assert.ok(env[key]?.startsWith(env.HOME), `${key}=${env[key]} is not under the fresh HOME ${env.HOME}`);
    }
  });

  it('keeps the caller PATH and passes COREPACK_HOME when it exists', () => {
    const { dir, caller } = fixture();
    const corepack = join(dir, 'corepack');
    mkdirSync(corepack);
    runAudit(dir, { env: { ...caller, COREPACK_HOME: corepack }, stdio: 'ignore' });
    const env = envOf(dir);
    assert.equal(env.PATH, caller.PATH);
    assert.equal(env.COREPACK_HOME, corepack);
  });

  for (const status of [0, 1, 7]) {
    it(`passes exit status ${status} through`, () => {
      const { dir, caller } = fixture({ exit: status });
      assert.equal(runAudit(dir, { env: caller, stdio: 'ignore' }).code, status);
    });
  }

  it('fails closed when pnpm is killed by a signal', () => {
    const { dir, caller } = fixture({ exit: 0, body: 'kill -TERM $$\n' });
    const result = runAudit(dir, { env: caller, stdio: 'ignore' });
    assert.notEqual(result.code, 0);
    assert.match(result.message, /signal SIGTERM/);
  });

  it('fails closed when pnpm runs longer than the timeout', () => {
    const { dir, caller } = fixture({ exit: 0, body: 'sleep 5\n' });
    const result = runAudit(dir, { env: caller, stdio: 'ignore', timeoutMs: 300 });
    assert.notEqual(result.code, 0);
    assert.match(result.message, /timed out|ETIMEDOUT|signal/);
  });

  it('fails closed when pnpm cannot start', () => {
    const { dir, caller } = fixture();
    const result = runAudit(dir, { env: { ...caller, PATH: join(dir, 'empty') }, stdio: 'ignore' });
    assert.notEqual(result.code, 0);
    assert.match(result.message, /cannot run pnpm/);
  });

  it('the timeout is shorter than the expiry margin', () => {
    assert.ok(AUDIT_TIMEOUT_MS > 0 && AUDIT_TIMEOUT_MS < MARGIN_MS, `${AUDIT_TIMEOUT_MS} is not below ${MARGIN_MS}`);
  });

  it('the wrapper refuses an expired entry before pnpm runs', () => {
    const { dir, caller } = fixture({ exit: 0 });
    writeFileSync(join(dir, '.trivyignore.yaml'), allowlist(entry({ approved_at: day(-20), expired_at: day(-1) })));
    const result = auditRepository(dir, { now: NOW, env: caller, stdio: 'ignore' });
    assert.equal(result.code, 1);
    assert.match(result.message, /expired/);
    assert.throws(() => read(dir, 'argv'), /ENOENT/);
  });

  it('the wrapper refuses an approval date in the future before pnpm runs', () => {
    const { dir, caller } = fixture({ exit: 0 });
    writeFileSync(join(dir, '.trivyignore.yaml'), allowlist(entry({ approved_at: day(1), expired_at: day(5) })));
    const result = auditRepository(dir, { now: NOW, env: caller, stdio: 'ignore' });
    assert.equal(result.code, 1);
    assert.match(result.message, /future/);
    assert.throws(() => read(dir, 'argv'), /ENOENT/);
  });

  it('the wrapper refuses a committed-file problem before pnpm runs', () => {
    const { dir, caller } = fixture({ exit: 0 });
    writeFileSync(join(dir, 'pnpm-workspace.yaml'), `${workspace(['GHSA-7w5x-hrqm-74c2'])}ignoreUnfixable: true\n`);
    const result = auditRepository(dir, { now: NOW, env: caller, stdio: 'ignore' });
    assert.equal(result.code, 1);
    assert.throws(() => read(dir, 'argv'), /ENOENT/);
  });

  it('the wrapper runs pnpm and passes its status through when the allowlist is accepted', () => {
    const { dir, caller } = fixture({ exit: 1 });
    assert.equal(auditRepository(dir, { now: NOW, env: caller, stdio: 'ignore' }).code, 1);
    assert.ok(read(dir, 'argv').includes('--ignore-pnpmfile'));
  });
});

describe('the audit wrapper with the real pnpm and a local registry', () => {
  // The real pnpm reads the real configuration sources, so each bypass is first
  // shown to work on a plain `pnpm audit` (exit 0), and then shown not to work
  // through the wrapper (exit 1). The registry is a local process that reports
  // one HIGH advisory for smol-toml under /adv/ and nothing under /empty/, so
  // these tests use no network.
  const LOCK_ONE = `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      smol-toml:
        specifier: 1.7.0
        version: 1.7.0

packages:

  smol-toml@1.7.0:
    resolution: {integrity: sha512-aqVvWoyO21L23mb+drl4RmMXbf6N7FdHjAhTRA9ZBL7apWBgfWC16KjrASI+1p9GAroljyMHj6fK67i0UiTNvQ==}
    engines: {node: '>= 18'}

snapshots:

  smol-toml@1.7.0: {}
`;
  const LOCK_NONE = `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .: {}
`;
  const SERVER = `import { createServer } from 'node:http';
const advisory = { 'smol-toml': [{ id: 1, url: 'https://github.com/advisories/GHSA-7w5x-hrqm-74c2',
  title: 'fixture advisory', severity: 'high', vulnerable_versions: '<=1.7.0' }] };
const server = createServer((req, res) => {
  req.resume();
  req.on('end', () => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify(req.url.startsWith('/adv/') ? advisory : {}));
  });
});
server.listen(0, '127.0.0.1', () => console.log(server.address().port));
`;
  const HOOK = "module.exports = { hooks: { updateConfig(config) { config.auditConfig = { ignoreGhsas: ['GHSA-7w5x-hrqm-74c2'] }; return config; } } };\n";
  const HOOK_MJS = "export const hooks = { updateConfig(config) { config.auditConfig = { ignoreGhsas: ['GHSA-7w5x-hrqm-74c2'] }; return config; } };\n";
  const GLOBAL_HOOK = 'module.exports = { hooks: { updateConfig(config) { config.ignoreUnfixable = true; delete config.globalPnpmfile; return config; } } };\n';
  const COREPACK_HOME =
    process.env.COREPACK_HOME ?? join(process.env.XDG_CACHE_HOME ?? join(homedir(), '.cache'), 'node', 'corepack');
  let server;
  let ADV;
  let EMPTY;

  before(async () => {
    const dir = mkdtempSync(join(tmpdir(), 'scan-allowlist-registry-'));
    dirs.push(dir);
    writeFileSync(join(dir, 'server.mjs'), SERVER);
    server = spawn(process.execPath, [join(dir, 'server.mjs')], { stdio: ['ignore', 'pipe', 'inherit'] });
    const port = await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.stdout.once('data', (data) => resolve(String(data).trim()));
    });
    ADV = `http://127.0.0.1:${port}/adv/`;
    EMPTY = `http://127.0.0.1:${port}/empty/`;
  });

  after(() => server?.kill());

  function fixture(files = {}) {
    const dir = repo({
      '.trivyignore.yaml': allowlist(),
      'package.json': `${JSON.stringify({ name: 'fixture', private: true, packageManager: PACKAGE_MANAGER })}\n`,
      'pnpm-workspace.yaml': 'packages: []\nauditConfig:\n  ignoreGhsas: []\n',
      'pnpm-lock.yaml': LOCK_ONE,
      ...files,
    });
    const home = join(dir, 'callerhome');
    mkdirSync(join(home, '.config', 'pnpm'), { recursive: true });
    const caller = { ...process.env, HOME: home, XDG_CONFIG_HOME: join(home, '.config'), COREPACK_HOME };
    return { dir, home, caller };
  }
  // A plain audit, as the pnpm-audit job ran it before the wrapper.
  function plain(dir, env, args = [`--registry=${ADV}`]) {
    return spawnSync('pnpm', ['audit', '--audit-level', 'high', ...args], { cwd: dir, env, encoding: 'utf8' }).status;
  }
  function wrapped(dir, env) {
    return runAudit(dir, { env, registry: ADV, stdio: 'ignore' }).code;
  }

  it('the fixture finding fails both a plain audit and the wrapper', () => {
    const { dir, caller } = fixture();
    assert.equal(plain(dir, caller), 1);
    assert.equal(wrapped(dir, caller), 1);
  });

  it('the local registry reports nothing under /empty/ (the control for the registry cases)', () => {
    const { dir, caller } = fixture();
    assert.equal(plain(dir, caller, [`--registry=${EMPTY}`]), 0);
  });

  it('a local .pnpmfile.cjs updateConfig hook is not run', () => {
    const { dir, caller } = fixture({ '.pnpmfile.cjs': HOOK });
    assert.equal(plain(dir, caller), 0);
    assert.equal(wrapped(dir, caller), 1);
  });

  it('a local .pnpmfile.mjs with a named hooks export is not run', () => {
    const { dir, caller } = fixture({ '.pnpmfile.mjs': HOOK_MJS });
    assert.equal(plain(dir, caller), 0);
    assert.equal(wrapped(dir, caller), 1);
  });

  it('a global pnpmfile in the caller configuration is not run', () => {
    const { dir, home, caller } = fixture();
    writeFileSync(join(home, 'global-hook.cjs'), GLOBAL_HOOK);
    writeFileSync(join(home, '.config', 'pnpm', 'config.yaml'), `globalPnpmfile: ${join(home, 'global-hook.cjs')}\n`);
    assert.equal(plain(dir, caller), 0);
    assert.equal(wrapped(dir, caller), 1);
  });

  it('a lockfile directory from the caller environment is not used', () => {
    const elsewhere = repo({ 'pnpm-lock.yaml': LOCK_NONE, 'package.json': '{"name":"elsewhere","private":true}\n' });
    const { dir, caller } = fixture();
    const env = { ...caller, PNPM_CONFIG_LOCKFILE_DIR: elsewhere };
    assert.equal(plain(dir, env), 0);
    assert.equal(wrapped(dir, env), 1);
  });

  it('a registry from the caller environment is not used', () => {
    const { dir, caller } = fixture();
    const env = { ...caller, PNPM_CONFIG_REGISTRY: EMPTY };
    assert.equal(plain(dir, env, []), 0);
    assert.equal(wrapped(dir, env), 1);
  });

  it('a registry from the caller HOME .npmrc is not used', () => {
    const { dir, home, caller } = fixture();
    writeFileSync(join(home, '.npmrc'), `registry=${EMPTY}\n`);
    assert.equal(plain(dir, caller, []), 0);
    assert.equal(wrapped(dir, caller), 1);
  });

  it('a registry from a local .npmrc is not used', () => {
    const { dir, caller } = fixture({ '.npmrc': `registry=${EMPTY}\n` });
    assert.equal(plain(dir, caller, []), 0);
    assert.equal(wrapped(dir, caller), 1);
  });
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

  // The CLI path that the pnpm-audit job runs, with a recording pnpm first on
  // PATH. The allowlist is empty, so the real clock cannot make it stale.
  function auditCli(exit) {
    const dir = repo({ '.trivyignore.yaml': allowlist(), 'pnpm-workspace.yaml': workspace([]) });
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    writeFileSync(join(bin, 'pnpm'), `#!/bin/sh\nprintf '%s\\n' "$@" > "${dir}/argv"\nexit ${exit}\n`, { mode: 0o755 });
    const run = spawnSync(process.execPath, [SCRIPT, 'audit', '--root', dir], {
      encoding: 'utf8',
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
    });
    return { run, dir };
  }

  it('audit runs the wrapper and passes the pnpm exit status through', () => {
    for (const exit of [0, 1]) {
      const { run, dir } = auditCli(exit);
      assert.equal(run.status, exit, run.stderr);
      assert.ok(readFileSync(join(dir, 'argv'), 'utf8').includes('--ignore-pnpmfile'));
    }
  });

  it("audit gives pnpm none of its own process environment: no npm or pnpm setting, no NODE_OPTIONS, a fresh HOME", () => {
    // The variables are in the REAL environment of the child process, not in an
    // injected parameter, so a wrapper that spreads process.env leaks them here.
    const dir = repo({ '.trivyignore.yaml': allowlist(), 'pnpm-workspace.yaml': workspace([]) });
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    writeFileSync(join(bin, 'pnpm'), `#!/bin/sh\nenv > "${dir}/env"\nexit 1\n`, { mode: 0o755 });
    const callerHome = join(dir, 'callerhome');
    mkdirSync(callerHome);
    const run = spawnSync(process.execPath, [SCRIPT, 'audit', '--root', dir], {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: `${bin}:${process.env.PATH}`,
        HOME: callerHome,
        PNPM_CONFIG_REGISTRY: 'http://127.0.0.1:9/',
        PNPM_CONFIG_LOCKFILE_DIR: '/elsewhere',
        npm_config_ignore_unfixable: 'true',
        NODE_OPTIONS: '--no-deprecation',
        TKT335_LEAK: 'yes',
      },
    });
    assert.equal(run.status, 1, run.stderr);
    const env = Object.fromEntries(
      readFileSync(join(dir, 'env'), 'utf8')
        .trim()
        .split('\n')
        .map((line) => [line.slice(0, line.indexOf('=')), line.slice(line.indexOf('=') + 1)]),
    );
    assert.deepEqual(Object.keys(env).filter((k) => /^(npm|pnpm)_config_/i.test(k)), []);
    assert.equal(env.NODE_OPTIONS, undefined);
    assert.equal(env.TKT335_LEAK, undefined);
    assert.notEqual(env.HOME, callerHome);
    assert.ok(env.HOME?.includes('scan-allowlist-audit-'), `HOME=${env.HOME} is not the fresh directory`);
  });

  it('audit refuses a lapsed entry and does not run pnpm', () => {
    const dir = repo(lapsed);
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    writeFileSync(join(bin, 'pnpm'), `#!/bin/sh\ntouch "${dir}/ran"\nexit 0\n`, { mode: 0o755 });
    const run = spawnSync(process.execPath, [SCRIPT, 'audit', '--root', dir], {
      encoding: 'utf8',
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
    });
    assert.equal(run.status, 1);
    assert.match(run.stderr, /expired/);
    assert.throws(() => readFileSync(join(dir, 'ran')), /ENOENT/);
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
