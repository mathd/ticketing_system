import { readdirSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  ROUTE_MATRIX,
  STAFF_ROLES,
  canAccessRoute,
  classifyRoute,
  isAnonymousRoute,
  isRecognisedRole,
  isSupportedTemplate,
  selectRule,
  type RouteRule,
} from '../src/lib/authorization';

/**
 * TKT-197 COS-1 — the fail-closed enumeration, and the load-bearing test of this
 * ticket.
 *
 * A test that lists today's five routes proves nothing about the sixth, which is
 * the one that will actually ship unclassified. So this derives the route set
 * from the filesystem — Astro uses file-based routing, so `src/pages/**` IS the
 * route table — and compares it against the matrix **in both directions**:
 *
 *   a page with no matrix row      -> a route nobody classified, reachable
 *   a matrix row with no page      -> the matrix has stopped describing reality
 *
 * The second direction matters as much as the first. A one-way check lets rows
 * accumulate for routes that no longer exist, and a stale allow-row is how an
 * exemption list quietly grows. (This ticket's own brief asserted logout was
 * anonymous when it is not — a hand-maintained list is exactly that mistake,
 * committed.)
 */
const PAGES_DIR = new URL('../src/pages/', import.meta.url).pathname;

/** Extensions Astro treats as routes in this app. */
const ROUTE_EXTENSIONS = ['.astro', '.ts'];

function derivedRouteTemplates(dir = PAGES_DIR, prefix = ''): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory()) {
      out.push(...derivedRouteTemplates(join(dir, entry.name), `${prefix}/${entry.name}`));
      continue;
    }
    const ext = ROUTE_EXTENSIONS.find((e) => entry.name.endsWith(e));
    if (!ext) {
      // Not a route. Failing loudly rather than ignoring it: a file shape this
      // enumerator does not understand is a route it cannot classify.
      throw new Error(
        `unrecognised file under src/pages: ${prefix}/${entry.name}. ` +
          `Either it is a route (extend ROUTE_EXTENSIONS) or it does not belong here.`,
      );
    }
    const base = entry.name.slice(0, -ext.length);
    out.push(base === 'index' ? `/admin${prefix}` : `/admin${prefix}/${base}`);
  }
  return out;
}

describe('route enumeration is fail-closed (COS-1)', () => {
  it('classifies every page, and every classification names a page', () => {
    const onDisk = new Set(derivedRouteTemplates());
    const declared = new Set(
      ROUTE_MATRIX.filter((r) => r.source === 'page').map((r) => r.template),
    );

    const unclassified = [...onDisk].filter((t) => !declared.has(t)).sort();
    const stale = [...declared].filter((t) => !onDisk.has(t)).sort();

    expect(
      unclassified,
      'these routes exist and no rule covers them — anyone who guesses the URL reaches them',
    ).toEqual([]);
    expect(
      stale,
      'these rules name routes that no longer exist — the matrix has stopped describing reality',
    ).toEqual([]);
  });

  it('sees the routes we know are there, so the walk is not vacuously empty', () => {
    const onDisk = derivedRouteTemplates();
    expect(onDisk).toContain('/admin');
    expect(onDisk).toContain('/admin/login');
    expect(onDisk).toContain('/admin/logout');
    expect(onDisk).toContain('/admin/healthz');
    expect(onDisk).toContain('/admin/venues/[id]');
    expect(onDisk).toContain('/admin/events/new');
    expect(onDisk).toContain('/admin/orders');
  });
});

describe('what the matrix says (COS-2, COS-3)', () => {
  it('opens the order console to admin and box office, not finance (TKT-193)', () => {
    // Support work, not settlement work: the console shows ticket identities and
    // lifecycle history and carries no money at all, so finance has nothing to do
    // here and the least-privilege answer is no.
    expect(canAccessRoute('/admin/orders', 'admin')).toBe(true);
    expect(canAccessRoute('/admin/orders', 'box_office')).toBe(true);
    expect(canAccessRoute('/admin/orders', 'finance')).toBe(false);
  });

  it('gates the event builder to admin alone (TKT-192)', () => {
    expect(canAccessRoute('/admin/events/new', 'admin')).toBe(true);
    expect(canAccessRoute('/admin/events/new', 'finance')).toBe(false);
    expect(canAccessRoute('/admin/events/new', 'box_office')).toBe(false);
  });

  it('gates the catalog authoring surface to admin alone', () => {
    // COS-3: the matrix must be able to EXPRESS a role-exclusive route, and this
    // is the proof, using a route that exists. The finance/settlement surface it
    // would ideally be demonstrated on does not exist yet — TKT-23 owns it, and
    // must add its page here as admin+finance rather than inventing one now.
    expect(canAccessRoute('/admin/venues/abc', 'admin')).toBe(true);
    expect(canAccessRoute('/admin/venues/abc', 'finance')).toBe(false);
    expect(canAccessRoute('/admin/venues/abc', 'box_office')).toBe(false);
  });

  it('lets every role see the venue list', () => {
    for (const role of STAFF_ROLES) {
      expect(canAccessRoute('/admin/', role)).toBe(true);
    }
  });

  it('lets every role sign out', () => {
    // Signing out must never be role-gated: a staff member who cannot reach a
    // page must still be able to end their session.
    for (const role of STAFF_ROLES) {
      expect(canAccessRoute('/admin/logout', role)).toBe(true);
    }
  });

  // The failure this ticket is named for. box_office reaches almost nothing
  // today and that is CORRECT — the order console it exists for is TKT-193/194.
  // Widening this classification to "give box office something to do" is the
  // mistake; the fix is to build those surfaces.
  it('does not give box_office the authoring surface for want of anything else', () => {
    expect(canAccessRoute('/admin/venues/abc', 'box_office')).toBe(false);
  });
});

describe('unclassified and unknown fail closed (COS-6)', () => {
  it('refuses a path no rule covers, rather than allowing it', () => {
    expect(classifyRoute('/admin/not-a-real-page')).toBeUndefined();
    for (const role of STAFF_ROLES) {
      expect(canAccessRoute('/admin/not-a-real-page', role)).toBe(false);
    }
  });

  it('refuses a role outside the vocabulary', () => {
    expect(isRecognisedRole('superuser')).toBe(false);
    expect(isRecognisedRole('')).toBe(false);
    expect(canAccessRoute('/admin/', 'superuser' as never)).toBe(false);
  });

  it('recognises exactly the contract vocabulary', () => {
    expect([...STAFF_ROLES].sort()).toEqual(['admin', 'box_office', 'finance']);
    for (const role of STAFF_ROLES) expect(isRecognisedRole(role)).toBe(true);
  });
});

describe('template matching', () => {
  it('matches a dynamic segment, and only one segment', () => {
    expect(classifyRoute('/admin/venues/abc')?.template).toBe('/admin/venues/[id]');
    // Two segments is a different route, and an unclassified one.
    expect(classifyRoute('/admin/venues/abc/extra')).toBeUndefined();
  });

  it('treats a trailing slash as the same route', () => {
    expect(classifyRoute('/admin')?.template).toBe('/admin');
    expect(classifyRoute('/admin/')?.template).toBe('/admin');
    expect(classifyRoute('/admin/venues/abc/')?.template).toBe('/admin/venues/[id]');
  });

  it('does not let a lookalike ride a classification', () => {
    expect(classifyRoute('/admin/venuess/abc')).toBeUndefined();
    expect(classifyRoute('/admin/venues')).toBeUndefined();
  });
});


describe('overlapping templates resolve by specificity, not declaration order', () => {
  // ai-review F1, and the sharpest finding on this ticket.
  //
  // First-match classification was a role-boundary bug waiting for a routine
  // route addition. `/admin/venues/[id]` is admin-only; add a static,
  // finance-only `/admin/venues/settlement` page and first-match hands it the
  // `[id]` rule — admin reaches the finance page and finance is refused, exactly
  // inverted. Astro resolves static before dynamic, so a matcher that disagrees
  // with the router about which route a URL IS cannot be trusted to say who may
  // reach it.
  //
  // The enumeration test would NOT have caught this: both templates exist in
  // both sets, so it passes while the roles are wrong. Hence a direct test.
  //
  // Driven through `selectRule` with a synthetic matrix rather than by adding a
  // real page — the property is about rule selection, and inventing a settlement
  // page to test against would be the "test that proves the test" this ticket
  // already refused once.
  const overlapping: readonly RouteRule[] = [
    { template: '/admin/venues/[id]', source: 'page', roles: ['admin'] },
    { template: '/admin/venues/settlement', source: 'page', roles: ['finance'] },
    { template: '/admin/[...rest]', source: 'page', roles: ['admin'] },
  ];

  it('prefers a static segment over a dynamic one, whatever the order', () => {
    expect(selectRule(overlapping, '/admin/venues/settlement')?.roles).toEqual(['finance']);
    // ...and the reversed declaration order must give the same answer.
    expect(selectRule([...overlapping].reverse(), '/admin/venues/settlement')?.roles).toEqual([
      'finance',
    ]);
  });

  it('still routes a genuine dynamic value to the dynamic rule', () => {
    expect(selectRule(overlapping, '/admin/venues/abc-123')?.roles).toEqual(['admin']);
  });

  it('prefers any exact-arity rule over a rest rule', () => {
    // A rest template matches almost everything; if it won, one catch-all row
    // would silently take over the whole surface.
    expect(selectRule(overlapping, '/admin/venues/abc')?.template).toBe('/admin/venues/[id]');
  });

  it('gives every shipped rule a path that actually selects it', () => {
    // ai-review pass 2, F2: the previous version of this guard probed five
    // hand-written paths and called selectRule([r], path) — a single-element
    // array, so it could only ever return that element. It was a verbose match
    // predicate wearing the costume of an ambiguity check, and it is why the
    // incomplete comparator above passed.
    //
    // The property that matters is REACHABILITY: a rule no path can select is a
    // rule that has been shadowed, which is exactly what a role-inverting
    // overlap looks like. Derived from the matrix, so it covers rules added
    // later without anyone extending a probe list.
    for (const rule of ROUTE_MATRIX) {
      const path = rule.template.replace(/\[\.\.\.[^\]]+\]/g, 'a/b').replace(/\[[^\]]+\]/g, 'x');
      expect(selectRule(ROUTE_MATRIX, path)?.template, `${rule.template} is shadowed by another rule`).toBe(
        rule.template,
      );
    }
  });

  it('uses only segment shapes the matcher models', () => {
    // ai-review pass 3. The previous version of this guard asked whether a `[`
    // was at index 0 — which rejects `order-[id]` and waves `[id]-edit` straight
    // through, because its bracket IS at index 0. Astro allows both; this
    // matcher models neither, and would read `[id]-edit` as an ordinary dynamic
    // segment matching ANY single segment. Paired with a `[...rest]` sibling
    // that gives one rule's permissions to another rule's route.
    for (const rule of ROUTE_MATRIX) {
      expect(isSupportedTemplate(rule.template), `unsupported segment shape in ${rule.template}`).toBe(true);
    }
  });

  it('refuses mixed segments in both spellings', () => {
    expect(isSupportedTemplate('/admin/orders/order-[id]')).toBe(false);
    expect(isSupportedTemplate('/admin/orders/[id]-edit')).toBe(false);
    expect(isSupportedTemplate('/admin/[a][b]')).toBe(false);
    expect(isSupportedTemplate('/admin/[...a]-x')).toBe(false);
    // ...while accepting the shapes it does model.
    expect(isSupportedTemplate('/admin/venues/[id]')).toBe(true);
    expect(isSupportedTemplate('/admin/_astro/[...asset]')).toBe(true);
    expect(isSupportedTemplate('/admin/logout')).toBe(true);
  });

  // The positional case my first comparator got wrong: same dynamic count, so it
  // tied and fell back to declaration order. Astro compares segment by segment
  // and prefers the static one at the first differing position.
  it('prefers the template whose EARLIER segment is static', () => {
    const positional: readonly RouteRule[] = [
      { template: '/admin/[section]/x', source: 'page', roles: ['admin'] },
      { template: '/admin/x/[id]', source: 'page', roles: ['finance'] },
    ];
    for (const order of [positional, [...positional].reverse()]) {
      expect(selectRule(order, '/admin/x/x')?.roles).toEqual(['finance']);
    }
  });

  it('prefers a deeper rest template over a shallower one', () => {
    const rests: readonly RouteRule[] = [
      { template: '/admin/[...rest]', source: 'page', roles: ['admin'] },
      { template: '/admin/reports/[...rest]', source: 'page', roles: ['finance'] },
    ];
    for (const order of [rests, [...rests].reverse()]) {
      expect(selectRule(order, '/admin/reports/q3/summary')?.roles).toEqual(['finance']);
    }
  });

});

describe('anonymous access has ONE source of truth', () => {
  // ai-review F2. The gate used to consult a hand-written predicate while the
  // matrix carried `anonymous` rows, so there were two declarations of the
  // unauthenticated attack surface — and they had already drifted on bare
  // `/admin/_astro`. This ticket's own brief got that list wrong once, which is
  // the argument for there being exactly one.
  it('agrees with the matrix on every anonymous rule', () => {
    for (const rule of ROUTE_MATRIX.filter((r) => r.anonymous)) {
      const concrete = rule.template.replace('[...asset]', 'index.abc123.css');
      expect(isAnonymousRoute(concrete), concrete).toBe(true);
    }
  });

  it('treats every authenticated rule as NOT anonymous', () => {
    for (const rule of ROUTE_MATRIX.filter((r) => !r.anonymous)) {
      const concrete = rule.template.replace('[id]', 'x');
      expect(isAnonymousRoute(concrete), concrete).toBe(false);
    }
  });

  it('does not make an unclassified path anonymous', () => {
    // Fail-closed: "no rule" must not read as "no session needed".
    expect(isAnonymousRoute('/admin/nope')).toBe(false);
    expect(isAnonymousRoute('/admin/healthz/../venues/x')).toBe(false);
  });
});
