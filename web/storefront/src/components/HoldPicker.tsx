import { useEffect, useRef, useState } from 'react';

type Props = { organizerId: string; slotId: string; locale: 'en' | 'fr' };
type Hold = { hold_id: string; expires_at: string; server_time: string; status: string };

export function remainingMilliseconds(hold: Pick<Hold, 'expires_at' | 'server_time'>): number {
  return Math.max(0, Date.parse(hold.expires_at) - Date.parse(hold.server_time));
}

export default function HoldPicker({ organizerId, slotId, locale }: Props) {
  const [quantity, setQuantity] = useState(1);
  const [remaining, setRemaining] = useState<number | null>(null);
  const [status, setStatus] = useState('');
  const [busy, setBusy] = useState(false);
  const deadline = useRef(0);
  const t = locale === 'fr'
    ? { reserve: 'Réserver', quantity: 'Quantité', held: 'Réservé pendant', expired: 'Réservation expirée', unavailable: 'Quantité indisponible' }
    : { reserve: 'Reserve', quantity: 'Quantity', held: 'Held for', expired: 'Hold expired', unavailable: 'Quantity unavailable' };

  useEffect(() => {
    if (remaining === null || remaining <= 0) return;
    const timer = window.setInterval(() => {
      const next = Math.max(0, deadline.current - performance.now());
      setRemaining(next);
      if (next === 0) setStatus(t.expired);
    }, 250);
    return () => window.clearInterval(timer);
  }, [remaining === null, status]);

  async function reserve() {
    setBusy(true); setStatus('');
    try {
      const response = await fetch('/api/inventory/holds', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
        body: JSON.stringify({ organizer_id: organizerId, slot_id: slotId, quantity }),
      });
      if (!response.ok) { setStatus(t.unavailable); return; }
      const hold = await response.json() as Hold;
      const duration = remainingMilliseconds(hold);
      deadline.current = performance.now() + duration;
      setRemaining(duration); setStatus(t.held);
    } catch { setStatus('Service unavailable'); }
    finally { setBusy(false); }
  }

  const seconds = Math.ceil((remaining ?? 0) / 1000);
  return <div className="hold-picker">
    <label>{t.quantity} <input aria-label={t.quantity} type="number" min="1" max="50" value={quantity} onChange={(e) => setQuantity(Math.max(1, Math.min(50, Number(e.target.value))))} /></label>
    <button type="button" disabled={busy || (remaining !== null && remaining > 0)} onClick={reserve}>{t.reserve}</button>
    <span aria-live="polite">{status}{remaining !== null && remaining > 0 ? ` ${seconds}s` : ''}</span>
  </div>;
}
