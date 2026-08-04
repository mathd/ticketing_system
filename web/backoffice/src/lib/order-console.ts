// The order console's orchestration (TKT-193): validate what the agent typed,
// fire the reads it can, and decide what the page is allowed to say.
//
// Kept out of the .astro page so the failure matrix is unit-testable — an Astro
// page can only be exercised through a running server, and the four-cell matrix
// is exactly the part that must be proven without one.
import { getOrderState, getOrderTickets, type Read, type OrderState, type SafeTicket } from './api';

export type Lookup = { orderId?: string; ref?: string };

export type ConsoleView = {
  /** Absent = not asked for, which is different from asked-and-not-found. */
  order?: Read<OrderState>;
  tickets?: Read<SafeTicket[]>;
  /** What the page responds with. 404 only when everything asked came back absent. */
  status: 200 | 404 | 503;
};

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Both identifiers are optional; at least one is required.
 *
 * The two reads are keyed differently (ADR-012: `guest_order_ref` is a CSPRNG
 * UUIDv4, deliberately distinct from the order id) and nothing maps one to the
 * other, so the console cannot infer the second from the first. It asks for
 * whichever the agent has.
 */
export function parseLookup(
  rawOrderId: string,
  rawRef: string,
): { ok: true; value: Lookup } | { ok: false; message: string } {
  const orderId = rawOrderId.trim();
  const ref = rawRef.trim();
  if (!orderId && !ref) {
    return { ok: false, message: 'Enter an order id, a ticket reference, or both.' };
  }
  if (orderId && !UUID.test(orderId)) {
    return { ok: false, message: 'That order id is not a valid identifier.' };
  }
  if (ref && !UUID.test(ref)) {
    return { ok: false, message: 'That ticket reference is not a valid identifier.' };
  }
  return { ok: true, value: { ...(orderId && { orderId }), ...(ref && { ref }) } };
}

export async function loadOrderConsole(
  lookup: Lookup,
  reads = { order: getOrderState, tickets: getOrderTickets },
): Promise<ConsoleView> {
  const [order, tickets] = await Promise.all([
    lookup.orderId ? reads.order(lookup.orderId) : undefined,
    lookup.ref ? reads.tickets(lookup.ref) : undefined,
  ]);

  const attempted = [order, tickets].filter((r) => r !== undefined);
  // Anything answered → the page has something true to show, so 200 even beside
  // a half that failed. Otherwise an outage outranks an absence: a 404 asserts
  // the order does not exist, and a service that could not answer has not
  // established that.
  const status = attempted.some((r) => r.ok)
    ? 200
    : attempted.some((r) => !r.ok && r.kind === 'unavailable')
      ? 503
      : 404;

  return { order, tickets, status };
}
