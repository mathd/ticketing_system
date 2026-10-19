import { afterEach, expect, it } from 'vitest'
import { forceCloseDatabase } from 'fake-indexeddb'
import type { LocalOutcome } from './occurrence-model'
import { STORE } from './occurrence-store'
import { planReconcileBatches } from './reconcile-batch'
import { instrumentTransactions } from './test/idb-instrument'
import { createTestPage, destroyAllPages, eventually, mintOn } from './test/page-fixture'
import { rawGet } from './test/raw-db'

// These store cells start after classification. App and controller tests prove
// the producer and full instruction. Here the production owner, write, recovery
// and serializer must preserve that exact Exit through each storage column.
afterEach(() => destroyAllPages())
const time = '2026-10-19T12:00:00.123Z'
const rows: Array<{ address: string; outcome: LocalOutcome; state: string }> = [
  { address: 'O01-X', outcome: { family: 'queue', cause: 'transport_failure' }, state: 'QUEUED' },
  ...([502, 503, 504] as const).map(status => ({ address: `O02-X/${status}`, outcome: { family: 'queue' as const, cause: 'server_unavailable' as const, status }, state: 'QUEUED' })),
  { address: 'L02-X', outcome: { family: 'terminal', cause: 'stopped', checkpoint: 'send', reason: 'deadline', requestMayHaveBeenSent: false }, state: 'RESOLVED' },
  { address: 'L03-X', outcome: { family: 'terminal', cause: 'stopped', checkpoint: 'decision', reason: 'deadline', requestMayHaveBeenSent: true }, state: 'RESOLVED' },
  { address: 'L05-X', outcome: { family: 'terminal', cause: 'stopped', checkpoint: 'decision', reason: 'clock', requestMayHaveBeenSent: true }, state: 'RESOLVED' },
  ...([false, true] as const).map(sent => ({ address: `C01-X/${sent}`, outcome: { family: 'terminal' as const, cause: 'cancelled' as const, requestMayHaveBeenSent: sent }, state: 'RESOLVED' })),
  { address: 'S02-X/committed-unconfirmed', outcome: { family: 'terminal', cause: 'mint_unconfirmed' }, state: 'RESOLVED' },
]
const columns = ['S', 'F-throw', 'F-abort', 'C', 'T', 'V', 'P-committed', 'P-uncommitted'] as const
it.each(rows.flatMap(row => columns.map(column => ({ ...row, column }))))('$address / $column keeps the captured row and original eligibility', async ({ outcome, state, column }) => {
  const dbName = `exit-storage-cell-${crypto.randomUUID()}`
  let now = 1000
  const a = createTestPage({ dbName, now: () => now, renewalMs: 3600000 }); await a.ready()
  const target = await mintOn(a, { direction: 'exit' })
  const initial = await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId) as Record<string, unknown>
  const control = await mintOn(a, { direction: 'entry' })
  a.coordinator.latch({ kind: 'queue', occurrenceId: control.scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: time })
  await a.coordinator.persist(control.scan.occurrenceId)
  const decision = a.coordinator.latch({ kind: outcome.family === 'queue' ? 'queue' : 'terminal', occurrenceId: target.scan.occurrenceId, outcome, decidedAt: time } as Parameters<typeof a.coordinator.latch>[0])
  const expected = { ...initial, state, localOutcome: outcome, decidedAt: time }
  const failed = !['S', 'P-committed'].includes(column)
  const instrument = instrumentTransactions()
  try {
    if (failed) instrument.inject(column === 'F-abort' ? 'abort' : 'throw', { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' })
    expect((await a.coordinator.persist(target.scan.occurrenceId)).durable).toBe(failed ? 'failed' : 'committed')
  } finally { instrument.restore() }
  expect(await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)).toStrictEqual(failed ? initial : expected)
  if (column === 'C') {
    const owner = a.coordinator.ownerId
    forceCloseDatabase(a.store().db as never); await a.waitFor('failed')
    a.storage.retry(); await a.waitFor('ready')
    expect(a.coordinator.ownerId).toBe(owner)
    expect(a.coordinator.latched(target.scan.occurrenceId)).toBe(decision)
    await a.coordinator.repairAll()
  }
  if (column === 'V') {
    a.store().db.dispatchEvent(new IDBVersionChangeEvent('versionchange', { oldVersion: 3, newVersion: 4 }))
    await a.waitFor('closed')
    expect(await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)).toStrictEqual(expected)
    a.storage.retry(); await a.waitFor('ready')
  }
  let observer = a
  if (column === 'T' || column.startsWith('P-')) {
    if (column.startsWith('P-')) a.destroy()
    now = 40000
    observer = createTestPage({ dbName, now: () => now }); await observer.ready()
    const after = await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)
    expect(after).toStrictEqual(column === 'T' ? initial : column === 'P-committed' ? expected
      : { ...initial, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } })
    if (column === 'P-uncommitted') expect(await observer.store().readNotices()).toContainEqual({ occurrenceId: target.scan.occurrenceId, occurredAt: target.scan.occurredAt, type: 'normal', direction: 'exit', cause: 'uncertain_recovery', syncState: 'QUEUED' })
  }
  const pendingProtected = column === 'T' || column === 'F-throw' || column === 'F-abort'
  const eligible = column === 'P-uncommitted' || outcome.family === 'queue' && !pendingProtected
  const plan = planReconcileBatches((await observer.store().queued()).entries)
  const wire = plan.batches.flatMap(batch => batch.occurrences)
  expect(wire).toContainEqual({ occurrence_id: control.scan.occurrenceId, qr_payload: control.scan.qrPayload, occurred_at: control.scan.occurredAt, event_type: 'entry' })
  const expectedWire = { occurrence_id: target.scan.occurrenceId, qr_payload: target.scan.qrPayload, occurred_at: target.scan.occurredAt, event_type: 'exit' }
  if (eligible) expect(wire).toContainEqual(expectedWire)
  else expect(wire.map(row => row.occurrence_id)).not.toContain(target.scan.occurrenceId)
  if (!a.destroyed) {
    await a.coordinator.repairAll()
    expect(await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)).toStrictEqual(expected)
    expect(a.coordinator.latched(target.scan.occurrenceId)).toBe(decision)
  }
})

it.each(['S', 'C', 'T', 'V', 'P'])('committed Exit mark / %s stays intact without creating a queue', async column => {
  const dbName = `exit-act-cell-${crypto.randomUUID()}`
  let now = 1000
  const a = createTestPage({ dbName, now: () => now }); await a.ready()
  const target = await mintOn(a, { direction: 'exit' })
  a.coordinator.beginMarking(target.scan.occurrenceId)
  expect(await a.store().reserveActuation(target.scan.occurrenceId)).toEqual({ reserved: true })
  const marked = await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)
  a.coordinator.latch({ kind: 'queue', occurrenceId: target.scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: time })
  await a.coordinator.repairAll()
  if (column === 'C') { forceCloseDatabase(a.store().db as never); await a.waitFor('failed'); a.storage.retry(); await a.waitFor('ready') }
  if (column === 'V') { a.store().db.dispatchEvent(new IDBVersionChangeEvent('versionchange', { oldVersion: 3, newVersion: 4 })); await a.waitFor('closed'); a.storage.retry(); await a.waitFor('ready') }
  if (column === 'P') a.destroy()
  now = 40000
  const b = createTestPage({ dbName, now: () => now }); await b.ready()
  expect(await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)).toStrictEqual(marked)
  expect(planReconcileBatches((await b.store().queued()).entries).batches).toEqual([])
})

it('refuses an actual late queue repair after an in-flight Exit mark commits', async () => {
  const dbName = `exit-late-queue-${crypto.randomUUID()}`
  const a = createTestPage({ dbName }); await a.ready()
  const target = await mintOn(a, { direction: 'exit' })
  const instrument = instrumentTransactions()
  const held = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' })
  try {
    a.coordinator.beginMarking(target.scan.occurrenceId)
    const marking = a.store().reserveActuation(target.scan.occurrenceId)
    const transaction = await held.started
    await eventually(() => transaction.events.includes('write-success'), 'Exit mark write before its commit')
    const late = a.coordinator.latch({ kind: 'queue', occurrenceId: target.scan.occurrenceId,
      outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: time })
    held.release(); expect(await marking).toEqual({ reserved: true })
    const marked = await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)
    expect(await a.store().persistDecision(late)).toMatchObject({ status: 'ineligible' })
    expect(await rawGet(dbName, STORE.occurrences, target.scan.occurrenceId)).toStrictEqual(marked)
  } finally { held.release(); instrument.restore() }
})
