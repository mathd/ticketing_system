import { UPSTREAM_DEADLINE_MS, withUpstreamDeadline } from './upstream';

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

export type TicketBundle = {
  tickets: Array<{
    ticket_id: string;
    issued_at: string;
    history: Array<{ type: string; occurred_at: string }>;
    qr_url: string;
  }>;
};

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
