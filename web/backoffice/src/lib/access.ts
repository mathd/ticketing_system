// The back office's access client: the ticket bundle behind a guest reference
// (TKT-193), with the QR credential removed.
import { GATEWAY_URL, type Read, readJson, required, sameIdentity } from './upstream';

const access = (path: string) => `${GATEWAY_URL}/api/access${path}`;

export type LifecycleEvent = {
  id: string;
  type: string;
  /** Absent on legacy rows the chain backfill has not adopted (ADR-025 §D5). */
  sequence?: number;
  occurredAt: string;
};
export type SafeTicket = { ticketId: string; issuedAt: string; history: LifecycleEvent[] };

/**
 * The ticket bundle, with the QR credential removed.
 *
 * Access returns `qr_payload` — the value a scanner admits on — and `qr_url`,
 * which points at an endpoint the gateway serves WITHOUT authentication. Either
 * one rendered on a staff console is a working ticket for someone else's order,
 * and it survives in screenshots and support transcripts long after the visit.
 * The allow-list is here rather than in the page so that no page, present or
 * future, can render what it was never handed.
 */
export function getOrderTickets(ref: string): Promise<Read<SafeTicket[]>> {
  return readJson(access(`/orders/${encodeURIComponent(ref)}/tickets`), (body) => {
    const b = body as { tickets?: unknown; order_ref?: unknown };
    sameIdentity(required(b.order_ref, 'order_ref'), ref, 'order_ref');
    if (!Array.isArray(b.tickets)) throw new Error('response is missing tickets');
    return b.tickets.map((raw) => {
      const t = raw as Record<string, unknown>;
      // NOT `t.history ?? []`. The access contract makes `history` required, so
      // its absence is a broken response, and defaulting it would render "no
      // lifecycle events recorded yet" — a statement about the ticket — when
      // what actually happened is that access did not answer properly.
      const history = t.history;
      if (!Array.isArray(history)) throw new Error('ticket history is missing or not a list');
      return {
        ticketId: required(t.ticket_id, 'ticket_id'),
        issuedAt: required(t.issued_at, 'issued_at'),
        history: history.map((rawEvent) => {
          const e = rawEvent as Record<string, unknown>;
          // A sequence that is present but not a number is a contract the client
          // does not understand — rendering NaN beside a lifecycle event would
          // read as a gap in the integrity chain (ADR-025 §D5) rather than as
          // this client's confusion.
          // Absent is legitimate — legacy rows the chain backfill has not
          // adopted. Present means an integer >= 1 (openapi.yaml: int64,
          // minimum 1); 0 or 1.5 is a chain position that cannot exist, and
          // rendering one as "#0" would read as an integrity gap rather than as
          // this client accepting nonsense.
          if (
            e.sequence !== undefined &&
            (typeof e.sequence !== 'number' || !Number.isInteger(e.sequence) || e.sequence < 1)
          ) {
            throw new Error('lifecycle sequence is not a chain position');
          }
          return {
            id: required(e.id, 'event id'),
            type: required(e.type, 'event type'),
            sequence: e.sequence as number | undefined,
            occurredAt: required(e.occurred_at, 'occurred_at'),
          };
        }),
      };
    });
  });
}
