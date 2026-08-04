import { readdirSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  ROUTE_MATRIX,
  STAFF_ROLES,
  canAccessRoute,
  classifyRoute,
  isRecognisedRole,
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
    out.push(base === 'index' ? `/admin${prefix}` || '/admin' : `/admin${prefix}/${base}`);
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
  });
});

describe('what the matrix says (COS-2, COS-3)', () => {
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
