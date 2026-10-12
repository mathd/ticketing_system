// @vitest-environment node
//
// TKT-368. The back-office shell and the event builder render in the operator's
// UI language, read from the `bo_ui_language` cookie. These render the REAL
// pages through Astro's container, with the cookie on the request, so the read
// path (cookie -> page -> Layout) is the one production runs. The container does
// not run the middleware or the gateway, so nothing here claims anything about
// the origin check, the form action a browser resolves, or the cookie a browser
// stores: test/browser/event-builder-language.mjs owns those.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const STAFF = '60000000-0000-4000-8000-000000000001';
const ORGANIZER = '00000000-0000-4000-8000-000000000001';
const VENUE = '10000000-0000-4000-8000-000000000001';
const EVENT = '30000000-0000-4000-8000-000000000001';
const PERFORMANCE = '40000000-0000-4000-8000-000000000001';
const TICKET_TYPE = '50000000-0000-4000-8000-000000000001';
const ASSERTION = `v2.catalog-org/test.${STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(86)}`;

const principal = {
  staffId: STAFF,
  organizerId: ORGANIZER,
  role: 'admin' as const,
  organizerAssertion: ASSERTION,
};

type Lang = 'en' | 'fr' | null;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function venues() {
  return {
    venues: [
      { id: VENUE, organizer_id: ORGANIZER, name: 'Hall', ga_capacity: 1200, created_at: '2026-09-01T10:00:00Z' },
    ],
  };
}

function request(path: string, lang: Lang, fields?: Record<string, string>): Request {
  const headers: Record<string, string> = {};
  if (lang) headers.cookie = `bo_sid=session-token; bo_ui_language=${lang}`;
  if (!fields) return new Request(`http://backoffice.test${path}`, { headers });
  headers['content-type'] = 'application/x-www-form-urlencoded';
  return new Request(`http://backoffice.test${path}`, {
    method: 'POST',
    headers,
    body: new URLSearchParams(fields),
  });
}

async function render(
  page: string,
  req: Request,
  options: { staff?: boolean } = {},
): Promise<{ status: number; html: string }> {
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create({ astroConfig: { base: '/admin' } });
  const mod = await import(/* @vite-ignore */ page);
  const response = await container.renderToResponse(mod.default, {
    request: req,
    locals: options.staff === false ? {} : { staff: principal },
    routeType: 'page',
    partial: false,
  });
  // Scoped <style> adds data-astro-cid-* to every tag; it is styling, not content.
  return { status: response.status, html: (await response.text()).replace(/ data-astro-cid-\w+/g, '') };
}

/** Catalog stub: venues for reads, and a recorder for writes. */
function stubCatalog(write: (url: string, body: unknown) => Response = () => json({}, 500)) {
  const writes: { url: string; body: unknown }[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if ((init?.method ?? 'GET') === 'POST') {
      const body = JSON.parse(String(init?.body ?? 'null'));
      writes.push({ url, body });
      return write(url, body);
    }
    if (url.includes('/public/venues')) return json(venues());
    throw new Error(`unexpected request: ${url}`);
  }));
  return writes;
}

const attr = (html: string, name: string) =>
  [...html.matchAll(new RegExp(`<input[^>]*\\s${name}="([^"]*)"`, 'g'))].map((m) => m[1]);
const alertText = (html: string) => html.match(/role="alert"[^>]*>([\s\S]*?)<\/p>/)?.[1] ?? '';

beforeEach(() => {
  vi.resetModules();
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'catalog-test-token';
  process.env.COMMERCE_STAFF_WRITE_TOKEN = 'commerce-test-token';
  process.env.INVENTORY_STAFF_WRITE_TOKEN = 'inventory-test-token';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
});

describe('the shell (Layout + venue list)', () => {
  it('renders in French from the cookie, with a switch back to English', async () => {
    stubCatalog();
    const { html } = await render('../src/pages/index.astro', request('/admin/', 'fr'));

    expect(html).toContain('<html lang="fr"');
    expect(html).toMatch(/<header[^>]*lang="fr"/);
    expect(html).toContain('<caption>Salles</caption>');
    expect(html).toContain('<th scope="col">Jauge en placement libre</th>');
    expect(html).toContain('Se déconnecter');
    // The role-filtered nav links are built from import.meta.env.BASE_URL, which
    // is "/" under the container, so the matrix hides them here. The browser spec
    // checks them in French on the real stack.
    // French grouping for the capacity, not English digits.
    expect(html).toContain('1\u202f200');
    // One button, naming the OTHER language in that language.
    expect(html).toMatch(/<form[^>]*method="post"[^>]*action="\/admin\/ui-language"/);
    expect(html).toMatch(/<button[^>]*name="ui_language"[^>]*value="en"[^>]*lang="en"[^>]*>English<\/button>/);
    expect(html).toMatch(/name="return_to" value="\/admin\/"/);
  }, 30_000);

  it('renders in English without a cookie, with a switch to French', async () => {
    stubCatalog();
    const { html } = await render('../src/pages/index.astro', request('/admin/', null));

    expect(html).toContain('<html lang="en"');
    expect(html).toContain('<caption>Venues</caption>');
    expect(html).toContain('Sign out');
    expect(html).toContain('1,200');
    expect(html).toMatch(/<button[^>]*name="ui_language"[^>]*value="fr"[^>]*lang="fr"[^>]*>Français<\/button>/);
  }, 30_000);

  it('renders in English for an unknown cookie value', async () => {
    stubCatalog();
    const req = new Request('http://backoffice.test/admin/', { headers: { cookie: 'bo_ui_language=FR' } });
    const { html } = await render('../src/pages/index.astro', req);
    expect(html).toContain('<html lang="en"');
    expect(html).toContain('<caption>Venues</caption>');
  }, 30_000);

  it('keeps the login page English and offers no switch to an anonymous caller', async () => {
    const { html } = await render('../src/pages/login.astro', request('/admin/login', 'fr'), { staff: false });
    expect(html).toContain('<html lang="en"');
    expect(html).toContain('Sign in');
    expect(html).not.toContain('ui-language');
    expect(html).not.toContain('name="ui_language"');
  }, 30_000);

  it('keeps an English page body under lang="en" while the header follows the UI language', async () => {
    // The order console is out of this story's scope and stays English. A French
    // document language over English text is an accessibility defect (D1(c)).
    const { html } = await render('../src/pages/orders.astro', request('/admin/orders', 'fr'));
    expect(html).toContain('<html lang="en"');
    expect(html).toMatch(/<header[^>]*lang="fr"/);
    expect(html).toMatch(/value="en"[^>]*lang="en"[^>]*>English<\/button>/);
  }, 30_000);
});

describe('the event builder in French', () => {
  it('renders step 1 in French and returns to the builder on a switch', async () => {
    stubCatalog();
    const { html } = await render('../src/pages/events/new.astro', request('/admin/events/new', 'fr'));

    expect(html).toContain('<html lang="fr"');
    expect(html).toContain('<title>Créer un événement — Back-office</title>');
    expect(html).toContain('<h1>Créer un événement</h1>');
    expect(html).toContain('Étape 1 sur 4');
    expect(html).toContain('<h2>1. L’événement</h2>');
    expect(html).toContain('<label for="name_en">Nom (anglais)</label>');
    expect(html).toContain('<label for="name_fr">Nom (français)</label>');
    expect(html).toContain('>Créer l’événement</button>');
    expect(html).toMatch(/name="return_to" value="\/admin\/events\/new"/);
  }, 30_000);

  it('labels the French content field in English for an English operator', async () => {
    stubCatalog();
    const { html } = await render('../src/pages/events/new.astro', request('/admin/events/new', 'en'));
    expect(html).toContain('<label for="name_fr">Name (French)</label>');
    expect(html).toContain('<label for="name_en">Name (English)</label>');
  }, 30_000);

  it('renders step 2 in French and keeps the progress in return_to', async () => {
    stubCatalog();
    const path = `/admin/events/new?event=${EVENT}`;
    const { html } = await render('../src/pages/events/new.astro', request(path, 'fr'));

    expect(html).toContain('Étape 2 sur 4');
    expect(html).toContain('<h2>2. Date et lieu</h2>');
    expect(html).toContain('<label for="venue_id">Salle</label>');
    expect(html).toContain('<label for="starts_at">Début</label>');
    expect(html).toContain('<label for="timezone">Fuseau horaire (IANA)</label>');
    expect(html).toContain('>Ajouter la date</button>');
    expect(html).toContain(`name="return_to" value="/admin/events/new?event=${EVENT}"`);
  }, 30_000);

  it('renders step 3 in French', async () => {
    stubCatalog();
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}`;
    const { html } = await render('../src/pages/events/new.astro', request(path, 'fr'));

    expect(html).toContain('Étape 3 sur 4');
    expect(html).toContain('<h2>3. Le prix</h2>');
    expect(html).toContain('<label for="tt_name_en">Nom du billet (anglais)</label>');
    expect(html).toContain('<label for="tt_name_fr">Nom du billet (français)</label>');
    expect(html).toContain('<label for="amount">Montant en unités mineures</label>');
    expect(html).toContain('placeholder="4550 (= 45,50\u00a0€)"');
    expect(html).toContain('<label for="currency">Devise</label>');
    expect(html).toContain('>Définir le prix</button>');
  }, 30_000);

  it('renders step 4 and the publication result in French', async () => {
    stubCatalog(() => json({
      id: PERFORMANCE,
      organizer_id: ORGANIZER,
      event_id: EVENT,
      venue_id: VENUE,
      kind: 'performance',
      starts_at: '2026-09-18T17:30:00Z',
      timezone: 'Europe/Paris',
      status: 'published',
      re_entry: { mode: 'single', requires_exit: false },
      closure: { status: 'open' },
      created_at: '2026-09-01T10:00:00Z',
    }));
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}&ticket_type=${TICKET_TYPE}`;
    const step4 = await render('../src/pages/events/new.astro', request(path, 'fr'));
    expect(step4.html).toContain('Étape 4 sur 4');
    expect(step4.html).toContain('<h2>4. Publication</h2>');
    expect(step4.html).toContain('La date devient alors disponible pour les acheteurs.');
    expect(step4.html).toContain('>Publier</button>');

    const done = await render(
      '../src/pages/events/new.astro',
      request(path, 'fr', { _action: 'publish-performance' }),
    );
    expect(done.html).toContain('<h2>Publication acceptée</h2>');
    expect(done.html).toMatch(/Le catalogue indique que cette date est à l’état «\u00a0<strong>publiée<\/strong>\u00a0»\./);
    expect(done.html).toContain('Retour aux salles');
  }, 30_000);

  it('shows a French validation state for a whitespace name, on the right field, keeping the key', async () => {
    const writes = stubCatalog();
    const key = '00000000-0000-4000-8000-000000000042';
    const { html } = await render(
      '../src/pages/events/new.astro',
      request('/admin/events/new', 'fr', {
        _action: 'create-event',
        idempotency_key: key,
        name_en: 'Night',
        name_fr: '   ',
      }),
    );

    expect(writes).toEqual([]);
    expect(alertText(html)).toBe('Le nom (FR) est obligatoire.');
    expect(html).toMatch(/id="name_fr"[^>]*aria-invalid="true"/);
    expect(html).not.toMatch(/id="name_en"[^>]*aria-invalid/);
    expect(html).toMatch(/name="name_en"[^>]*value="Night"/);
    expect(html).toContain(`name="idempotency_key" value="${key}"`);
  }, 30_000);

  it('says a malformed price in French', async () => {
    stubCatalog();
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}`;
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(path, 'fr', {
        _action: 'create-ticket-type',
        idempotency_key: 'k',
        tt_name_en: 'Standard',
        tt_name_fr: 'Standard',
        amount: '45.50',
        currency: 'EUR',
      }),
    );
    expect(alertText(html)).toBe(
      'Saisissez le montant en unités mineures\u00a0: chiffres uniquement, sans séparateur décimal (45,50\u00a0€ s’écrit 4550).',
    );
    expect(html).toMatch(/id="amount"[^>]*aria-invalid="true"/);
  }, 30_000);

  it('leads a catalog conflict in French and keeps catalog’s own reason, marked English', async () => {
    stubCatalog(() => json({ error: 'performance has no ticket type; create one before publishing' }, 409));
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}&ticket_type=${TICKET_TYPE}`;
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(path, 'fr', { _action: 'publish-performance' }),
    );
    expect(alertText(html)).toBe(
      'Le catalogue a refusé cette modification\u00a0: <span lang="en">performance has no ticket type; create one before publishing</span>',
    );
    expect(html).not.toContain('Publication acceptée');
  }, 30_000);

  it('leads any other catalog refusal as a refused step', async () => {
    stubCatalog(() => json({ error: 'unknown timezone "Mars/Olympus"' }, 400));
    const path = `/admin/events/new?event=${EVENT}`;
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(path, 'fr', {
        _action: 'create-performance',
        idempotency_key: 'k',
        venue_id: VENUE,
        starts_at: '2026-09-18T19:30:00+02:00',
        timezone: 'Mars/Olympus',
      }),
    );
    expect(alertText(html)).toBe(
      'Le catalogue a refusé cette étape\u00a0: <span lang="en">unknown timezone &quot;Mars/Olympus&quot;</span>',
    );
  }, 30_000);

  it('keeps catalog’s reason in English too, after an English lead', async () => {
    stubCatalog(() => json({ error: 'performance has no ticket type; create one before publishing' }, 409));
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}&ticket_type=${TICKET_TYPE}`;
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(path, null, { _action: 'publish-performance' }),
    );
    expect(alertText(html)).toBe(
      'Catalog refused this change: <span lang="en">performance has no ticket type; create one before publishing</span>',
    );
  }, 30_000);

  it('says an ambiguous save in French, keeping the key', async () => {
    stubCatalog(() => json({}, 201));
    const key = '00000000-0000-4000-8000-000000000043';
    const { html } = await render(
      '../src/pages/events/new.astro',
      request('/admin/events/new', 'fr', {
        _action: 'create-event',
        idempotency_key: key,
        name_en: 'Night',
        name_fr: 'Nuit',
      }),
    );
    expect(alertText(html)).toBe(
      'Le catalogue a peut-être enregistré cette étape, mais sa réponse n’a pas pu être vérifiée. ' +
        'Rechargez la page et vérifiez l’état actuel de l’événement avant de réessayer. ' +
        'Ce formulaire conserve la même clé d’idempotence.',
    );
    expect(html).toContain(`name="idempotency_key" value="${key}"`);
  }, 30_000);

  it('says an ambiguous publish in French, without claiming an idempotency key', async () => {
    stubCatalog(() => json({}, 200));
    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}&ticket_type=${TICKET_TYPE}`;
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(path, 'fr', { _action: 'publish-performance' }),
    );
    expect(alertText(html)).toBe(
      'Le catalogue a peut-être publié cette date, mais sa réponse n’a pas pu être vérifiée. ' +
        'Rechargez la page et vérifiez l’état actuel de l’événement avant de réessayer.',
    );
  }, 30_000);

  it('says the venue list is unavailable in French', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'down' }, 503)));
    const { html } = await render(
      '../src/pages/events/new.astro',
      request(`/admin/events/new?event=${EVENT}`, 'fr'),
    );
    expect(alertText(html)).toBe('Une erreur est survenue lors de l’échange avec le catalogue. Réessayez.');
    expect(html).toContain(
      'La liste des salles est indisponible\u00a0: vous ne pouvez pas terminer cette étape pour le moment.',
    );
  }, 30_000);
});

/**
 * TKT-368 A5. The UI language must never decide which content field is
 * submitted. Both content locales are required whatever the operator reads, and
 * the field names, ids and the catalog payload are the same in both languages.
 */
describe('content locale keys do not depend on the UI language', () => {
  const steps = [
    ['step 1', '/admin/events/new'],
    ['step 3', `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}`],
  ] as const;

  it.each(steps)('renders the same field names and ids in English and French on %s', async (_label, path) => {
    stubCatalog();
    const en = (await render('../src/pages/events/new.astro', request(path, 'en'))).html;
    stubCatalog();
    const fr = (await render('../src/pages/events/new.astro', request(path, 'fr'))).html;

    const fields = (html: string) => ({
      names: attr(html, 'name').sort(),
      ids: attr(html, 'id').sort(),
    });
    expect(fields(fr)).toEqual(fields(en));
    // ...and the set is the content-locale pair, not something derived from the UI.
    const names = fields(fr).names;
    if (path.includes('performance')) {
      expect(names).toEqual(expect.arrayContaining(['tt_name_en', 'tt_name_fr']));
    } else {
      expect(names).toEqual(expect.arrayContaining(['name_en', 'name_fr']));
    }
  }, 30_000);

  it('sends catalog exactly both content locales from a form rendered in French', async () => {
    const writes = stubCatalog(() => json({
      id: EVENT,
      organizer_id: ORGANIZER,
      name: { en: 'Night', fr: 'Nuit' },
      created_at: '2026-09-01T10:00:00Z',
    }, 201));
    // The fields the French page rendered, submitted as the browser would.
    const page = (await render('../src/pages/events/new.astro', request('/admin/events/new', 'fr'))).html;
    const key = page.match(/name="idempotency_key" value="([^"]+)"/)?.[1] ?? '';
    const [en, fr] = ['name_en', 'name_fr'].map((n) => {
      expect(attr(page, 'name'), n).toContain(n);
      return n;
    });
    const { status } = await render(
      '../src/pages/events/new.astro',
      request('/admin/events/new', 'fr', {
        _action: 'create-event',
        idempotency_key: key,
        [en!]: 'Night',
        [fr!]: 'Nuit',
      }),
    );

    expect(status).toBe(303);
    expect(writes).toHaveLength(1);
    const [write] = writes;
    expect(write?.url).toContain('/events');
    expect(write?.body).toMatchObject({ name: { en: 'Night', fr: 'Nuit' } });
    expect(Object.keys((write?.body ?? {}) as object)).toContain('name');
    expect(Object.keys(((write?.body as { name?: object } | undefined)?.name ?? {}) as object).sort()).toEqual(['en', 'fr']);
  }, 30_000);
});
