import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { ProposedDecision } from './occurrence-coordinator'
import { LEGACY_INVALID_DIRECTION, planReconcileBatches } from './reconcile-batch'
import { createTestPage, destroyAllPages, mintOn, type TestPage } from './test/page-fixture'
import { V2_STORES, seedDatabase } from './test/raw-db'

// ST tier with batch planner fixtures (plan, checkpoint stage): queued entries
// come from the production store; the expected wire rows are written out from
// ADR-085 §7.4 and owner D31, not from a serializer run.

let dbName: string
beforeEach(() => {
  dbName = `plan-${crypto.randomUUID()}`
})
afterEach(() => destroyAllPages())

const time = '2026-10-02T10:00:00.123Z'
const legacy = (id: string, extra: Record<string, unknown>) =>
  ({ occurrenceId: id, qrPayload: `qr-${id}`, occurredAt: time, state: 'QUEUED', actuated: false, createdAt: time, ...extra })
const wire = (id: string, extra: Record<string, unknown> = {}) => ({ qr_payload: `qr-${id}`, occurrence_id: id, occurred_at: time, ...extra })

async function migrated(rows: unknown[]): Promise<TestPage> {
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: rows })
  const a = createTestPage({ dbName, ownerId: 'owner-a', renewalMs: 3_600_000 })
  await a.ready()
  return a
}

describe('legacy direction rules (I5-04a, I5-04b)', () => {
  it.each([
    ['no own direction: omit event_type (legacy Entry default)', {}, {}],
    ['own entry', { direction: 'entry' }, { event_type: 'entry' }],
    ['own exit', { direction: 'exit' }, { event_type: 'exit' }],
    ['an unknown nonempty string', { direction: 'sideways' }, { event_type: 'sideways' }],
    ['own undefined', { direction: undefined }, { event_type: LEGACY_INVALID_DIRECTION }],
    ['null', { direction: null }, { event_type: LEGACY_INVALID_DIRECTION }],
    ['an empty string', { direction: '' }, { event_type: LEGACY_INVALID_DIRECTION }],
    ['zero', { direction: 0 }, { event_type: LEGACY_INVALID_DIRECTION }],
    ['false', { direction: false }, { event_type: LEGACY_INVALID_DIRECTION }],
  ])('%s', async (_name, stored, sent) => {
    const a = await migrated([legacy('m', stored), legacy('control', {})])
    const plan = planReconcileBatches((await a.store().queued()).entries)
    expect(plan).toStrictEqual({
      batches: [{ keys: ['control', 'm'], occurrences: [wire('control'), wire('m', sent)], isolated: false }],
      unsendable: [],
    })
  })
})

describe('refusal rows (I5-05)', () => {
  it('sends local_decision only for legacy refusals, even beside a direction-looking property', async () => {
    const a = await migrated([
      legacy('r1', { localDecision: 'revocation_refused', direction: 'exit' }),
      legacy('r2', { localDecision: 'revocation_refused' }),
    ])
    expect(planReconcileBatches((await a.store().queued()).entries).batches[0].occurrences).toStrictEqual([
      wire('r1', { local_decision: 'revocation_refused' }),
      wire('r2', { local_decision: 'revocation_refused' }),
    ])
  })

  it('sends explicit entry for a new normal row and local_decision only for a new refusal (I7-01)', async () => {
    const a = createTestPage({ dbName, ownerId: 'owner-a', renewalMs: 3_600_000 })
    await a.ready()
    const normal = await mintOn(a)
    const refusal = await mintOn(a, { type: 'refusal' })
    const decide = (occurrenceId: string, cause: 'transport_failure' | 'local_refusal'): ProposedDecision =>
      ({ kind: 'queue', occurrenceId, outcome: { family: 'queue', cause }, decidedAt: time })
    a.coordinator.latch(decide(normal.scan.occurrenceId, 'transport_failure'))
    a.coordinator.latch(decide(refusal.scan.occurrenceId, 'local_refusal'))
    await a.coordinator.repairAll()
    const plan = planReconcileBatches((await a.store().queued()).entries)
    const expected = [
      { qr_payload: normal.scan.qrPayload, occurrence_id: normal.scan.occurrenceId, occurred_at: normal.scan.occurredAt, event_type: 'entry' },
      { qr_payload: refusal.scan.qrPayload, occurrence_id: refusal.scan.occurrenceId, occurred_at: refusal.scan.occurredAt, local_decision: 'revocation_refused' },
    ].sort((x, y) => x.occurrence_id.localeCompare(y.occurrence_id))
    expect(plan.batches).toStrictEqual([{ keys: expected.map((row) => row.occurrence_id), occurrences: expected, isolated: false }])
  })
})

describe('odd pre-upgrade rows (TKT-558 D12, D17)', () => {
  it('sends a non-string credential alone, literally, and keeps a valid control in its own batch', async () => {
    const a = await migrated([legacy('odd', { qrPayload: 42 }), legacy('control', {})])
    expect(planReconcileBatches((await a.store().queued()).entries)).toStrictEqual({
      batches: [
        { keys: ['control'], occurrences: [wire('control')], isolated: false },
        { keys: ['odd'], occurrences: [{ qr_payload: 42, occurrence_id: 'odd', occurred_at: time }], isolated: true },
      ],
      unsendable: [],
    })
  })

  it('isolates a non-string local decision and sends it literally', async () => {
    const a = await migrated([legacy('odd', { localDecision: 7 })])
    expect(planReconcileBatches((await a.store().queued()).entries).batches).toStrictEqual([
      { keys: ['odd'], occurrences: [wire('odd', { local_decision: 7 })], isolated: true },
    ])
  })

  it.each([
    ['an absent credential', (id: string) => { const row: Record<string, unknown> = legacy(id, {}); delete row.qrPayload; return row }, 'qrPayload has no value'],
    ['an own undefined time', (id: string) => legacy(id, { occurredAt: undefined }), 'occurredAt has no value'],
    ['a non-finite time', (id: string) => legacy(id, { occurredAt: Number.NaN }), 'occurredAt cannot be sent exactly'],
    ['a date object', (id: string) => legacy(id, { occurredAt: new Date(0) }), 'occurredAt cannot be sent exactly'],
  ])('does not send %s; the row stays listed and other rows proceed', async (_name, build, reason) => {
    const a = await migrated([build('odd'), legacy('control', {})])
    const listed = await a.store().queued()
    expect(listed.entries.map((entry) => entry.key)).toEqual(['control', 'odd'])
    const plan = planReconcileBatches(listed.entries)
    expect(plan.batches).toStrictEqual([{ keys: ['control'], occurrences: [wire('control')], isolated: false }])
    expect(plan.unsendable).toEqual([{ key: 'odd', reason }])
  })
})

describe('batch bound', () => {
  it('chunks valid rows by the request limit in queue order', async () => {
    const a = await migrated([legacy('a', {}), legacy('b', {}), legacy('c', {})])
    expect(planReconcileBatches((await a.store().queued()).entries, 2).batches.map((batch) => batch.keys)).toEqual([['a', 'b'], ['c']])
  })
})
