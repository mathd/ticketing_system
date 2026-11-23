import { randomUUID } from 'node:crypto';

import { ORGANIZER, sql } from './support.mjs';

// A paid, completed commerce order with NO payments operation behind it yet:
// payments answers its first refund with 404, which commerce reports as 502, and
// the page saves the request as an unresolved retry. Shared by the refund specs
// (refund-abandoned.mjs, refund-redirected.mjs).
export function seedPaidOrder(pg, label, quantity, unit, stamp) {
  const o = {
    reservation: randomUUID(),
    order: randomUUID(),
    ref: randomUUID(),
    buyer: randomUUID(),
    chargeKey: `browser-${label}-${stamp}`,
    quantity,
    unit,
  };
  sql(
    pg,
    'commerce',
    `INSERT INTO reservations
       (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
        unit_amount, total_amount, face_value_amount, currency, status)
     VALUES
       ('${o.reservation}', '${ORGANIZER}', '${randomUUID()}', '${randomUUID()}',
        '${randomUUID()}', '${o.buyer}', ${quantity}, ${unit}, ${quantity * unit}, ${quantity * unit}, 'EUR', 'completed');
     INSERT INTO orders
       (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
     VALUES
       ('${o.order}', '${o.reservation}', 'completed', '${o.chargeKey}', '${o.chargeKey}', '${o.ref}');`,
  );
  return o;
}
