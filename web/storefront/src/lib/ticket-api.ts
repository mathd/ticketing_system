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
 * What the generation buys, measured rather than assumed by renaming `qr_url`
 * in access's contract, regenerating and COMMITTING the output:
 *
 *   - `check-generate` PASSES. It compares the spec against the generated files
 *     and both moved together, so it detects generator drift, not consumer
 *     compatibility. An earlier version of this comment claimed it "closes the
 *     gap"; that was false and an ai-review finding caught it.
 *   - A `.ts` consumer that READS a renamed field fails to compile (TS2339).
 *   - A `.astro` template does not. `pnpm run build` is
 *     `astro sync && tsc --noEmit && astro build`, `tsc` does not parse
 *     `.astro`, and this repo does not install `@astrojs/check`.
 *
 * So a type alias nothing reads buys nothing: the cast below names the type but
 * touches no field, and the only field consumer was the unchecked template.
 * `ticketsForDisplay` exists to close that — it reads every field the page
 * renders, in checked TypeScript, so the rename that would ship an
 * `<img src={undefined}>` fails the build instead.
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

/**
 * The fields the ticket page renders, read HERE so the compiler sees them.
 *
 * This is the whole point of adopting the generated type: a cast alone is a
 * claim nobody checks. Every property access below is a compile-time assertion
 * that access's contract still carries that field under that name, and the page
 * consumes this projection instead of the wire object.
 *
 * `qrUrl` is the one that motivated it — rename `qr_url` in the contract,
 * regenerate, and this file stops compiling. Before, the page rendered
 * `<img src={undefined}>` and every gate stayed green.
 */
export type TicketForDisplay = {
  qrUrl: string;
  history: ReadonlyArray<{ type: string; occurredAt: string }>;
};

export function ticketsForDisplay(bundle: TicketBundle): TicketForDisplay[] {
  return bundle.tickets.map((ticket) => ({
    qrUrl: ticket.qr_url,
    history: ticket.history.map((event) => ({
      type: event.type,
      occurredAt: event.occurred_at,
    })),
  }));
}
