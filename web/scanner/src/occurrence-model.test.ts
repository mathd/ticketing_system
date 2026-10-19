import { describe, expect, it } from 'vitest'
import { readCurrentRecord } from './occurrence-model'

// ADR-085 §2.2: the store boundary validates every stored row. TypeScript does not
// protect rows that old code or a storage fault wrote. Each case below changes one
// field of a valid row, so a refusal can come only from that field.

const owner = 'owner-a'
const base = {
  occurrenceId: '0f9a54d6-5d2b-4b8e-9d61-1f2c3a4b5c6d',
  qrPayload: 'qr',
  occurredAt: '2026-10-18T09:00:00.000Z',
  createdAt: '2026-10-18T09:00:00.001Z',
  actuated: false,
  ownerId: owner,
  leaseExpiresAt: 1_000,
  recordFormat: 1,
}
const normalPending = { ...base, state: 'PENDING', direction: 'entry' }
const refusalPending = { ...base, state: 'PENDING', localDecision: 'revocation_refused' }
const decided = '2026-10-18T09:00:01.000Z'

const valid: Array<[string, Record<string, unknown>]> = [
  ['normal PENDING', normalPending],
  ['refusal PENDING', refusalPending],
  ['normal QUEUED transport', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided }],
  ['normal QUEUED 503', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'server_unavailable', status: 503 }, decidedAt: decided }],
  ['refusal QUEUED', { ...refusalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: decided }],
  ['recovered normal QUEUED without decidedAt', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } }],
  ['recovered refusal QUEUED without decidedAt', { ...refusalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } }],
  ['normal ACTUATED', { ...normalPending, state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided }],
  ['normal RESOLVED known denial', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 409, latchId: 'l1', reason: 'refunded' }, decidedAt: decided }],
  ['normal RESOLVED unknown denial', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 422, latchId: 'l1', reason: null }, decidedAt: decided }],
  ['normal RESOLVED auth', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: decided }],
  ['normal RESOLVED 500', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'unconfirmed_server_error', status: 500 }, decidedAt: decided }],
  ['normal RESOLVED status 0', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'unconfirmed_status', status: 0 }, decidedAt: decided }],
  ['normal RESOLVED cancelled', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: true }, decidedAt: decided }],
  ['normal SYNCED', { ...normalPending, state: 'SYNCED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided, result: 'recorded' }],
  ['normal RESOLVED by reconcile conflict', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided, result: 'conflict' }],
  ['refusal SYNCED', { ...refusalPending, state: 'SYNCED', localOutcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: decided, result: 'synced' }],
]

describe('current-format record validation', () => {
  it.each(valid)('accepts %s', (_name, raw) => {
    expect(readCurrentRecord(raw)).toEqual({ ok: true, record: raw })
  })

  const invalid: Array<[string, unknown]> = [
    ['a primitive', 'row'],
    ['format 0', { ...normalPending, recordFormat: 0 }],
    ['an absent format', (({ recordFormat: _ignored, ...rest }) => rest)(normalPending)],
    ['format 2', { ...normalPending, recordFormat: 2 }],
    ['a normal row without direction', (({ direction: _ignored, ...rest }) => rest)(normalPending)],
    ['a normal row with direction exit', { ...normalPending, direction: 'exit' }],
    ['a normal row with own undefined direction', { ...normalPending, direction: undefined }],
    ['a row with both direction and refusal', { ...normalPending, localDecision: 'revocation_refused' }],
    ['a refusal with own undefined direction', { ...refusalPending, direction: undefined }],
    ['an unknown local decision', { ...refusalPending, localDecision: 'other' }],
    ['a PENDING row without owner', (({ ownerId: _ignored, ...rest }) => rest)(normalPending)],
    ['a PENDING row with a non-finite lease', { ...normalPending, leaseExpiresAt: Number.NaN }],
    ['a PENDING row with an outcome', { ...normalPending, localOutcome: { family: 'queue', cause: 'transport_failure' } }],
    ['a PENDING row with a decision time', { ...normalPending, decidedAt: decided }],
    ['a PENDING row marked actuated', { ...normalPending, actuated: true }],
    ['a QUEUED row without outcome', { ...normalPending, state: 'QUEUED', decidedAt: decided }],
    ['a QUEUED row with a terminal outcome', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: decided }],
    ['a QUEUED decision without decidedAt', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'transport_failure' } }],
    ['a recovery with an invented decidedAt', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' }, decidedAt: decided }],
    ['a normal row with a refusal queue cause', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: decided }],
    ['a refusal with a transport queue cause', { ...refusalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided }],
    ['a queue status outside 502/503/504', { ...normalPending, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'server_unavailable', status: 500 }, decidedAt: decided }],
    ['an ACTUATED refusal', { ...refusalPending, state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided }],
    ['an ACTUATED row with actuated false', { ...normalPending, state: 'ACTUATED', localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided }],
    ['a terminal refusal', { ...refusalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: decided }],
    ['a denial with status 400', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 400, latchId: 'l1', reason: null }, decidedAt: decided }],
    ['a denial with an empty reason', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 409, latchId: 'l1', reason: '' }, decidedAt: decided }],
    ['a denial without latch', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 409, reason: null }, decidedAt: decided }],
    ['a server error code 502 as U03', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'unconfirmed_server_error', status: 502 }, decidedAt: decided }],
    ['an unconfirmed_status with a server-error code', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'unconfirmed_status', status: 503 }, decidedAt: decided }],
    ['an unknown terminal cause', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'other' }, decidedAt: decided }],
    ['a terminal RESOLVED row with a result', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: decided, result: 'rejected' }],
    ['a SYNCED row with result conflict', { ...normalPending, state: 'SYNCED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided, result: 'conflict' }],
    ['a reconcile RESOLVED row with result recorded', { ...normalPending, state: 'RESOLVED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: decided, result: 'recorded' }],
    ['an unknown state', { ...normalPending, state: 'LOST' }],
    ['a non-string credential', { ...normalPending, qrPayload: 7 }],
    ['an empty occurrence id', { ...normalPending, occurrenceId: '' }],
  ]

  it.each(invalid)('refuses %s', (_name, raw) => {
    expect(readCurrentRecord(raw).ok).toBe(false)
  })
})
