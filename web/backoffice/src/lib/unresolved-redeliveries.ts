// Resends access may have performed without telling us (TKT-203, ai-review F1).
//
// When a resend times out or answers 5xx, access may already have handed some or all of
// the order's tickets to the transport. The only safe next step is replaying the SAME
// idempotency key: access treats that as a resume, finishing what is outstanding and
// re-sending nothing that was already accepted. A NEW key is a different request — it
// sends every ticket again and consumes another slot in the per-order bound.
//
// Holding the key in the page does not work, and this is the defect this module exists
// to close. The console mints a fresh key on every render, so an operator who follows
// the on-screen instruction — "submitting the same form again replays the same request"
// — was in fact issuing a second request. The page STATED a guarantee it did not
// provide, which is worse than offering nothing: the instruction was wrong, not merely
// unhelpful.
//
// So the server holds it, keyed by organizer and order, which is the scope the risk
// actually has: it is the ORDER that has an unsettled resend, not the operator who
// happened to submit it. A colleague looking the same order up gets the retry too.
//
// **Why this is a sibling of unresolved-refunds rather than an extension of it.** That
// module carries a documented expiry — TKT-201's commerce read replaces it, and its own
// header says whoever lands that should delete it rather than extend it. Widening it to
// a second kind of outstanding work would tie the resend's correctness to a module
// scheduled for removal. The two also differ in what they protect: a refund's hazard is
// moving money twice, so it BLOCKS a new refund while unsettled; a resend's hazard is
// re-emitting a capability and burning the bound, so it PREFERS the original key while
// still allowing a deliberate new send. Different rule, different module.
//
// **Limits, stated rather than implied.** In-memory, like the session store and the
// refund store beside it. It does not survive a restart and is not shared between
// replicas; the back office runs as a single container today. A restart during an outage
// loses the key, and the operator's recourse is then a fresh resend — which costs one
// slot of the per-order bound and one extra mail to the address already on file, not a
// wrong outcome. What it closes is the far larger everyday window: the operator looked
// the order up again, or reloaded, or handed the shift over.
import type { RedeliveryInput } from './order-console';

type Entry = { request: RedeliveryInput; recordedAt: number };

const outstanding = new Map<string, Entry>();

/**
 * How long an unsettled resend keeps being offered as a retry.
 *
 * Shorter than the refund's 24h, deliberately. The cost of holding a resend key too long
 * is that an operator is offered a replay of a request access has long since settled —
 * harmless, it returns the same answer. The cost of holding it too SHORT is one extra
 * mail to the address on file. Neither is expensive, so this is sized to the support
 * interaction it serves — a customer on the phone, or calling back the same day — rather
 * than to a reconciliation window.
 */
export const UNRESOLVED_REDELIVERY_TTL_MS = 60 * 60 * 1000;

/** A bound, so a run of outages cannot grow this without limit. */
export const MAX_UNRESOLVED_REDELIVERIES = 1000;

const key = (organizerId: string, orderId: string) => `${organizerId} ${orderId}`;

export function resetUnresolvedRedeliveriesForTest(): void {
  outstanding.clear();
}

export function noteUnresolvedRedelivery(
  organizerId: string,
  request: RedeliveryInput,
  now = Date.now(),
): void {
  const k = key(organizerId, request.orderId);
  // Never overwrite an existing entry: the FIRST unsettled key is the one access may
  // have partially executed, and it is the only key that can RESUME that work. A later
  // attempt's key would start a fresh request and leave the partial one stranded.
  if (outstanding.has(k)) return;
  if (outstanding.size >= MAX_UNRESOLVED_REDELIVERIES) {
    // Drop the oldest rather than refuse to record. Map iterates in insertion order.
    const oldest = outstanding.keys().next();
    if (!oldest.done) outstanding.delete(oldest.value);
  }
  outstanding.set(k, { request, recordedAt: now });
}

export function unresolvedRedeliveryFor(
  organizerId: string,
  orderId: string,
  now = Date.now(),
): RedeliveryInput | undefined {
  const k = key(organizerId, orderId);
  const entry = outstanding.get(k);
  if (!entry) return undefined;
  if (now - entry.recordedAt > UNRESOLVED_REDELIVERY_TTL_MS) {
    outstanding.delete(k);
    return undefined;
  }
  return entry.request;
}

/** Called when access has told us what happened to the original key. */
export function clearUnresolvedRedelivery(organizerId: string, orderId: string): void {
  outstanding.delete(key(organizerId, orderId));
}
