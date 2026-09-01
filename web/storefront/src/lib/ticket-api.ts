import { UPSTREAM_DEADLINE_MS, withUpstreamDeadline } from './upstream';
import type { components } from './access-api-types.gen';

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

/**
 * The wire shape, from access's contract rather than restated here (TKT-303).
 *
 * The hand-written version this replaces was already drifting on four fields:
 * it omitted `order_ref` and `qr_payload` entirely, and typed `history` as
 * `{ type, occurred_at }` while the contract's LifecycleEvent also carries a
 * required `id` and an optional `sequence`. Nothing failed, because the body
 * is cast rather than validated —
 * which is exactly why a renamed field would have surfaced as a broken buyer
 * ticket page instead of a red gate. `check-generate` now covers this file.
 *
 * Still a cast, not validation. Generated types are a compile-time claim about
 * what access promises, not a runtime check that it delivered; that gap is
 * unchanged by this ticket and is not what it set out to close.
 *
 * What the generation actually buys, measured rather than assumed by renaming
 * `qr_url` in access's contract and regenerating:
 *
 *   - `check-generate` goes red. That is the gate COS3 asked for, and it fires
 *     whether or not anyone consumes the type.
 *   - A `.ts` consumer of this type fails to compile (TS2339).
 *   - A `.astro` TEMPLATE does NOT. `pnpm run build` is
 *     `astro sync && tsc --noEmit && astro build`, and `tsc` does not parse
 *     `.astro`; that needs `astro check`, which this repo does not run. So
 *     `[orderRef].astro`'s `ticket.qr_url` stayed green under the rename.
 *
 * The last point is a real limit of this change, not a detail: the buyer ticket
 * page is exactly the surface the ticket worried about. `check-generate` is what
 * closes the gap; the type adoption is what makes the next `.ts` reader honest.
 */
export type TicketBundle = components['schemas']['TicketBundle'];

export type TicketBundleResult =
  | { ok: true; value: TicketBundle }
  | { ok: false; status: number };

/**
 * Issuance is asynchronous, so a 404 here means "not yet", not "never". These
 * two numbers are the waiting budget: up to ISSUANCE_ATTEMPTS reads spaced
 * ISSUANCE_RETRY_MS apart, i.e. about three seconds of catching up.
 */
const ISSUANCE_ATTEMPTS = 12;
const ISSUANCE_RETRY_MS = 250;

/**
 * The whole read, including every retry and the waits between them.
 *
 * This bound must accommodate the retry budget it contains. Wrapping the loop
 * in a single UPSTREAM_DEADLINE_MS deadline looks equivalent and is not: that
 * deadline covers the FETCHES as well as the waits, so at a realistic 200ms per
 * attempt it fires on attempt 12 — the buyer whose issuance completed just in
 * time is handed a 503 instead of their tickets. Budget the waits, the reads,
 * and headroom for the one read that is still in flight when the waits run out.
 */
export const ISSUANCE_TOTAL_BUDGET_MS =
  ISSUANCE_ATTEMPTS * ISSUANCE_RETRY_MS + UPSTREAM_DEADLINE_MS;

/** Sleep that gives up early when the surrounding read has been abandoned. */
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    }
    signal.addEventListener('abort', done, { once: true });
  });
}

/** Read a ticket bundle, allowing issuance about three seconds to catch up. */
export async function getTicketBundle(orderRef: string): Promise<TicketBundleResult> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      let response: Response | undefined;
      for (let attempt = 0; attempt < ISSUANCE_ATTEMPTS; attempt += 1) {
        // A retry after the read was abandoned would be a request nobody is
        // waiting for; stop rather than spend the remaining attempts.
        if (signal.aborted) break;
        response = await fetch(
          `${GATEWAY_URL}/api/access/orders/${encodeURIComponent(orderRef)}/tickets`,
          { headers: { Accept: 'application/json' }, signal },
        );
        if (response.ok || response.status !== 404) break;
        await pause(ISSUANCE_RETRY_MS, signal);
      }
      if (!response?.ok) return { ok: false, status: response?.status ?? 503 };
      return { ok: true, value: (await response.json()) as TicketBundle };
    }, ISSUANCE_TOTAL_BUDGET_MS);
  } catch {
    return { ok: false, status: 503 };
  }
}
