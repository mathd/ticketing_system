import { useCallback, useEffect, useRef, useState } from 'react';
import { formatMoney } from '../lib/format';
import { UI_STRINGS } from '../lib/locales';
import SeatMapPicker, { type SeatSelection } from './SeatMapPicker';

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
  const strings = UI_STRINGS[locale];
  // Stable identity so SeatMapPicker's effect does not re-run on every render.
  const onSelectionChange = useCallback((next: SeatSelection) => setSelection(next), []);
  const t = locale === 'fr'
    ? { reserve: 'Réserver', pay: 'Payer', quantity: 'Quantité', held: 'Réservé pendant', expired: 'Réservation expirée', unavailable: 'Quantité indisponible', completed: 'Commande confirmée', tickets: 'Voir mes billets', declined: 'Paiement refusé — réessayez' }
    : { reserve: 'Reserve', pay: 'Pay', quantity: 'Quantity', held: 'Held for', expired: 'Hold expired', unavailable: 'Quantity unavailable', completed: 'Order confirmed', tickets: 'View my tickets', declined: 'Payment declined — try again' };

  useEffect(() => {
    if (holdId === null) return;
    const timer = window.setInterval(() => {
      const next = Math.max(0, deadline.current - performance.now());
      setRemaining(next);
      if (next === 0) {
        window.clearInterval(timer);
        setStatus(t.expired);
      }
    }, 250);
    return () => window.clearInterval(timer);
  }, [holdId, t.expired]);

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
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
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
            window.dispatchEvent(new CustomEvent(`seat-conflict:${slotId}`, { detail: refusal.seat_identities }));
            setStatus(strings.seatsNoLongerAvailable.replace('{seats}', refusal.seat_identities.join(', ')));
            return;
          }
          // An orphan refusal names seats that are FREE and that the buyer did not
          // request. They must NOT go through the conflict channel: marking them
          // unavailable would remove the buyer's only repair, which is to add one.
          if (refusal?.code === 'orphaned_seats' && refusal.seat_identities?.length) {
            setStatus(strings.seatsWouldStrand.replace('{seats}', refusal.seat_identities.join(', ')));
            return;
          }
        }
        setStatus(t.unavailable); return;
      }
      const hold = await response.json() as Reservation;
      const duration = remainingMilliseconds(hold);
      deadline.current = performance.now() + duration;
      setRemaining(duration); setHoldId(hold.hold_id); setReservation(hold); setStatus(t.held);
    } catch { setStatus('Service unavailable'); }
    finally { setBusy(false); }
  }

  async function checkout() {
    if (!reservation) return;
    setBusy(true); setStatus('');
    try {
      const response = await fetch('/api/commerce/orders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
        body: JSON.stringify({ reservation_id: reservation.reservation_id, name, email, payment_token: paymentToken }),
      });
      const result = await response.json() as { order_id?: string; guest_order_ref?: string; status?: string };
      if (response.ok && result.status === 'completed' && result.guest_order_ref) { setRemaining(0); setTicketLink(`/${locale}/tickets/${result.guest_order_ref}`); setStatus(t.completed); return; }
      if (response.status === 402 || response.status === 408) { setReservation(null); setHoldId(null); setRemaining(null); setStatus(t.declined); return; }
      setStatus('Payment status is being checked');
    } catch { setStatus('Payment status is being checked'); }
    finally { setBusy(false); }
  }

  const seconds = Math.ceil((remaining ?? 0) / 1000);
  const holding = remaining !== null && remaining > 0;
  return <div className="hold-picker">
    {seated
      ? <SeatMapPicker organizerId={organizerId} slotId={slotId!} seatMapId={seatMapId!} locale={locale} onSelectionChange={onSelectionChange} />
      : <label>{t.quantity} <input aria-label={t.quantity} type="number" min="1" max="50" value={quantity} onChange={(e) => setQuantity(Math.max(1, Math.min(50, Number(e.target.value))))} /></label>}
    <button type="button" disabled={busy || holding || (seated && !selection.claimable)} onClick={reserve}>{seated ? strings.reserveSeats : t.reserve}</button>
    <span aria-live="polite">{status}{remaining !== null && remaining > 0 ? ` ${seconds}s` : ''}</span>
    {ticketLink && <a href={ticketLink}>{t.tickets}</a>}
    {reservation && remaining !== null && remaining > 0 && <div className="checkout-form">
      <label>Name <input aria-label="Name" value={name} onChange={(e) => setName(e.target.value)} /></label>
      <label>Email <input aria-label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} /></label>
      <label>Fake payment <select aria-label="Fake payment" value={paymentToken} onChange={(e) => setPaymentToken(e.target.value)}><option value="fake-ok">Success</option><option value="fake-decline">Decline</option><option value="fake-timeout">Timeout</option></select></label>
      <button type="button" disabled={busy} onClick={checkout}>{t.pay} {formatMoney(reservation.amount, reservation.currency, locale)}</button>
    </div>}
  </div>;
}
