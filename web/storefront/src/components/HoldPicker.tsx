import { useCallback, useEffect, useRef, useState } from 'react';
import { formatMoney } from '../lib/format';
import { UI_STRINGS } from '../lib/locales';
import SeatMapPicker, { type SeatMapHandle, type SeatSelection } from './SeatMapPicker';

// slotId + seatMapId together mean "this performance is seated" (TKT-174). Their
// presence is the mode switch: absence is the GA path, byte-for-byte as before.
// SeatMapPicker owns the seat concern; this component keeps the reservation and the
// checkout, so a seated purchase and a GA one go through ONE reservation state and
// ONE checkout — the seated fork AC3 forbids never exists.
type Props = {
  organizerId: string;
  ticketTypeId: string;
  locale: 'en' | 'fr';
  slotId?: string;
  seatMapId?: string;
};
type Hold = { hold_id: string; expires_at: string; server_time: string; status: string };
type Reservation = Hold & { reservation_id: string; buyer_id: string; amount: number; currency: string };

export function remainingMilliseconds(hold: Pick<Hold, 'expires_at' | 'server_time'>): number {
  return Math.max(0, Date.parse(hold.expires_at) - Date.parse(hold.server_time));
}

/**
 * reservationTerms fingerprints what a reserve call is ASKING FOR, so the idempotency
 * key can be bound to the terms rather than to the click.
 *
 * The seat list is sorted and copied: commerce compares the SET, so [B,A] and [A,B] are
 * one request, and mutating the caller's array here would reorder the buyer's selection
 * as a side effect of naming it.
 */
export function reservationTerms(seated: boolean, seats: string[], quantity: number): string {
  return seated ? `seats:${[...seats].sort().join(',')}` : `qty:${quantity}`;
}

export default function HoldPicker({ organizerId, ticketTypeId, locale, slotId, seatMapId }: Props) {
  const seated = Boolean(slotId && seatMapId);
  // Both halves matter: the identities, and whether the child can currently vouch
  // for them. Gating Reserve on seats.length alone leaves the button live over a
  // selection made before a read failed, closed or shrank (ai-review).
  const [selection, setSelection] = useState<SeatSelection>({ seats: [], claimable: false });
  const [quantity, setQuantity] = useState(1);
  const [remaining, setRemaining] = useState<number | null>(null);
  const [holdId, setHoldId] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const [busy, setBusy] = useState(false);
  const [reservation, setReservation] = useState<Reservation | null>(null);
  const [name, setName] = useState('Test Buyer');
  const [email, setEmail] = useState('buyer@example.test');
  const [paymentToken, setPaymentToken] = useState('fake-ok');
  const [ticketLink, setTicketLink] = useState<string | null>(null);
  const deadline = useRef(0);
  const map = useRef<SeatMapHandle>(null);
  // One string table. There used to be a second, inline one here, and the two had
  // colliding keys with different meanings (`tickets` was "Tickets" in one and "View my
  // tickets" in the other), so a new string landed in whichever the author happened to
  // be looking at — and the status messages never got translated at all (TKT-184).
  const t = UI_STRINGS[locale];
  // Stable identity so SeatMapPicker's effect does not re-run on every render.
  const onSelectionChange = useCallback((next: SeatSelection) => setSelection(next), []);

  // Idempotency keys are STATE, not decoration — minting a fresh uuid per attempt is the
  // same as sending none. Commerce DERIVES the reservation id from the reserve key, so a
  // retry under a new key takes out a SECOND hold; and it compares the checkout key
  // against the one stored on the order, so a retry under a new key is refused 409 and
  // the buyer can never finish paying an order they may already have been charged for.
  //
  // So the key is bound to the TERMS, not to the click: the same request replays under
  // the same key, and changing the selection mints a new one (reusing it there is what
  // commerce answers with "idempotency key reused with different terms").
  const reserveKey = useRef<{ terms: string; key: string } | null>(null);
  const checkoutKeys = useRef(new Map<string, string>());

  function keyForTerms(terms: string): string {
    if (reserveKey.current?.terms !== terms) {
      reserveKey.current = { terms, key: crypto.randomUUID() };
    }
    return reserveKey.current.key;
  }

  function keyForReservation(reservationId: string): string {
    const existing = checkoutKeys.current.get(reservationId);
    if (existing !== undefined) return existing;
    const key = crypto.randomUUID();
    checkoutKeys.current.set(reservationId, key);
    return key;
  }

  useEffect(() => {
    if (holdId === null) return;
    const timer = window.setInterval(() => {
      const next = Math.max(0, deadline.current - performance.now());
      setRemaining(next);
      if (next === 0) {
        window.clearInterval(timer);
        setStatus(t.holdExpired);
      }
    }, 250);
    return () => window.clearInterval(timer);
  }, [holdId, t.holdExpired]);

  async function reserve() {
    setBusy(true); setStatus('');
    try {
      // Exactly one of quantity or seat_identities — the contract rejects both and
      // neither, and so does the handler.
      const claim = seated
        ? { organizer_id: organizerId, ticket_type_id: ticketTypeId, seat_identities: selection.seats }
        : { organizer_id: organizerId, ticket_type_id: ticketTypeId, quantity };
      const response = await fetch('/api/commerce/reservations', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Idempotency-Key': keyForTerms(reservationTerms(seated, selection.seats, quantity)),
        },
        body: JSON.stringify(claim),
      });
      if (!response.ok) {
        // TKT-173 answers a contended selection by NAME. Handing those identities to
        // the map is the whole point of it going to that trouble: the buyer sees
        // exactly which seats they lost, keeps the ones they did not, and the map
        // re-renders rather than going stale under a generic error.
        if (seated && response.status === 409) {
          const refusal = await response.json().catch(() => null) as
            { code?: string; seat_identities?: string[] } | null;
          if (refusal?.code === 'seat_taken' && refusal.seat_identities?.length) {
            map.current?.applyConflict(refusal.seat_identities);
            setStatus(t.seatsNoLongerAvailable.replace('{seats}', refusal.seat_identities.join(', ')));
            return;
          }
          // An orphan refusal names seats that are FREE and that the buyer did not
          // request. They must NOT go through the conflict channel: marking them
          // unavailable would remove the buyer's only repair, which is to add one.
          if (refusal?.code === 'orphaned_seats' && refusal.seat_identities?.length) {
            setStatus(t.seatsWouldStrand.replace('{seats}', refusal.seat_identities.join(', ')));
            return;
          }
        }
        setStatus(t.quantityUnavailable); return;
      }
      const hold = await response.json() as Reservation;
      const duration = remainingMilliseconds(hold);
      deadline.current = performance.now() + duration;
      setRemaining(duration); setHoldId(hold.hold_id); setReservation(hold); setStatus(t.heldFor);
    } catch { setStatus(t.serviceUnavailable); }
    finally { setBusy(false); }
  }

  async function checkout() {
    if (!reservation) return;
    setBusy(true); setStatus('');
    try {
      // Posts to the storefront's own bridge, not straight to commerce (TKT-221).
      // The session cookie is httpOnly, so this island cannot know who is signed
      // in — and must not: the proof of identity is added server-side, where the
      // browser cannot reach it. Signed out, the bridge forwards the same request
      // to the same place and the checkout is a guest checkout exactly as before.
      const response = await fetch('/checkout', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Idempotency-Key': keyForReservation(reservation.reservation_id),
        },
        body: JSON.stringify({ reservation_id: reservation.reservation_id, name, email, payment_token: paymentToken }),
      });
      // Tolerant parse: the error bodies are JSON but a proxy failure page is not, and
      // throwing here would report a decided outcome as an unknown one.
      const result = await response.json().catch(() => ({})) as
        { order_id?: string; guest_order_ref?: string; status?: string };
      if (response.ok && result.status === 'completed' && result.guest_order_ref) { setRemaining(0); setTicketLink(`/${locale}/tickets/${result.guest_order_ref}`); setStatus(t.orderConfirmed); return; }
      if (response.status === 402 || response.status === 408) { setReservation(null); setHoldId(null); setRemaining(null); setStatus(t.paymentDeclined); return; }
      // 401 = the customer assertion was refused (expired, or signed with a key
      // that has since rotated). It is NOT the payment-uncertainty answer, which
      // is what this fell through to before — a lie in the frightening direction.
      //
      // But it is not proof that nothing happened either, and the first version of
      // this said "your seats are still held" as if it were (ai-review pass 2
      // [high]). Commerce verifies the assertion BEFORE it resolves an existing
      // order, so a retry of an already-successful checkout whose assertion has
      // since died gets this same 401 — with the order completed and the seats
      // long since confirmed. The copy therefore points at the tickets page rather
      // than asserting a state this code cannot know.
      if (response.status === 401) { setStatus(t.signInAgain); return; }
      // 409 is commerce holding this order under its recovery lease. It clears on its
      // own, and because the key above is stable the retry is a REPLAY rather than a
      // second attempt — so keep the reservation and say "try again" instead of
      // stranding the buyer on the ambiguous checking message (TKT-184).
      //
      // NOT covered, and named so it is not mistaken for covered: claimOrder answers
      // 409 for a second reason — `storedFingerprint != fingerprint`, the fingerprint
      // being sha256 over reservation id, name, email and payment token. A buyer who
      // edits any of those between attempts gets this same 409, and for them it does
      // NOT clear on its own, so "try again in a moment" is a promise this branch
      // cannot keep. It is still an improvement on what main said there (the same
      // ambiguous "being checked"), and the two causes are distinguishable only by
      // commerce's error string today. Telling them apart wants a machine-readable
      // code on the response rather than copy matched against prose.
      if (response.status === 409) { setStatus(t.checkoutRetryShortly); return; }
      setStatus(t.paymentChecking);
    } catch { setStatus(t.paymentChecking); }
    finally { setBusy(false); }
  }

  const seconds = Math.ceil((remaining ?? 0) / 1000);
  const holding = remaining !== null && remaining > 0;
  return <div className="hold-picker">
    {seated
      ? <SeatMapPicker ref={map} organizerId={organizerId} slotId={slotId!} seatMapId={seatMapId!} locale={locale} onSelectionChange={onSelectionChange} />
      : <label>{t.quantity} <input aria-label={t.quantity} type="number" min="1" max="50" value={quantity} onChange={(e) => setQuantity(Math.max(1, Math.min(50, Number(e.target.value))))} /></label>}
    <button type="button" disabled={busy || holding || (seated && !selection.claimable)} onClick={reserve}>{seated ? t.reserveSeats : t.reserve}</button>
    <span aria-live="polite">{status}{remaining !== null && remaining > 0 ? ` ${seconds}s` : ''}</span>
    {ticketLink && <a href={ticketLink}>{t.viewMyTickets}</a>}
    {reservation && remaining !== null && remaining > 0 && <div className="checkout-form">
      <label>{t.nameLabel} <input aria-label={t.nameLabel} value={name} onChange={(e) => setName(e.target.value)} /></label>
      <label>{t.emailLabel} <input aria-label={t.emailLabel} type="email" value={email} onChange={(e) => setEmail(e.target.value)} /></label>
      <label>Fake payment <select aria-label="Fake payment" value={paymentToken} onChange={(e) => setPaymentToken(e.target.value)}><option value="fake-ok">Success</option><option value="fake-decline">Decline</option><option value="fake-timeout">Timeout</option></select></label>
      <button type="button" disabled={busy} onClick={checkout}>{t.pay} {formatMoney(reservation.amount, reservation.currency, locale)}</button>
    </div>}
  </div>;
}
