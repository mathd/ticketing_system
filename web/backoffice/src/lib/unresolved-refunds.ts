// Refunds commerce may have applied without telling us (TKT-194).
//
// When a refund request times out or answers 5xx, the money may have moved. The
// only safe next step is replaying the SAME idempotency key. This process keeps
// offering that key until commerce settles it, for at most 24 hours and while it
// remains within the 1,000-entry bound. During that window it survives a new
// lookup, reload, navigation, and shift change to another operator.
//
// Holding that in the page, or in a hidden form field, does neither: request
// state dies with the request, and a browser-supplied field is not evidence of
// anything. An operator who simply looks the order up again would be offered a
// freshly keyed refund form on an order commerce deliberately leaves
// `completed` with quantity still refundable — one click from refunding a
// second time, to a customer who may already have been refunded once.
//
// So the server holds it, keyed by organizer and order, which is the scope the
// risk actually has: it is the ORDER that has an outstanding refund, not the
// person who happened to submit it.
//
// **This module has a known expiry: TKT-201.** It exists only because there is
// no commerce read of an order's refunds — ADR-042 puts it plainly: *"the
// durable answer is a read, not a store."* Once TKT-201 lands that read, the
// page can ASK commerce what it applied instead of remembering what it sent, and
// this becomes an optimisation at best. Whoever lands it should delete this, not
// extend it.
//
// **Limits, stated rather than implied.** This is in-memory, like the session
// store it sits beside (TKT-190). It does not survive a restart and is not
// shared between replicas — the back office runs as a single container today,
// and a second one would need this in Postgres or Redis. A restart during an
// outage therefore still loses the key; that residual is named in ADR-042. What
// it is not is the far larger everyday window of "the operator looked the order
// up again", which this closes.
import type { RefundInput } from './order-console';

type Entry = { request: RefundInput; recordedAt: number };

/**
 * How long an unresolved refund keeps blocking new ones.
 *
 * Long, deliberately. The failure this guards is rare and expensive, and the
 * cost of holding it too long is that an operator must reconcile a refund by
 * hand — which is the correct outcome — while the cost of dropping it too early
 * is refunding a customer twice.
 */
export const UNRESOLVED_TTL_MS = 24 * 60 * 60 * 1000;

/**
 * A bound, so a pathological run of outages cannot grow this without limit.
 *
 * This and the TTL are defensive rather than based on measured demand. The
 * eviction direction matters: dropping the oldest keeps the newest ambiguous
 * request available for recovery.
 */
export const MAX_UNRESOLVED = 1000;

export interface UnresolvedRefundTracker {
  note(organizerId: string, request: RefundInput): void;
  find(organizerId: string, orderId: string): RefundInput | undefined;
  clear(organizerId: string, orderId: string, idempotencyKey: string): boolean;
}

/** Creates one isolated tracker. Production owns the instance exported below. */
export function createUnresolvedRefundTracker(
  clock: () => number = Date.now,
): UnresolvedRefundTracker {
  const outstanding = new Map<string, Entry>();
  const key = (organizerId: string, orderId: string) => `${organizerId} ${orderId}`;

  return {
    note(organizerId, request) {
      const k = key(organizerId, request.orderId);
      // Keep the first key. It is the request that may have moved money.
      if (outstanding.has(k)) return;
      if (outstanding.size >= MAX_UNRESOLVED) {
        // Dropping the oldest keeps the new ambiguous request recoverable.
        const oldest = outstanding.keys().next();
        if (!oldest.done) outstanding.delete(oldest.value);
      }
      outstanding.set(k, { request, recordedAt: clock() });
    },

    find(organizerId, orderId) {
      const k = key(organizerId, orderId);
      const entry = outstanding.get(k);
      if (!entry) return undefined;
      if (clock() - entry.recordedAt > UNRESOLVED_TTL_MS) {
        outstanding.delete(k);
        return undefined;
      }
      return entry.request;
    },

    clear(organizerId, orderId, idempotencyKey) {
      const k = key(organizerId, orderId);
      if (outstanding.get(k)?.request.idempotencyKey !== idempotencyKey) return false;
      return outstanding.delete(k);
    },
  };
}

/** Unresolved refunds held by this back-office process. */
export const unresolvedRefunds = createUnresolvedRefundTracker();
