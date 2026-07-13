import { useEffect, useRef, useState } from 'react';
import { formatMoney } from '../lib/format';

type Props = { organizerId: string; ticketTypeId: string; locale: 'en' | 'fr' };
type Hold = { hold_id: string; expires_at: string; server_time: string; status: string };
type Reservation = Hold & { reservation_id: string; buyer_id: string; amount: number; currency: string };

export function remainingMilliseconds(hold: Pick<Hold, 'expires_at' | 'server_time'>): number {
  return Math.max(0, Date.parse(hold.expires_at) - Date.parse(hold.server_time));
}

export default function HoldPicker({ organizerId, ticketTypeId, locale }: Props) {
  const [quantity, setQuantity] = useState(1);
  const [remaining, setRemaining] = useState<number | null>(null);
  const [holdId, setHoldId] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const [busy, setBusy] = useState(false);
  const [reservation, setReservation] = useState<Reservation | null>(null);
  const [name, setName] = useState('Test Buyer');
  const [email, setEmail] = useState('buyer@example.test');
  const [paymentToken, setPaymentToken] = useState('fake-ok');
  const deadline = useRef(0);
  const t = locale === 'fr'
    ? { reserve: 'Réserver', pay: 'Payer', quantity: 'Quantité', held: 'Réservé pendant', expired: 'Réservation expirée', unavailable: 'Quantité indisponible', completed: 'Commande confirmée', declined: 'Paiement refusé — réessayez' }
    : { reserve: 'Reserve', pay: 'Pay', quantity: 'Quantity', held: 'Held for', expired: 'Hold expired', unavailable: 'Quantity unavailable', completed: 'Order confirmed', declined: 'Payment declined — try again' };

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
      const response = await fetch('/api/commerce/reservations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
        body: JSON.stringify({ organizer_id: organizerId, ticket_type_id: ticketTypeId, quantity }),
      });
      if (!response.ok) { setStatus(t.unavailable); return; }
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
      const result = await response.json() as { order_id?: string; status?: string };
      if (response.ok && result.status === 'completed') { setRemaining(0); setStatus(`${t.completed}: ${result.order_id}`); return; }
      if (response.status === 402 || response.status === 408) { setReservation(null); setHoldId(null); setRemaining(null); setStatus(t.declined); return; }
      setStatus('Payment status is being checked');
    } catch { setStatus('Payment status is being checked'); }
    finally { setBusy(false); }
  }

  const seconds = Math.ceil((remaining ?? 0) / 1000);
  return <div className="hold-picker">
    <label>{t.quantity} <input aria-label={t.quantity} type="number" min="1" max="50" value={quantity} onChange={(e) => setQuantity(Math.max(1, Math.min(50, Number(e.target.value))))} /></label>
    <button type="button" disabled={busy || (remaining !== null && remaining > 0)} onClick={reserve}>{t.reserve}</button>
    <span aria-live="polite">{status}{remaining !== null && remaining > 0 ? ` ${seconds}s` : ''}</span>
    {reservation && remaining !== null && remaining > 0 && <div className="checkout-form">
      <label>Name <input aria-label="Name" value={name} onChange={(e) => setName(e.target.value)} /></label>
      <label>Email <input aria-label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} /></label>
      <label>Fake payment <select aria-label="Fake payment" value={paymentToken} onChange={(e) => setPaymentToken(e.target.value)}><option value="fake-ok">Success</option><option value="fake-decline">Decline</option><option value="fake-timeout">Timeout</option></select></label>
      <button type="button" disabled={busy} onClick={checkout}>{t.pay} {formatMoney(reservation.amount, reservation.currency, locale)}</button>
    </div>}
  </div>;
}
