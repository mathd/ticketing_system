// The route→role matrix (TKT-197 / US-B2b).
//
// One declared table, read by BOTH the gate (`gate.ts`) and the navigation
// (`index.astro`). Two readers, one source — because the failure this ticket is
// about is a link being hidden while the URL still works, and that can only
// happen if the nav and the gate disagree.
//
// Why the matrix lives in the back office and not in catalog: since TKT-191 the
// browser holds no catalog credential, so a staff member cannot reach a catalog
// write except through this app. That makes this the only place a role can gate
// one, and it is why "signed role claims verified in catalog" was considered and
// rejected there.

import type { components } from './api-types.gen';

/**
 * The vocabulary, taken from the generated contract rather than re-typed.
 * `StaffRole` in `services/catalog/api/openapi.yaml` is the single source; if a
 * role is added there and not handled here, `ROUTE_MATRIX`'s exhaustive typing
 * stops compiling. That is deliberate: a new role must not default to anything.
 */
export type StaffRole = components['schemas']['StaffRole'];

export const STAFF_ROLES = ['admin', 'box_office', 'finance'] as const satisfies readonly StaffRole[];

export function isRecognisedRole(role: string): role is StaffRole {
  return (STAFF_ROLES as readonly string[]).includes(role);
}

export interface RouteRule {
  /** Astro route template, e.g. `/admin/venues/[id]`. */
  template: string;
  /**
   * `page` rows correspond 1:1 with files under `src/pages/` and are what the
   * enumeration test compares against. `asset` rows cover build output that is
   * served but is not a page file.
   */
  source: 'page' | 'asset';
  /** Roles allowed. `anonymous` routes list none and are reachable without a session. */
  anonymous?: true;
  roles?: readonly StaffRole[];
}

/**
 * Every route this app serves. The enumeration test (`authorization.test.ts`)
 * compares this against the real `src/pages/` tree in BOTH directions, so a page
 * without a rule and a rule without a page each fail the build.
 */
export const ROUTE_MATRIX: readonly RouteRule[] = [
  // Anonymous — this is the entire unauthenticated attack surface.
  { template: '/admin/login', source: 'page', anonymous: true },
  // Compose probes healthz DIRECTLY on the container, before the gateway.
  // Gating it makes the container unhealthy, the gateway's depends_on never
  // satisfies, and the whole stack fails to start (TKT-190).
  { template: '/admin/healthz', source: 'page', anonymous: true },
  { template: '/admin/_astro/[...asset]', source: 'asset', anonymous: true },

  // Authenticated.
  { template: '/admin', source: 'page', roles: STAFF_ROLES },
  // Never role-gated: a staff member who cannot reach a page must still be able
  // to end their session.
  { template: '/admin/logout', source: 'page', roles: STAFF_ROLES },

  // The catalog authoring surface — admin only.
  //
  // box_office and finance reach almost nothing as a result, and that is
  // CORRECT rather than an oversight: the surfaces they exist for are the order
  // console (TKT-193/TKT-194) and settlement (TKT-23), none of which are built.
  // Widening this row to give them something to do is the mistake; building
  // those surfaces is the fix. When TKT-23 lands, its page joins this table as
  // admin + finance.
  { template: '/admin/venues/[id]', source: 'page', roles: ['admin'] },
];

/** Trailing slashes are cosmetic; `/admin/` and `/admin` are one route. */
function normalize(pathname: string): string {
  if (pathname.length > 1 && pathname.endsWith('/')) return pathname.slice(0, -1);
  return pathname;
}

function matches(template: string, pathname: string): boolean {
  const t = normalize(template).split('/');
  const p = normalize(pathname).split('/');
  for (let i = 0; i < t.length; i++) {
    const seg = t[i]!;
    if (seg.startsWith('[...')) return p.length >= i; // rest segment: matches the remainder
    if (i >= p.length) return false;
    if (seg.startsWith('[')) continue; // dynamic: exactly ONE segment
    if (seg !== p[i]) return false;
  }
  return t.length === p.length;
}

/**
 * Segment kinds, ordered by precedence. Lower wins.
 *
 * Whole-segment only: this app has no mixed segments (`order-[id]`), and a test
 * asserts the matrix never grows one — the comparator below would classify such
 * a segment as static and disagree with Astro about it.
 */
const enum SegmentKind {
  Static = 0,
  Dynamic = 1,
  Spread = 2,
}

/**
 * Segments this matcher models: fully static, a whole `[param]`, or a whole
 * `[...rest]`. Nothing else.
 *
 * Astro also allows MIXED segments — `order-[id]`, `[id]-edit` — and this
 * matcher would get them wrong in both directions: `segmentKind` reads
 * `[id]-edit` as a plain dynamic segment (it starts with `[`), and `matches`
 * then admits any single segment where Astro would require the `-edit` suffix.
 * Pair that with a `[...rest]` sibling and the mirrored priority hands the
 * supposed-dynamic rule a path Astro would route to the spread — one rule's
 * permissions applied to another rule's route.
 *
 * So they are refused rather than mis-ranked. An earlier version of this guard
 * lived only in the test and checked that a `[` was at index 0, which rejected
 * `order-[id]` and waved `[id]-edit` straight through (ai-review pass 3).
 */
export function isSupportedTemplate(template: string): boolean {
  return normalize(template)
    .split('/')
    .every((seg) => !seg.includes('[') && !seg.includes(']')
      ? true
      : /^\[[^[\]./]+\]$/.test(seg) || /^\[\.\.\.[^[\]./]+\]$/.test(seg));
}

function segmentKind(segment: string): SegmentKind {
  if (segment.startsWith('[...')) return SegmentKind.Spread;
  if (segment.startsWith('[')) return SegmentKind.Dynamic;
  return SegmentKind.Static;
}

/**
 * Astro's own route precedence, mirrored (ai-review pass 2, F1).
 *
 * Read from `astro/dist/core/routing/priority.js` (`routeComparator`) at the
 * installed 7.0.9 rather than described from memory, because the property that
 * matters is agreement with the router: a matcher that disagrees about which
 * route a URL IS cannot be trusted to say who may reach it.
 *
 * Segment by segment: static beats dynamic beats spread, and two differing
 * statics order alphabetically. Then longer beats shorter, except that a route
 * ending in a spread loses to its one-segment-shorter sibling — Astro's own
 * carve-out, kept so the two do not diverge on it.
 *
 * A first attempt scored whole templates as (hasSpread, dynamicCount) and tied
 * on `/admin/[a]/x` vs `/admin/x/[b]`, falling back to declaration order — which
 * is how a routine route addition inverts a role boundary.
 */
export function compareRoutePriority(a: string, b: string): number {
  const as = normalize(a).split('/');
  const bs = normalize(b).split('/');
  const common = Math.min(as.length, bs.length);

  for (let i = 0; i < common; i++) {
    const ak = segmentKind(as[i]!);
    const bk = segmentKind(bs[i]!);
    if (ak !== bk) return ak - bk;
    if (ak === SegmentKind.Static && as[i] !== bs[i]) return as[i]!.localeCompare(bs[i]!);
  }

  if (as.length !== bs.length) {
    const aEndsInRest = segmentKind(as.at(-1)!) === SegmentKind.Spread;
    const bEndsInRest = segmentKind(bs.at(-1)!) === SegmentKind.Spread;
    if (aEndsInRest !== bEndsInRest && Math.abs(as.length - bs.length) === 1) {
      if (as.length > bs.length && aEndsInRest) return 1;
      if (bs.length > as.length && bEndsInRest) return -1;
    }
    return as.length > bs.length ? -1 : 1;
  }
  return a.localeCompare(b);
}

/**
 * The rule covering a path, chosen by Astro's PRECEDENCE rather than by
 * declaration order (ai-review pass 1, F1).
 *
 * First-match was a role-boundary bug waiting for a routine route addition:
 * `/admin/venues/[id]` is admin-only, so a static finance-only
 * `/admin/venues/settlement` added later would have been handed the `[id]` rule —
 * admin reaching the finance page, finance refused, exactly inverted. The
 * enumeration test would not have noticed, because both templates exist in both
 * sets.
 */
export function selectRule(
  rules: readonly RouteRule[],
  pathname: string,
): RouteRule | undefined {
  const candidates = rules.filter((rule) => matches(rule.template, pathname));
  if (candidates.length <= 1) return candidates[0];
  return [...candidates].sort((x, y) => compareRoutePriority(x.template, y.template))[0];
}

/** The rule covering a path, or undefined when nothing covers it. */
export function classifyRoute(pathname: string): RouteRule | undefined {
  return selectRule(ROUTE_MATRIX, pathname);
}

/**
 * May this role reach this path?
 *
 * Fail-closed on every uncertainty: an unclassified path, an unrecognised role,
 * and an authenticated route with no role list all answer `false`. A route
 * nobody classified must not be a route everybody can reach.
 */
export function canAccessRoute(pathname: string, role: StaffRole): boolean {
  const rule = classifyRoute(pathname);
  if (!rule) return false;
  if (rule.anonymous) return true;
  if (!isRecognisedRole(role)) return false;
  return rule.roles?.includes(role) ?? false;
}

/**
 * Is this path reachable without a session?
 *
 * Derived from the matrix, so there is ONE declaration of the unauthenticated
 * attack surface (ai-review F2). TKT-190 answered this with a separate
 * hand-written predicate, and the two had already drifted: bare `/admin/_astro`
 * was anonymous to the matrix's rest rule and gated by the predicate, which
 * required the trailing slash. Two lists of what is public is one list too many,
 * and this ticket's own brief got that list wrong once already.
 */
export function isAnonymousRoute(pathname: string): boolean {
  return classifyRoute(pathname)?.anonymous === true;
}
