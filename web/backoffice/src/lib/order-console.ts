// The order console's orchestration (TKT-193): validate what the agent typed,
// fire the reads it can, and decide what the page is allowed to say.
//
// Kept out of the .astro page so the failure matrix is unit-testable — an Astro
// page can only be exercised through a running server, and the four-cell matrix
// is exactly the part that must be proven without one.
import { getOrderTickets, type SafeTicket } from './access';
import { getOrderState, type OrderState } from './commerce';
import type { Read } from './upstream';

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

/** What the refund form is allowed to contribute (TKT-194). */
export type RefundInput = {
  orderId: string;
  quantity: number;
  reason: string;
  idempotencyKey: string;
};

/**
 * Parse the refund form.
 *
 * Note what is NOT here: `actor`, `organizer_id`, `amount`, `currency`. The
 * first two come from the session and the last two are commerce's to derive
 * from the stored order price — a page that accepted an amount from the browser
 * would let the browser choose how much money moves.
 *
 * `actor` matters most. It is the attribution for an operation that moves money,
 * and reading it from the form would make every refund attributable to whatever
 * the client typed. That is not a hardening detail: box office can refund
 * (owner decision), so more people can do this than can approve it, and
 * attribution is the control that remains.
 */
export function parseRefund(
  field: (name: string) => string,
): { ok: true; value: RefundInput } | { ok: false; message: string } {
  const orderId = field('order_id').trim();
  if (!UUID.test(orderId)) {
    return { ok: false, message: 'That order id is not a valid identifier.' };
  }
  const raw = field('quantity').trim();
  const quantity = /^[0-9]{1,3}$/.test(raw) ? Number(raw) : NaN;
  // 1..50 is the contract's own bound (RefundCreate). Checked here so a typo is
  // refused before it becomes a request, and digits-only so "1.5" and "two"
  // cannot arrive as NaN and be compared into silence.
  if (!Number.isInteger(quantity) || quantity < 1 || quantity > 50) {
    return { ok: false, message: 'Quantity must be a whole number between 1 and 50.' };
  }
  const reason = field('reason').trim();
  if (reason === '' || reason.length > 500) {
    return { ok: false, message: 'Give a reason for the refund, up to 500 characters.' };
  }
  // Minted server-side when the form rendered. Its absence means this submission
  // cannot be made idempotent, and a refund that cannot be replayed safely is
  // one a double-click performs twice.
  const idempotencyKey = field('idempotency_key').trim();
  if (!UUID.test(idempotencyKey)) {
    return { ok: false, message: 'This form is stale — try the lookup again.' };
  }
  return { ok: true, value: { orderId, quantity, reason, idempotencyKey } };
}

/**
 * The refund still hanging over this order, or null if there is none.
 *
 * "Unresolved" means: commerce may have moved money and has not told us so. The
 * page must keep offering the SAME idempotency key until that is settled, and
 * must not offer a new refund in the meantime.
 *
 * Two things this is deliberately not derived from:
 *
 * - **Any reloaded order.** The outage that loses a refund response is the same
 *   one that takes the follow-up read, so a retry gated on that read disappears
 *   exactly when it is needed (ai-review pass 2).
 * - **The latest outcome alone.** A retry can be REFUSED and still leave the
 *   original unresolved. Commerce fingerprints the idempotency key over
 *   `(order, quantity, actor, reason)` (`store/refunds.go:204`), so the same
 *   retry submitted by a different staff member — a shift change, an escalation
 *   — is a 409. That is a decided answer about the RETRY and says nothing about
 *   whether the first attempt moved money. Reading it as "resolved" re-enabled
 *   an ordinary refund form, freshly keyed and prefilled with the same quantity:
 *   one click from refunding a second ticket for a customer already refunded
 *   (ai-review pass 3).
 *
 * `wasRetryOf` is the key the submitted form carried, which is how a request
 * says "I am the continuation of an unresolved refund".
 */
export function unresolvedRefund(
  outcome: { ok: boolean; kind?: string },
  request: RefundInput,
  wasRetryOf: string | null,
): RefundInput | null {
  // Commerce answered with a refund — for a replay, the ORIGINAL one. Settled.
  if (outcome.ok) return null;
  // Once a key is unresolved it stays THE key: whatever the latest submission
  // carried, the one still hanging over the order is the first one.
  const key = wasRetryOf ?? request.idempotencyKey;
  // Ambiguous may have moved money; a decided refusal to a RETRY settles the
  // retry and not the original. Either way it is still outstanding.
  if (outcome.kind === 'ambiguous' || wasRetryOf) {
    return { ...request, idempotencyKey: key };
  }
  // A decided refusal to a first attempt: nothing moved.
  return null;
}
