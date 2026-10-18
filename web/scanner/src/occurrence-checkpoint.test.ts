import { afterEach, expect, it } from 'vitest'
import { prepareExport } from './occurrence-export'
import { readCurrentRecord } from './occurrence-model'
import { planReconcileBatches } from './reconcile-batch'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { rawGet, rawPut, seedDatabase, V2_STORES } from './test/raw-db'

let instrument: Instrument | undefined
afterEach(() => { instrument?.restore(); instrument = undefined; destroyAllPages() })
const T0 = 1_000_000
const DECIDED = '2026-10-18T09:00:05.000Z'

it.each(['input', 'latch', 'latched', 'view'] as const)('protects the first terminal decision from its %s alias through repair (C1-1)', async (alias) => {
  const dbName = `immutable-${crypto.randomUUID()}`
  const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0 })
  await a.ready()
  const { scan } = await mintOn(a)
  const proposed = { kind: 'terminal' as const, occurrenceId: scan.occurrenceId,
    outcome: { family: 'terminal' as const, cause: 'auth_refused' as const }, decidedAt: DECIDED }
  const latch = a.coordinator.latch(proposed)
  instrument = instrumentTransactions()
  instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
  expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
  const target = alias === 'input' ? proposed : alias === 'latch' ? latch
    : alias === 'latched' ? a.coordinator.latched(scan.occurrenceId)! : a.coordinator.view(scan.occurrenceId)!.final!
  // A caller can retain these objects even when its static type is read-only.
  Reflect.set(target, 'kind', 'queue')
  Reflect.set(target, 'outcome', { family: 'queue', cause: 'transport_failure' })
  Reflect.set(Reflect.get(target, 'outcome'), 'cause', 'transport_failure')
  const capture = a.coordinator.view(scan.occurrenceId)!.captured
  Reflect.set(capture, 'qrPayload', 'changed-credential')
  expect(a.coordinator.view(scan.occurrenceId)!.captured.qrPayload).toBe(scan.qrPayload)
  await a.coordinator.repairAll()
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({
    occurrenceId: scan.occurrenceId, qrPayload: scan.qrPayload, occurredAt: scan.occurredAt, createdAt: scan.createdAt,
    state: 'RESOLVED', actuated: false, ownerId: 'owner-a', leaseExpiresAt: T0 + 30_000, recordFormat: 1,
    direction: 'entry', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: DECIDED,
  })
  expect((await a.store().queued()).entries).toEqual([])
})

it('commits pre-send refusal cancellation and excludes it after page loss (C1-2)', async () => {
  const dbName = `cancel-refusal-${crypto.randomUUID()}`
  const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0 })
  await a.ready()
  const { scan } = await mintOn(a, { type: 'refusal' })
  a.coordinator.latch({ kind: 'terminal', occurrenceId: scan.occurrenceId,
    outcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: false }, decidedAt: DECIDED })
  expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('committed')
  const expected = { occurrenceId: scan.occurrenceId, qrPayload: scan.qrPayload, occurredAt: scan.occurredAt,
    createdAt: scan.createdAt, state: 'RESOLVED', actuated: false, ownerId: 'owner-a', leaseExpiresAt: T0 + 30_000,
    recordFormat: 1, localDecision: 'revocation_refused',
    localOutcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: false }, decidedAt: DECIDED }
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expected)
  expect(readCurrentRecord(expected).ok).toBe(true)
  a.destroy()
  const b = createTestPage({ dbName, ownerId: 'owner-b', now: () => T0 + 60_000 })
  await b.ready()
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expected)
  expect((await b.store().queued()).entries).toEqual([])
})

it.each(['top', 'nested'] as const)('exports and binds a literal own __proto__ field at the %s level (C1-3)', async (level) => {
  const dbName = `proto-${crypto.randomUUID()}`
  const literal = JSON.parse('{"occurrenceId":"held","state":"UNKNOWN","__proto__":{"original":"top"},"extra":{"__proto__":{"original":"nested"}}}')
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: [literal] })
  const a = createTestPage({ dbName })
  await a.ready()
  const prepared = prepareExport(await a.store().readDisposalSnapshot(), {
    exportId: 'receipt', exportedAt: DECIDED, databaseName: dbName, databaseVersion: 3,
  })
  const fields = JSON.parse(prepared.contents).held[0].fields
  expect(Object.hasOwn(fields, '__proto__')).toBe(true)
  expect(fields.__proto__).toStrictEqual({ presence: 'value', value: { original: 'top' } })
  expect(fields.extra).toStrictEqual(JSON.parse('{"presence":"value","value":{"__proto__":{"original":"nested"}}}'))
  await a.store().commitExportReceipt(prepared.receipt)
  const changed = JSON.parse(level === 'top'
    ? '{"occurrenceId":"held","state":"UNKNOWN","__proto__":{"original":"changed"},"extra":{"__proto__":{"original":"nested"}}}'
    : '{"occurrenceId":"held","state":"UNKNOWN","__proto__":{"original":"top"},"extra":{"__proto__":{"original":"changed"}}}')
  await rawPut(dbName, STORE.occurrences, changed)
  expect(await a.store().acknowledgeHeld('receipt', ['held'])).toMatchObject({ status: 'refused' })
  expect((await a.store().readDisposalSnapshot()).held[0].disposal).toBeUndefined()
})

it.each(['object', 'array'] as const)('keeps a cyclic legacy %s local while planning valid peers and shared acyclic values (C1-4)', async (kind) => {
  const dbName = `cycle-${crypto.randomUUID()}`
  const cyclic: Record<string, unknown> | unknown[] = kind === 'object' ? {} : []
  if (Array.isArray(cyclic)) cyclic.push(cyclic)
  else cyclic.self = cyclic
  const shared = { value: 'shared' }
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: [
    { occurrenceId: 'bad', occurredAt: DECIDED, qrPayload: cyclic, state: 'QUEUED' },
    { occurrenceId: 'control', occurredAt: DECIDED, qrPayload: 'credential', state: 'QUEUED' },
    { occurrenceId: 'shared', occurredAt: DECIDED, qrPayload: [shared, shared], state: 'QUEUED' },
  ] })
  const a = createTestPage({ dbName })
  await a.ready()
  expect(planReconcileBatches((await a.store().queued()).entries)).toStrictEqual({
    unsendable: [{ key: 'bad', reason: 'qrPayload cannot be sent exactly' }],
    batches: [
      { keys: ['control'], occurrences: [{ occurrence_id: 'control', occurred_at: DECIDED, qr_payload: 'credential' }], isolated: false },
      { keys: ['shared'], occurrences: [{ occurrence_id: 'shared', occurred_at: DECIDED, qr_payload: [{ value: 'shared' }, { value: 'shared' }] }], isolated: true },
    ],
  })
  expect((await rawGet(dbName, STORE.occurrences, 'bad') as Record<string, unknown>).state).toBe('QUEUED')
})
