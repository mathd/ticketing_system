import { UPSTREAM_DEADLINE_MS, withUpstreamDeadline } from './upstream';
import type { components } from './access-api-types.gen';
import { dateTimeField, sameUuid, uuidField } from './wire-primitives';

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

/** Access's generated response type. The decoder below checks the wire value. */
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

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function objectBody(value: unknown, name: string): Record<string, unknown> {
  if (!isObject(value)) {
    throw new TypeError(`${name} must be an object`);
  }
  return value;
}

function stringField(value: unknown, name: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${name} must be a non-empty string`);
  }
  return value;
}

function decodeTicketBundle(value: unknown, expectedOrderRef: string): TicketBundle {
  const body = objectBody(value, 'ticket bundle');
  const orderRef = uuidField(body.order_ref, 'order_ref');
  if (!sameUuid(orderRef, expectedOrderRef)) throw new TypeError('order_ref does not match the request');
  if (!Array.isArray(body.tickets)) throw new TypeError('tickets must be an array');
  return {
    order_ref: orderRef,
    tickets: body.tickets.map((value, ticketIndex) => {
      const ticket = objectBody(value, `tickets[${ticketIndex}]`);
      if (!Array.isArray(ticket.history)) {
        throw new TypeError(`tickets[${ticketIndex}].history must be an array`);
      }
      return {
        ticket_id: uuidField(ticket.ticket_id, `tickets[${ticketIndex}].ticket_id`),
        qr_payload: stringField(ticket.qr_payload, `tickets[${ticketIndex}].qr_payload`),
        issued_at: dateTimeField(ticket.issued_at, `tickets[${ticketIndex}].issued_at`),
        qr_url: stringField(ticket.qr_url, `tickets[${ticketIndex}].qr_url`),
        history: ticket.history.map((value, eventIndex) => {
          const event = objectBody(value, `tickets[${ticketIndex}].history[${eventIndex}]`);
          const sequence = event.sequence;
          if (
            sequence !== undefined &&
            (typeof sequence !== 'number' || !Number.isSafeInteger(sequence) || sequence < 1)
          ) {
            throw new TypeError(
              `tickets[${ticketIndex}].history[${eventIndex}].sequence must be a positive integer`,
            );
          }
          return {
            id: uuidField(event.id, `tickets[${ticketIndex}].history[${eventIndex}].id`),
            type: stringField(event.type, `tickets[${ticketIndex}].history[${eventIndex}].type`),
            occurred_at: dateTimeField(
              event.occurred_at,
              `tickets[${ticketIndex}].history[${eventIndex}].occurred_at`,
            ),
            ...(sequence === undefined ? {} : { sequence }),
          };
        }),
      };
    }),
  };
}

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
      return { ok: true, value: decodeTicketBundle(await response.json(), orderRef) };
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
