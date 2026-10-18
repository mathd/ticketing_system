import { afterEach, expect, it } from 'vitest'
import { prepareExport, UnsupportedExportValue } from './occurrence-export'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { captured, createTestPage, destroyAllPages } from './test/page-fixture'
import { rawGet, seedDatabase, V2_STORES } from './test/raw-db'

let instrument: Instrument | undefined
afterEach(() => { instrument?.restore(); instrument = undefined; destroyAllPages() })
const DECIDED = '2026-10-18T09:00:05.000Z'

it.each(['missing', 'aborted'] as const)('refuses marking when the occurrence is %s without throwing (C2I-2)', async (state) => {
  const page = createTestPage({ dbName: `mark-${crypto.randomUUID()}` })
  await page.ready()
  const scan = captured()
  if (state === 'aborted') {
    page.coordinator.register(scan)
    instrument = instrumentTransactions()
    instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
    await expect(page.store().mintNormal(scan.occurrenceId)).rejects.toBeDefined()
    expect(page.coordinator.view(scan.occurrenceId)).toBeUndefined()
  }
  expect(page.coordinator.beginMarking(scan.occurrenceId)).toBe(false)
  expect(page.coordinator.canReserve(scan.occurrenceId)).toBe(false)
  expect(page.coordinator.pendingTargets()).toEqual([])
})

it.each(['missing', 'aborted'] as const)('refuses denial enrichment when the occurrence is %s without throwing (C2I-1)', async (state) => {
  const page = createTestPage({ dbName: `reason-${crypto.randomUUID()}` })
  await page.ready()
  const scan = captured()
  if (state === 'aborted') {
    page.coordinator.register(scan)
    instrument = instrumentTransactions()
    instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
    await expect(page.store().mintNormal(scan.occurrenceId)).rejects.toBeDefined()
    expect(page.coordinator.view(scan.occurrenceId)).toBeUndefined()
    page.coordinator.latch({ kind: 'terminal', occurrenceId: scan.occurrenceId,
      outcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: false }, decidedAt: DECIDED })
  }
  expect(page.coordinator.captureDenialReason(scan.occurrenceId, 'already_redeemed')).toBe(false)
  expect(page.coordinator.pendingTargets()).toEqual([])
})

it.each(['object', 'array'] as const)('rejects a held %s cycle with the export error and no receipt (C2I-3)', async (kind) => {
  const dbName = `held-cycle-${crypto.randomUUID()}`
  const cyclic: Record<string, unknown> | unknown[] = kind === 'object' ? {} : []
  if (Array.isArray(cyclic)) cyclic.push(cyclic)
  else cyclic.self = cyclic
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: [
    { occurrenceId: 'held', state: 'UNKNOWN', extra: cyclic },
  ] })
  const page = createTestPage({ dbName })
  await page.ready()
  const snapshot = await page.store().readDisposalSnapshot()
  expect(() => prepareExport(snapshot, { exportId: 'receipt', exportedAt: DECIDED,
    databaseName: dbName, databaseVersion: 3 })).toThrow(UnsupportedExportValue)
  expect(await rawGet(dbName, STORE.exportReceipts, 'receipt')).toBeUndefined()
  expect((await page.store().readDisposalSnapshot()).held[0].disposal).toBeUndefined()
})

it('exports shared acyclic held values exactly (C2I-3 control)', async () => {
  const dbName = `held-alias-${crypto.randomUUID()}`
  const shared = { original: 'credential' }
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: [
    { occurrenceId: 'held', state: 'UNKNOWN', extra: [shared, shared] },
  ] })
  const page = createTestPage({ dbName })
  await page.ready()
  const prepared = prepareExport(await page.store().readDisposalSnapshot(), {
    exportId: 'receipt', exportedAt: DECIDED, databaseName: dbName, databaseVersion: 3,
  })
  expect(JSON.parse(prepared.contents).held[0].fields.extra).toStrictEqual({
    presence: 'value', value: [{ original: 'credential' }, { original: 'credential' }],
  })
  await page.store().commitExportReceipt(prepared.receipt)
  expect(await page.store().acknowledgeHeld('receipt', ['held'])).toMatchObject({ status: 'acknowledged' })
})
