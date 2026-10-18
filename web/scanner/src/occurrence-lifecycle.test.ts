import { forceCloseDatabase } from 'fake-indexeddb'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { ProposedDecision } from './occurrence-coordinator'
import { ownerLockName } from './occurrence-lock'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { bounded, createTestPage, destroyAllPages, eventually, lockAvailable, mintOn, settle, type TestPage } from './test/page-fixture'
import { rawGet, rawGetAll } from './test/raw-db'

// ST tier: connection lifecycle. A version change repairs on the old connection
// before it closes (TKT-558 D8, ADR-085 D27). An unexpected close fails only its
// own generation. A reconnect keeps the page coordinator, owner and lock.

let dbName: string
let clock: number
let instrument: Instrument | undefined
const T0 = 1_000_000
const EXPIRED = T0 + 60_000

beforeEach(() => {
  dbName = `life-${crypto.randomUUID()}`
  clock = T0
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
})

const page = (ownerId: string) => createTestPage({ dbName, ownerId, now: () => clock, renewalMs: 3_600_000 })
const decided = '2026-10-18T09:00:05.000Z'
const denial = { family: 'terminal', cause: 'denied', status: 422, latchId: 'l1', reason: 'invalid_credential' } as const
const offline = { family: 'queue', cause: 'server_unavailable', status: 503 } as const
const OCC_RW_STRICT = { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' } as const

type Targets = { term: string; queue: string; act: string; actRow: unknown }

/** Two failed final writes (TERM and QUEUE) and a committed ACT control, on a live page. */
async function failedTargets(a: TestPage): Promise<Targets> {
  const term = await mintOn(a)
  const queue = await mintOn(a)
  const act = await mintOn(a)
  a.coordinator.beginMarking(act.scan.occurrenceId)
  expect(await a.store().reserveActuation(act.scan.occurrenceId)).toEqual({ reserved: true })
  const failing = instrumentTransactions()
  failing.inject('abort', OCC_RW_STRICT)
  failing.inject('throw', OCC_RW_STRICT)
  a.coordinator.latch({ kind: 'terminal', occurrenceId: term.scan.occurrenceId, outcome: denial, decidedAt: decided } as ProposedDecision)
  a.coordinator.latch({ kind: 'queue', occurrenceId: queue.scan.occurrenceId, outcome: offline, decidedAt: decided } as ProposedDecision)
  expect((await a.coordinator.persist(term.scan.occurrenceId)).durable).toBe('failed')
  expect((await a.coordinator.persist(queue.scan.occurrenceId)).durable).toBe('failed')
  failing.restore()
  return {
    term: term.scan.occurrenceId,
    queue: queue.scan.occurrenceId,
    act: act.scan.occurrenceId,
    actRow: await rawGet(dbName, STORE.occurrences, act.scan.occurrenceId),
  }
}

/** Requests version 4 and aborts that upgrade, so the database stays at version 3 with its rows. */
function versionChangeThenAbortUpgrade(): Promise<void> {
  return new Promise((resolve) => {
    const request = indexedDB.open(dbName, 4)
    request.onupgradeneeded = () => request.transaction!.abort()
    request.onerror = () => resolve()
    request.onsuccess = () => { request.result.close(); resolve() }
  })
}

/** fake-indexeddb's forced close: emits close, and lets an active transaction finish first. */
const forceClose = (db: IDBDatabase) => forceCloseDatabase(db as never)

const state = async (id: string) => (await rawGet(dbName, STORE.occurrences, id) as Record<string, unknown> | undefined)?.state

describe('version change on the old connection', () => {
  it('attempts every pending final write before close and keeps ACT (I2-10, I2-11)', async () => {
    const a = page('owner-a')
    await a.ready()
    const targets = await failedTargets(a)
    instrument = instrumentTransactions()
    await bounded(versionChangeThenAbortUpgrade(), 'version change')
    await a.waitFor('closed')
    expect(a.storage.state()).toMatchObject({ status: 'closed', cause: 'versionchange' })
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['RESOLVED', 'QUEUED'])
    expect(await rawGet(dbName, STORE.occurrences, targets.act)).toStrictEqual(targets.actRow)
    const repairs = instrument.calls
      .map((call, index) => ({ call, index }))
      .filter(({ call }) => call.mode === 'readwrite' && call.options?.durability === 'strict')
    expect(repairs).toHaveLength(2)
    const closeAt = instrument.log.indexOf('close v3')
    expect(closeAt).toBeGreaterThan(-1)
    for (const { index } of repairs) expect(instrument.log.indexOf(`complete#${index}`)).toBeLessThan(closeAt)
    expect(a.coordinator.pendingTargets()).toEqual([])
  })

  it('keeps the owner lock and both targets when the old-connection repairs abort (I2-03, I2-12)', async () => {
    const a = page('owner-a')
    await a.ready()
    const targets = await failedTargets(a)
    instrument = instrumentTransactions()
    instrument.inject('abort', OCC_RW_STRICT)
    instrument.inject('abort', OCC_RW_STRICT)
    await bounded(versionChangeThenAbortUpgrade(), 'version change')
    await a.waitFor('closed')
    // The test's own aborted version-4 upgrade is a versionchange transaction; count only repairs.
    const aborts = instrument.calls.filter((call) => call.mode === 'readwrite' && call.events.includes('abort'))
    expect(aborts).toHaveLength(2)
    instrument.restore()
    instrument = undefined
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['PENDING', 'PENDING'])
    expect(a.coordinator.pendingTargets().sort()).toEqual([targets.term, targets.queue].sort())
    expect(await lockAvailable(ownerLockName('owner-a'))).toBe(false)
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.liveOwner.sort()).toEqual([targets.term, targets.queue].sort())
    expect((await b.store().queued()).entries).toEqual([])
    // The same page repairs the same targets through its next compatible connection.
    a.storage.retry()
    await a.waitFor('ready')
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['RESOLVED', 'QUEUED'])
  })
})

describe('unexpected close (PR1-3)', () => {
  it('fails only its generation when idle, then reconnects with the same owner and lock', async () => {
    const a = page('owner-a')
    const first = await a.ready()
    forceClose(first.db)
    await a.waitFor('failed')
    expect(a.storage.state()).toMatchObject({ status: 'failed', cause: 'unexpected_close' })
    expect(a.storage.readyStore()).toBeNull()
    a.storage.retry()
    await a.waitFor('ready')
    expect(a.storage.readyStore()).not.toBe(first)
    expect(await lockAvailable(ownerLockName('owner-a'))).toBe(false)
  })

  it('keeps a failed target protected across a forced close and repairs it through the new connection', async () => {
    const a = page('owner-a')
    await a.ready()
    const targets = await failedTargets(a)
    forceClose(a.store().db)
    await a.waitFor('failed')
    clock = EXPIRED
    const b = page('owner-b')
    await b.ready()
    expect(b.storage.initialReads()?.recovery.liveOwner.sort()).toEqual([targets.term, targets.queue].sort())
    a.storage.retry()
    await a.waitFor('ready')
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['RESOLVED', 'QUEUED'])
  })

  it('settles an active write on a forced close by its observed commit', async () => {
    const a = page('owner-a')
    await a.ready()
    const { scan } = await mintOn(a)
    instrument = instrumentTransactions()
    const hold = instrument.hold(OCC_RW_STRICT)
    a.coordinator.latch({ kind: 'terminal', occurrenceId: scan.occurrenceId, outcome: denial, decidedAt: decided } as ProposedDecision)
    const persisting = a.coordinator.persist(scan.occurrenceId)
    await bounded(hold.started, 'held write')
    forceClose(a.store().db)
    hold.release()
    expect((await bounded(persisting, 'persist')).durable).toBe('committed')
    await a.waitFor('failed')
    expect(await state(scan.occurrenceId)).toBe('RESOLVED')
  })

  it('does not let a late close of an old generation fail a ready replacement', async () => {
    const a = page('owner-a')
    const first = await a.ready()
    const oldDb = first.db
    forceClose(oldDb)
    await a.waitFor('failed')
    a.storage.retry()
    await a.waitFor('ready')
    // The old generation's close callback arrives after the replacement is ready.
    oldDb.onclose?.call(oldDb, new Event('close'))
    await settle(10)
    expect(a.storage.state().status).toBe('ready')
  })
})

describe('reconnect identity (I2-07)', () => {
  it('keeps the same coordinator, owner and lock through repeated replacements; the denied id never queues', async () => {
    const a = page('owner-a')
    await a.ready()
    const coordinator = a.coordinator
    const targets = await failedTargets(a)
    for (let round = 0; round < 2; round += 1) {
      instrument = instrumentTransactions()
      instrument.inject('abort', OCC_RW_STRICT)
      instrument.inject('abort', OCC_RW_STRICT)
      forceClose(a.store().db)
      await a.waitFor('failed')
      a.storage.retry()
      await a.waitFor('ready')
      instrument.restore()
      instrument = undefined
      expect(a.storage.coordinator).toBe(coordinator)
      expect(coordinator.ownerId).toBe('owner-a')
      expect((await a.store().queued()).entries).toEqual([])
    }
    expect(await lockAvailable(ownerLockName('owner-a'))).toBe(false)
    await coordinator.repairAll()
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['RESOLVED', 'QUEUED'])
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual([targets.queue])
  })
})

describe('readiness with failed repairs (RD-06) and database deletion (RD-07)', () => {
  it('reaches ready after an observed failed repair and keeps the target, warning and lock (RD-06)', async () => {
    const a = page('owner-a')
    await a.ready()
    const targets = await failedTargets(a)
    forceClose(a.store().db)
    await a.waitFor('failed')
    instrument = instrumentTransactions()
    instrument.inject('abort', OCC_RW_STRICT)
    instrument.inject('quota', OCC_RW_STRICT)
    a.storage.retry()
    await a.waitFor('ready')
    expect(a.storage.initialReads()?.repairs.map((report) => report.durable)).toEqual(['failed', 'failed'])
    expect(a.coordinator.repairWarnings().map((warning) => warning.kind)).toEqual(['write_needs_repair', 'write_needs_repair'])
    expect([await state(targets.term), await state(targets.queue)]).toEqual(['PENDING', 'PENDING'])
    expect(await lockAvailable(ownerLockName('owner-a'))).toBe(false)
    clock = EXPIRED
    expect((await a.store().recoverDeadOwners()).recovered).toEqual([])
  })

  it('never recreates live targets after the database is deleted (RD-07)', async () => {
    const a = page('owner-a')
    await a.ready()
    const targets = await failedTargets(a)
    instrument = instrumentTransactions()
    instrument.inject('abort', OCC_RW_STRICT)
    instrument.inject('abort', OCC_RW_STRICT)
    const deleted = new Promise<void>((resolve, reject) => {
      const request = indexedDB.deleteDatabase(dbName)
      request.onsuccess = () => resolve()
      request.onerror = () => reject(request.error)
    })
    await a.waitFor('closed')
    expect(a.storage.state()).toMatchObject({ status: 'closed', cause: 'deleted' })
    await bounded(deleted, 'deletion')
    expect(instrument.calls.filter((call) => call.mode === 'readwrite' && call.events.includes('abort'))).toHaveLength(2)
    instrument.restore()
    instrument = undefined
    expect(a.coordinator.pendingTargets().sort()).toEqual([targets.term, targets.queue].sort())
    a.storage.retry()
    await a.waitFor('ready')
    expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
    expect(a.storage.initialReads()?.repairs.map((report) => report.durable)).toEqual(['unrepairable', 'unrepairable'])
    expect(a.coordinator.repairWarnings().map((warning) => warning.kind).sort()).toEqual(['cannot_repair', 'cannot_repair'])
    expect((await a.store().queued()).entries).toEqual([])
    const fresh = await mintOn(a)
    expect(await state(fresh.scan.occurrenceId)).toBe('PENDING')
    await eventually(() => a.storage.state().status === 'ready', 'still ready')
  })
})
