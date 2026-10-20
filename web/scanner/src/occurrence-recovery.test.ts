import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ProposedDecision } from './occurrence-coordinator'
import { ownerLockName } from './occurrence-lock'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { captured, createTestPage, destroyAllPages, lockAvailable, mintOn, type TestPage } from './test/page-fixture'
import { rawGet } from './test/raw-db'

// ST+LK tier: dead-owner recovery, its notice and the page owner lock. Page loss
// is only ever the fixture's destroy(); a connection close is not page loss (I8).

let dbName: string
let clock: number
let instrument: Instrument | undefined
const T0 = 1_000_000
const EXPIRED = T0 + 60_000
const BEFORE_EXPIRY = T0 + 10_000

beforeEach(() => {
  dbName = `rec-${crypto.randomUUID()}`
  clock = T0
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
  vi.unstubAllGlobals()
})

const page = (ownerId: string, recoverOwnerId?: string) =>
  createTestPage({ dbName, ownerId, recoverOwnerId, now: () => clock, renewalMs: 3_600_000 })

function pendingRow(scan: ReturnType<typeof captured>, ownerId: string) {
  const base = {
    occurrenceId: scan.occurrenceId,
    qrPayload: scan.qrPayload,
    occurredAt: scan.occurredAt,
    createdAt: scan.createdAt,
    state: 'PENDING',
    actuated: false,
    ownerId,
    leaseExpiresAt: T0 + 30_000,
    recordFormat: 1,
  }
  return scan.type === 'refusal' ? { ...base, localDecision: 'revocation_refused' } : { ...base, direction: 'entry' }
}
const recovered = (scan: ReturnType<typeof captured>, ownerId: string) => ({
  ...pendingRow(scan, ownerId),
  state: 'QUEUED',
  localOutcome: { family: 'queue', cause: 'uncertain_recovery' },
})
const decided = '2026-10-18T09:00:05.000Z'
const terminal = (occurrenceId: string): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome: { family: 'terminal', cause: 'denied', status: 409, latchId: 'l1', reason: 'refunded' }, decidedAt: decided })
const OCC_RW_STRICT = { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' } as const

async function deadPage(mints: Array<Partial<ReturnType<typeof captured>>>) {
  const a = page('owner-a')
  await a.ready()
  const scans = []
  for (const overrides of mints) scans.push((await mintOn(a, overrides)).scan)
  return { a, scans }
}

/** A live page whose final write failed: its row stays PENDING and its lock stays held. */
async function failedTerminalWrite(a: TestPage) {
  const { scan } = await mintOn(a)
  instrument = instrumentTransactions()
  instrument.inject('abort', OCC_RW_STRICT)
  a.coordinator.latch(terminal(scan.occurrenceId))
  expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
  instrument.restore()
  instrument = undefined
  return scan
}

describe('dead-owner recovery', () => {
  it('recovers a dead page\'s normal and refusal PENDING rows with type, id and time, plus notices (I8-07, I8-05, I8-04)', async () => {
    const { a, scans: [normal, refusal] } = await deadPage([{}, { type: 'refusal' }])
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.recovered.sort()).toEqual([normal.occurrenceId, refusal.occurrenceId].sort())
    expect(await rawGet(dbName, STORE.occurrences, normal.occurrenceId)).toStrictEqual(recovered(normal, 'owner-a'))
    expect(await rawGet(dbName, STORE.occurrences, refusal.occurrenceId)).toStrictEqual(recovered(refusal, 'owner-a'))
    const notices = (await b.store().readNotices()).sort((x, y) => x.occurrenceId.localeCompare(y.occurrenceId))
    expect(notices).toEqual([
      { occurrenceId: normal.occurrenceId, occurredAt: normal.occurredAt, type: 'normal', direction: 'entry', cause: 'uncertain_recovery', syncState: 'QUEUED' },
      { occurrenceId: refusal.occurrenceId, occurredAt: refusal.occurredAt, type: 'refusal', cause: 'uncertain_recovery', syncState: 'QUEUED' },
    ].sort((x, y) => x.occurrenceId.localeCompare(y.occurrenceId)))
    expect(await rawGet(dbName, STORE.recoveryNotices, normal.occurrenceId)).toStrictEqual({
      occurrenceId: normal.occurrenceId, cause: 'uncertain_recovery', recoveredAt: new Date(EXPIRED).toISOString(),
    })
  })

  it('does not recover a dead page before lease expiry without a reload relation', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = BEFORE_EXPIRY
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery).toEqual({ recovered: [], liveOwner: [], notEligible: [scan.occurrenceId], failed: [] })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan, 'owner-a'))
  })

  it('lets a genuine reload predecessor bypass the lease only after its lock is absent', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = BEFORE_EXPIRY
    const reloaded = page('owner-b', 'owner-a')
    await reloaded.ready()
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(recovered(scan, 'owner-a'))
  })

  it.each([
    ['an expired lease (I2-08)', EXPIRED, undefined],
    ['a reload relation (I8-03)', BEFORE_EXPIRY, 'owner-a'],
    ['a reload relation and an expired lease (I8-03)', EXPIRED, 'owner-a'],
  ])('refuses a live owner\'s row despite %s', async (_name, at, recoverOwnerId) => {
    const { scans: [scan] } = await deadPage([{}])
    clock = at
    const b = page('owner-b', recoverOwnerId)
    await b.ready()
    expect(b.storage.initialReads()?.recovery.liveOwner).toEqual([scan.occurrenceId])
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan, 'owner-a'))
    expect(await b.store().readNotices()).toEqual([])
  })

  it('never recovers committed TERM or ACT rows; the PENDING control recovers (I8-02a, I8-02b)', async () => {
    const a = page('owner-a')
    await a.ready()
    const term = await mintOn(a)
    a.coordinator.latch(terminal(term.scan.occurrenceId))
    await a.coordinator.persist(term.scan.occurrenceId)
    const act = await mintOn(a)
    a.coordinator.beginMarking(act.scan.occurrenceId)
    await a.store().reserveActuation(act.scan.occurrenceId)
    const control = await mintOn(a)
    const termRow = await rawGet(dbName, STORE.occurrences, term.scan.occurrenceId)
    const actRow = await rawGet(dbName, STORE.occurrences, act.scan.occurrenceId)
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.recovered).toEqual([control.scan.occurrenceId])
    expect(await rawGet(dbName, STORE.occurrences, term.scan.occurrenceId)).toStrictEqual(termRow)
    expect(await rawGet(dbName, STORE.occurrences, act.scan.occurrenceId)).toStrictEqual(actRow)
    expect((await b.store().readNotices()).map((notice) => notice.occurrenceId)).toEqual([control.scan.occurrenceId])
    expect((await b.store().queued()).entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
  })

  it('pins the lost-terminal-write residual with the PENDING row a failed write left (I8-13)', async () => {
    const a = page('owner-a')
    await a.ready()
    const scan = await failedTerminalWrite(a)
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan, 'owner-a'))
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    // Accepted residual (TKT-399 D30): the lost denial recovers as an uncertain queue fact.
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(recovered(scan, 'owner-a'))
    expect((await b.store().readNotices()).map((notice) => notice.occurrenceId)).toEqual([scan.occurrenceId])
  })
})

describe('recovery notices', () => {
  it('keeps the notice through sync retirement until its own acknowledgement, across pages (I8-09)', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(await b.store().applyReconcileResults([scan.occurrenceId], [{ occurrence_id: scan.occurrenceId, result: 'recorded' }]))
      .toEqual({ status: 'applied', retired: 1, alreadyApplied: 0 })
    expect(await b.store().readNotices()).toEqual([
      { occurrenceId: scan.occurrenceId, occurredAt: scan.occurredAt, type: 'normal', direction: 'entry', cause: 'uncertain_recovery', syncState: 'SYNCED' },
    ])
    const c = page('owner-c')
    await c.ready()
    expect((await c.store().readNotices()).map((notice) => notice.syncState)).toEqual(['SYNCED'])
    expect(await c.store().acknowledgeNotice(scan.occurrenceId)).toBe('acknowledged')
    expect(await c.store().acknowledgeNotice(scan.occurrenceId)).toBe('alreadySame')
    expect(await b.store().readNotices()).toEqual([])
    const d = page('owner-d')
    await d.ready()
    expect(await d.store().readNotices()).toEqual([])
    expect((await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>).state).toBe('SYNCED')
  })

  it('queues nothing when the notice cannot commit in the same transaction (I8-10)', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = EXPIRED
    instrument = instrumentTransactions()
    instrument.inject('quota', { mode: 'readwrite', when: (call) => call.storeNames.includes(STORE.recoveryNotices) }, { onStore: STORE.recoveryNotices })
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.failed.map((failure) => failure.occurrenceId)).toEqual([scan.occurrenceId])
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan, 'owner-a'))
    expect(await rawGet(dbName, STORE.recoveryNotices, scan.occurrenceId)).toBeUndefined()
    expect((await b.store().queued()).entries).toEqual([])
  })

  it('creates the notice for ordinary foreign-expired recovery, and a later page shows it (I8-12)', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    const c = page('owner-c')
    await c.ready()
    expect((await c.store().readNotices()).map((notice) => notice.occurrenceId)).toEqual([scan.occurrenceId])
  })

  it('keeps the notice when its acknowledgement aborts', async () => {
    const { a, scans: [scan] } = await deadPage([{}])
    a.destroy()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    instrument = instrumentTransactions()
    instrument.inject('abort', { stores: [STORE.recoveryNotices], mode: 'readwrite' })
    await expect(b.store().acknowledgeNotice(scan.occurrenceId)).rejects.toThrow()
    expect((await b.store().readNotices()).map((notice) => notice.occurrenceId)).toEqual([scan.occurrenceId])
  })
})

describe('page owner lock (I1-03, I2-02, I2-06, I8-01)', () => {
  it('keeps the lock after a failed final write, so expiry does not let another tab recover (I1-03, I2-06)', async () => {
    const a = page('owner-a')
    await a.ready()
    const scan = await failedTerminalWrite(a)
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.liveOwner).toEqual([scan.occurrenceId])
    expect(await lockAvailable(ownerLockName('owner-a'))).toBe(false)
    // The same page repairs the same target later.
    expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('committed')
    expect((await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>).state).toBe('RESOLVED')
  })

  it('treats a closed connection as a live page; only page destruction allows recovery (I2-02, I8-01)', async () => {
    const a = page('owner-a')
    await a.ready()
    const { scan } = await mintOn(a)
    a.storage.stop()
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.liveOwner).toEqual([scan.occurrenceId])
    // Reopen on the same page: same owner, lock still held.
    a.storage.retry()
    await a.waitFor('ready')
    expect(a.coordinator.ownerId).toBe('owner-a')
    expect((await b.store().recoverDeadOwners()).liveOwner).toEqual([scan.occurrenceId])
    a.destroy()
    expect((await b.store().recoverDeadOwners()).recovered).toEqual([scan.occurrenceId])
  })

  it('holds the production page lock through the production wiring (I2-09)', async () => {
    vi.stubGlobal('indexedDB', new IDBFactory())
    vi.resetModules()
    const production = await import('./occurrence-readiness')
    const { pageStorage } = production
    const coordinator = pageStorage.coordinator
    pageStorage.start()
    try {
      await vi.waitFor(() => expect(pageStorage.state().status).toBe('ready'), { timeout: 2_000 })
      expect(await lockAvailable(ownerLockName(coordinator.ownerId))).toBe(false)
      const scan = captured()
      coordinator.register(scan)
      await pageStorage.readyStore()!.mintNormal(scan.occurrenceId)
      instrument = instrumentTransactions()
      instrument.inject('abort', OCC_RW_STRICT)
      coordinator.latch(terminal(scan.occurrenceId))
      expect((await coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
      instrument.restore()
      instrument = undefined
      clock = Date.now() + 120_000
      const b = createTestPage({ dbName: 'gate-occurrences', ownerId: 'owner-b', now: () => clock, renewalMs: 3_600_000 })
      await b.ready()
      expect(b.storage.initialReads()?.recovery.liveOwner).toEqual([scan.occurrenceId])
      expect(await lockAvailable(ownerLockName(coordinator.ownerId))).toBe(false)
    } finally {
      pageStorage.stop()
    }
  })
})
