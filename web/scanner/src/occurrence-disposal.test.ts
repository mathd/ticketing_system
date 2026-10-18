import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { UnsupportedExportValue, prepareExport, summarizeDisposal, type ExportDocument } from './occurrence-export'
import { OCCURRENCE_DB_VERSION, STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { V2_STORES, rawGet, rawGetAll, rawPut, seedDatabase } from './test/raw-db'

// ST tier: held-row export, receipt and selected acknowledgement (ADR-085 §7.4,
// D25; TKT-558 D6, D15). The expected export document is written out literally,
// independent of the serializer.

let dbName: string
let instrument: Instrument | undefined
beforeEach(() => {
  dbName = `disp-${crypto.randomUUID()}`
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
})

const held = {
  a: { occurrenceId: 'h-a', qrPayload: 'qr-ha', occurredAt: '2026-10-01T10:00:00.111Z', state: 'PENDING', actuated: false, createdAt: '2026-10-01T10:00:00.112Z', ownerId: 'old', leaseExpiresAt: 5, direction: undefined },
  b: { occurrenceId: 'h-b', qrPayload: 'qr-hb', occurredAt: '2026-10-01T10:00:01.234Z', state: 'PENDING', actuated: false, createdAt: '2026-10-01T10:00:01.235Z', localDecision: 'revocation_refused' },
  c: { occurrenceId: 'h-c', state: 'MYSTERY', extra: { nested: [1, 'two'] } },
}
const queuedRows = {
  a: { occurrenceId: 'q-a', qrPayload: 'qr-qa', occurredAt: '2026-10-01T10:00:02.345Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-01T10:00:02.346Z' },
  b: { occurrenceId: 'q-b', qrPayload: 'qr-qb', state: 'QUEUED', actuated: false },
}
const actuated = { occurrenceId: 'o-a', qrPayload: 'qr-oa', occurredAt: '2026-10-01T10:00:03.000Z', state: 'ACTUATED', actuated: true, createdAt: '2026-10-01T10:00:03.001Z' }

const identity = (exportId: string) => ({ exportId, exportedAt: '2026-10-18T12:00:00.000Z', databaseName: dbName, databaseVersion: OCCURRENCE_DB_VERSION })
const value = (v: unknown) => ({ presence: 'value', value: v })
const absent = { presence: 'absent' }

function expectedDocument(): ExportDocument {
  return {
    schemaVersion: 1,
    exportId: 'export-1',
    exportedAt: '2026-10-18T12:00:00.000Z',
    source: { databaseName: dbName, databaseVersion: 3, legacyFormat: 0, currentFormat: 1 },
    held: [
      {
        occurrenceId: 'h-a', legacyClass: 'M01', holdReason: 'legacy_pending', format: 0, type: 'normal',
        fields: {
          actuated: value(false), createdAt: value('2026-10-01T10:00:00.112Z'), direction: { presence: 'undefined' },
          leaseExpiresAt: value(5), localDecision: absent, occurredAt: value('2026-10-01T10:00:00.111Z'),
          occurrenceId: value('h-a'), ownerId: value('old'), qrPayload: value('qr-ha'), result: absent, state: value('PENDING'),
        },
        disposal: { status: 'unresolved' },
      },
      {
        occurrenceId: 'h-b', legacyClass: 'M01', holdReason: 'legacy_pending', format: 0, type: 'refusal',
        fields: {
          actuated: value(false), createdAt: value('2026-10-01T10:00:01.235Z'), direction: absent,
          leaseExpiresAt: absent, localDecision: value('revocation_refused'), occurredAt: value('2026-10-01T10:00:01.234Z'),
          occurrenceId: value('h-b'), ownerId: absent, qrPayload: value('qr-hb'), result: absent, state: value('PENDING'),
        },
        disposal: { status: 'unresolved' },
      },
      {
        occurrenceId: 'h-c', legacyClass: 'unclassifiable', holdReason: 'legacy_unclassifiable', format: 0, type: 'unknown',
        fields: {
          actuated: absent, createdAt: absent, direction: absent, extra: value({ nested: [1, 'two'] }),
          leaseExpiresAt: absent, localDecision: absent, occurredAt: absent, occurrenceId: value('h-c'),
          ownerId: absent, qrPayload: absent, result: absent, state: value('MYSTERY'),
        },
        disposal: { status: 'unresolved' },
      },
    ],
    legacyQueued: [
      { occurrenceId: 'q-a', legacyClass: 'M02', state: value('QUEUED'), occurredAt: value('2026-10-01T10:00:02.345Z') },
      { occurrenceId: 'q-b', legacyClass: 'M02', state: value('QUEUED'), occurredAt: absent },
    ],
  } as ExportDocument
}

async function seededPage(rows: unknown[] = [...Object.values(held), ...Object.values(queuedRows), actuated]): Promise<TestPage> {
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: rows })
  const a = createTestPage({ dbName, ownerId: 'owner-a', renewalMs: 3_600_000 })
  await a.ready()
  return a
}

const unresolved = async (a: TestPage) => summarizeDisposal(await a.store().readDisposalSnapshot()).unresolvedCount

describe('held export', () => {
  it('produces the exact versioned document, with field presence and sub-second times (disposal-01, disposal-02, DX-02)', async () => {
    const a = await seededPage()
    const prepared = prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    expect(prepared.document).toStrictEqual(expectedDocument())
    expect(JSON.parse(prepared.contents)).toStrictEqual(expectedDocument())
    expect(prepared.receipt.held.map((entry) => entry.occurrenceId)).toEqual(['h-a', 'h-b', 'h-c'])
    expect(JSON.parse(prepared.receipt.held[0].snapshot)).toStrictEqual({ occurrenceId: 'h-a', fields: expectedDocument().held[0].fields })
    expect(summarizeDisposal(await a.store().readDisposalSnapshot())).toEqual({ heldCount: 3, unresolvedCount: 3, legacyQueuedCount: 2, exportAvailable: true })
  })

  it('fails visibly for a value it cannot represent exactly, with no receipt', async () => {
    const a = await seededPage([{ ...held.a, leaseExpiresAt: Number.NaN }])
    await expect(a.store().readDisposalSnapshot().then((snapshot) => prepareExport(snapshot, identity('x')))).rejects.toBeInstanceOf(UnsupportedExportValue)
    expect(await rawGetAll(dbName, STORE.exportReceipts)).toEqual([])
  })

  it('keeps exporting M02 ids at held 0, before and after sync retirement, with no acknowledgement (DX-01, PR1-4)', async () => {
    const a = await seededPage([queuedRows.a])
    const before = prepareExport(await a.store().readDisposalSnapshot(), identity('before'))
    expect([before.document.held, before.document.legacyQueued]).toEqual([[], [expectedDocument().legacyQueued[0]]])
    expect(summarizeDisposal(await a.store().readDisposalSnapshot())).toEqual({ heldCount: 0, unresolvedCount: 0, legacyQueuedCount: 1, exportAvailable: true })
    expect(await a.store().applyReconcileResults(['q-a'], [{ occurrence_id: 'q-a', result: 'conflict' }])).toEqual({ status: 'applied', retired: 1, alreadyApplied: 0 })
    const after = prepareExport(await a.store().readDisposalSnapshot(), identity('after'))
    expect(after.document.legacyQueued).toEqual([{ occurrenceId: 'q-a', legacyClass: 'M02', state: value('RESOLVED'), occurredAt: value('2026-10-01T10:00:02.345Z') }])
    expect(summarizeDisposal(await a.store().readDisposalSnapshot()).exportAvailable).toBe(true)
    await a.store().commitExportReceipt(after.receipt)
    expect(await a.store().acknowledgeHeld('after', ['q-a'])).toEqual({ status: 'refused', why: 'a selected record is not held' })
  })
})

describe('held acknowledgement', () => {
  it('acknowledges only the selected exported held rows, keeps every original and never syncs them (DX-05, DX-04, disposal-05)', async () => {
    const a = await seededPage()
    const prepared = prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    await a.store().commitExportReceipt(prepared.receipt)
    expect(await a.store().acknowledgeHeld('export-1', ['h-a'])).toEqual({ status: 'acknowledged', count: 1 })
    expect(await unresolved(a)).toBe(2)
    expect(await rawGet(dbName, STORE.heldDisposals, 'h-a')).toStrictEqual({
      occurrenceId: 'h-a', disposition: 'operator_acknowledged_without_sync', acknowledgedAt: expect.any(String), receiptId: 'export-1',
    })
    for (const row of Object.values(held)) expect(await rawGet(dbName, STORE.occurrences, row.occurrenceId)).toStrictEqual(row)
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual(['q-a', 'q-b'])
    // A repeated acknowledgement keeps the same ids and count.
    expect(await a.store().acknowledgeHeld('export-1', ['h-a'])).toEqual({ status: 'acknowledged', count: 1 })
    expect(await unresolved(a)).toBe(2)
    const again = prepareExport(await a.store().readDisposalSnapshot(), identity('export-2'))
    expect(again.document.held.map((entry) => [entry.occurrenceId, entry.disposal.status])).toEqual([['h-a', 'acknowledged'], ['h-b', 'unresolved'], ['h-c', 'unresolved']])
  })

  it('refuses acknowledgement without a committed covering receipt (disposal-03)', async () => {
    const a = await seededPage()
    prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    expect(await a.store().acknowledgeHeld('export-1', ['h-a'])).toEqual({ status: 'refused', why: 'no committed export receipt' })
    expect(await unresolved(a)).toBe(3)
    expect(await rawGetAll(dbName, STORE.heldDisposals)).toEqual([])
  })

  it.each([
    ['an unexported held id', ['h-a', 'h-b']],
    ['an M02 id', ['h-a', 'q-a']],
    ['a missing id', ['h-a', 'nope']],
    ['a retained other-class id', ['h-a', 'o-a']],
    ['a repeated id', ['h-a', 'h-a']],
  ])('refuses the whole selection with %s (DX-03)', async (_name, keys) => {
    const a = await seededPage()
    const snapshot = await a.store().readDisposalSnapshot()
    const onlyA = prepareExport({ ...snapshot, held: snapshot.held.filter((entry) => entry.key === 'h-a') }, identity('export-a'))
    await a.store().commitExportReceipt(onlyA.receipt)
    expect((await a.store().acknowledgeHeld('export-a', keys)).status).toBe('refused')
    expect(await unresolved(a)).toBe(3)
    expect(await rawGetAll(dbName, STORE.heldDisposals)).toEqual([])
  })

  it('refuses a held row that changed after its export', async () => {
    const a = await seededPage()
    const prepared = prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    await a.store().commitExportReceipt(prepared.receipt)
    await rawPut(dbName, STORE.occurrences, { ...held.a, qrPayload: 'changed' })
    expect(await a.store().acknowledgeHeld('export-1', ['h-a'])).toEqual({ status: 'refused', why: 'a selected record changed after the export' })
    expect(await unresolved(a)).toBe(3)
  })

  it.each(['abort', 'quota'] as const)('keeps the whole selected set unresolved when the acknowledgement fails by %s (disposal-04)', async (kind) => {
    const a = await seededPage()
    const prepared = prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    await a.store().commitExportReceipt(prepared.receipt)
    instrument = instrumentTransactions()
    instrument.inject(kind, { mode: 'readwrite', when: (call) => call.storeNames.includes(STORE.heldDisposals) }, { onStore: STORE.heldDisposals })
    await expect(a.store().acknowledgeHeld('export-1', ['h-a', 'h-b'])).rejects.toThrow()
    expect(await unresolved(a)).toBe(3)
    expect(await rawGetAll(dbName, STORE.heldDisposals)).toEqual([])
  })

  it('writes no receipt when the receipt transaction aborts', async () => {
    const a = await seededPage()
    const prepared = prepareExport(await a.store().readDisposalSnapshot(), identity('export-1'))
    instrument = instrumentTransactions()
    instrument.inject('abort', { stores: [STORE.exportReceipts], mode: 'readwrite' })
    await expect(a.store().commitExportReceipt(prepared.receipt)).rejects.toThrow()
    expect(await a.store().acknowledgeHeld('export-1', ['h-a'])).toEqual({ status: 'refused', why: 'no committed export receipt' })
  })
})
