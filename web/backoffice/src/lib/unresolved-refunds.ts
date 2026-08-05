// Refunds commerce may have applied without telling us (TKT-194, ai-review pass 4).
//
// When a refund request times out or answers 5xx, the money may have moved. The
// only safe next step is replaying the SAME idempotency key, so the page must
// keep offering that key until commerce settles it — and it must keep offering
// it across a new lookup, a reload, a navigation, and a shift change to another
// operator.
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

const outstanding = new Map<string, Entry>();

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
 * Both this and the TTL were added defensively rather than from a measured
 * need, and both were re-examined in the TKT-22 refactor and kept. The bound
 * costs six lines in a process that runs for weeks; the test that pins it is
 * not testing that a number exists, it is testing the eviction DIRECTION —
 * dropping the oldest and keeping the newest — and getting that backwards would
 * silently discard the entry most likely to still be unsettled. That is the
 * half worth a test, and it is not self-evident from the code.
 */
export const MAX_UNRESOLVED = 1000;

const key = (organizerId: string, orderId: string) => `${organizerId} ${orderId}`;

export function resetUnresolvedForTest(): void {
  outstanding.clear();
}

export function noteUnresolvedRefund(
  organizerId: string,
  request: RefundInput,
  now = Date.now(),
): void {
  const k = key(organizerId, request.orderId);
  // Never overwrite an existing entry: the FIRST unresolved key is the one that
  // may have moved money, and a later attempt's key does not replace it.
  if (outstanding.has(k)) return;
  if (outstanding.size >= MAX_UNRESOLVED) {
    // Drop the oldest rather than refuse to record — failing to record is the
    // outcome that costs money. Map iterates in insertion order.
    const oldest = outstanding.keys().next();
    if (!oldest.done) outstanding.delete(oldest.value);
  }
  outstanding.set(k, { request, recordedAt: now });
}

export function unresolvedRefundFor(
  organizerId: string,
  orderId: string,
  now = Date.now(),
): RefundInput | undefined {
  const k = key(organizerId, orderId);
  const entry = outstanding.get(k);
  if (!entry) return undefined;
  if (now - entry.recordedAt > UNRESOLVED_TTL_MS) {
    outstanding.delete(k);
    return undefined;
  }
  return entry.request;
}

/** Called only when commerce has told us what happened to the original key. */
export function clearUnresolvedRefund(organizerId: string, orderId: string): void {
  outstanding.delete(key(organizerId, orderId));
}
