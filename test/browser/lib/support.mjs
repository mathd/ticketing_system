import { execFileSync } from 'node:child_process';

export const ORGANIZER = '00000000-0000-0000-0000-000000000001';

export function sql(container, database, statement) {
  return execFileSync(
    'docker',
    ['exec', '-i', container, 'psql', '-U', 'postgres', '-d', database, '-tAqc', statement],
    { encoding: 'utf8' },
  ).trim();
}

export function provisionAdmin(container, identifier, password) {
  execFileSync(
    'docker',
    [
      'exec', '-i', container, '/app', 'provision-staff',
      '--organizer-id', ORGANIZER,
      '--identifier', identifier,
      '--role', 'admin',
    ],
    { input: password, encoding: 'utf8' },
  );
}

export function enrolScanner(container, label) {
  const output = execFileSync(
    'docker',
    ['exec', '-i', container, '/app', 'enrol-scanner', ORGANIZER, label],
    { encoding: 'utf8' },
  );
  const token = output.match(/^  X-Scanner-Token: (.+)$/m)?.[1]?.trim();
  if (!token) throw new Error('access enrol-scanner returned no pairing token');
  return token;
}

export async function signIn(page, identifier, password) {
  await page.goto('/admin/login', { waitUntil: 'domcontentloaded' });
  await page.fill('#identifier', identifier);
  await page.fill('#password', password);
  await Promise.all([
    page.waitForURL(
      (url) => url.pathname === '/admin/' && url.search === '' && url.hash === '',
    ),
    page.click('button[type=submit]'),
  ]);
  await page.getByRole('button', { name: 'Sign out', exact: true }).waitFor({ state: 'visible' });
}

export async function submitForm(page, button) {
  const [request] = await Promise.all([
    page.waitForRequest((candidate) => candidate.method() === 'POST'),
    button.click(),
  ]);
  await page.waitForLoadState('domcontentloaded');
  return request;
}

export function resultRecorder(label) {
  const results = [];
  let failed = false;

  return {
    check(name, ok, detail = '') {
      results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `: ${detail}` : ''}`);
      if (!ok) failed = true;
    },
    finish() {
      console.log(results.join('\n'));
      console.log(failed ? `\n${label} FAILED` : `\n${label} passed`);
      return !failed;
    },
  };
}
