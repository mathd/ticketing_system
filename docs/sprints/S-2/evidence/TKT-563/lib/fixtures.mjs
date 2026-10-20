// Synthetic ticket fixtures and Access readbacks for the TKT-563 journeys.
//
// The credential shape and the signing path follow test/browser/lib/scanner-entry-core.mjs.
// The re-entry policy row follows services/access/internal/store/policy.go: a
// slot without a row is single, and every journey policy here is written
// explicitly so that the fixture states its own policy.

import { createPrivateKey, randomUUID, sign } from 'node:crypto';

export const POLICY = Object.freeze({
  single: { mode: 'single', maxEntries: null, requiresExit: false },
  multi: { mode: 'multi', maxEntries: null, requiresExit: false },
  multiRequiresExit: { mode: 'multi', maxEntries: null, requiresExit: true },
  countLimitedOne: { mode: 'count_limited', maxEntries: 1, requiresExit: false },
});

// PKCS#8 prefix for a raw Ed25519 seed, as scanner-entry-core.mjs builds it.
const ED25519_PKCS8_PREFIX = '302e020100300506032b657004220420';

export function createFixtures({ PG, QR_SEED, QR_KID, ORGANIZER, sql, scrubber }) {
  const privateKey = createPrivateKey({
    key: Buffer.concat([Buffer.from(ED25519_PKCS8_PREFIX, 'hex'), Buffer.from(QR_SEED, 'base64')]),
    format: 'der',
    type: 'pkcs8',
  });
  return {
    create(label, policy) {
      const ticket = randomUUID();
      const order = randomUUID();
      const slot = randomUUID();
      const issuedAt = new Date();
      const header = Buffer.from(JSON.stringify({ alg: 'EdDSA', kid: QR_KID, typ: 'TKT' })).toString('base64url');
      const claims = Buffer.from(
        JSON.stringify({ v: 1, tid: ticket, oid: order, org: ORGANIZER, sid: slot, iat: Math.floor(issuedAt.getTime() / 1000) }),
      ).toString('base64url');
      const body = `${header}.${claims}`;
      const credential = `${body}.${sign(null, Buffer.from(body), privateKey).toString('base64url')}`;
      const maxEntries = policy.maxEntries === null ? 'NULL' : String(policy.maxEntries);
      sql(
        PG,
        'access',
        `INSERT INTO tickets (id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at) VALUES ('${ticket}','${order}','${randomUUID()}','${ORGANIZER}','${randomUUID()}','${slot}','${randomUUID()}','${credential}','${issuedAt.toISOString()}'); INSERT INTO slot_re_entry_policies(slot_id,organizer_id,mode,max_entries,requires_exit) VALUES('${slot}','${ORGANIZER}','${policy.mode}',${maxEntries},${policy.requiresExit ? 'true' : 'false'})`,
      );
      scrubber.add(credential);
      return { label, ticket, order, slot, credential, policy, issuedAt: issuedAt.toISOString() };
    },
  };
}

// Readbacks use the same columns the active spec reads. Every result is JSON
// text produced by Postgres, so the probe never parses a driver object.
export function createReadbacks({ PG, sql }) {
  const json = (statement) => JSON.parse(sql(PG, 'access', statement));
  const at = (column) => `to_char(${column} AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`;
  return {
    // Identifiers, types and times of the admission facts for one ticket.
    facts(ticket) {
      return json(`SELECT COALESCE(json_agg(json_build_object('occurrence_id',id,'event_type',event_type,'occurred_at',${at('occurred_at')}) ORDER BY occurred_at,id),'[]'::json)::text FROM lifecycle_events WHERE ticket_id='${ticket}'`);
    },
    // Every column of every lifecycle row for one ticket, for byte-level comparison.
    rows(ticket) {
      return json(`SELECT COALESCE(json_agg(row_to_json(e) ORDER BY e.occurred_at,e.id),'[]'::json)::text FROM lifecycle_events e WHERE e.ticket_id='${ticket}'`);
    },
    quarantineCount(ticket) {
      return Number(sql(PG, 'access', `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id='${ticket}'`));
    },
    localDecisionsForOccurrence(occurrence) {
      return json(`SELECT COALESCE(json_agg(json_build_object('occurrence_id',occurrence_id::text,'ticket_id',ticket_id::text,'organizer_id',organizer_id::text,'occurred_at',${at('occurred_at')},'decision',decision)),'[]'::json)::text FROM scanner_local_decisions WHERE occurrence_id='${occurrence}'`);
    },
    localDecisionsForTicket(ticket) {
      return json(`SELECT COALESCE(json_agg(json_build_object('occurrence_id',occurrence_id::text,'ticket_id',ticket_id::text,'organizer_id',organizer_id::text,'occurred_at',${at('occurred_at')},'decision',decision)),'[]'::json)::text FROM scanner_local_decisions WHERE ticket_id='${ticket}'`);
    },
    // Every alarm row in every delivery state. The envelope is the stored JSON.
    // The delivery columns come from migration 0003. They are mutable, so checks
    // read only the booleans, and retained evidence keeps all of them.
    alarmRows() {
      return json(`SELECT COALESCE(json_agg(json_build_object('event_id',event_id::text,'subject',subject,'published',published_at IS NOT NULL,'dead_lettered',dead_lettered_at IS NOT NULL,'envelope',envelope,'created_at',created_at,'published_at',published_at,'claim_id',claim_id::text,'lease_until',lease_until,'attempts',attempts,'next_attempt_at',next_attempt_at,'last_error',last_error,'dead_lettered_at',dead_lettered_at) ORDER BY created_at,event_id),'[]'::json)::text FROM lifecycle_integrity_alarm_outbox`);
    },
  };
}
