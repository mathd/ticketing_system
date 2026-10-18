import { forceCloseDatabase } from 'fake-indexeddb'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { FinalDecision, ProposedDecision } from './occurrence-coordinator'
import { STORE, StorageWriteError } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { bounded, captured, createTestPage, destroyAllPages, eventually, mintOn, settle, type TestPage } from './test/page-fixture'
import { rawGet, rawPut } from './test/raw-db'

// ST tier: the production store, coordinator and controller over fake-indexeddb.
// Every expected row is written out from ADR-085 §2.2, not read back from a run.

let dbName: string
let clock: number
let instrument: Instrument | undefined
const T0 = 1_000_000

beforeEach(() => {
  dbName = `st-${crypto.randomUUID()}`
  clock = T0
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
  vi.restoreAllMocks()
})

const page = (ownerId = 'owner-a', extra: { recoverOwnerId?: string } = {}) =>
  createTestPage({ dbName, ownerId, now: () => clock, renewalMs: 3_600_000, ...extra })

const decided = '2026-10-18T09:00:05.000Z'
const terminal = (occurrenceId: string, outcome: Extract<ProposedDecision, { kind: 'terminal' }>['outcome'], decidedAt = decided): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome, decidedAt })
const queue = (occurrenceId: string, outcome: Extract<ProposedDecision, { kind: 'queue' }>['outcome'], decidedAt = decided): ProposedDecision =>
  ({ kind: 'queue', occurrenceId, outcome, decidedAt })
const denied = (reason: string | null, latchId = 'latch-1') => ({ family: 'terminal', cause: 'denied', status: 409, latchId, reason }) as const
const transport = { family: 'queue', cause: 'transport_failure' } as const

const OCC_RW_STRICT = { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' } as const

function pendingRow(scan: ReturnType<typeof captured>, ownerId = 'owner-a', leaseExpiresAt = T0 + 30_000) {
  const base = {
    occurrenceId: scan.occurrenceId,
    qrPayload: scan.qrPayload,
    occurredAt: scan.occurredAt,
    createdAt: scan.createdAt,
    state: 'PENDING',
    actuated: false,
    ownerId,
    leaseExpiresAt,
    recordFormat: 1,
  }
  return scan.type === 'refusal' ? { ...base, localDecision: 'revocation_refused' } : { ...base, direction: 'entry' }
}

async function persistLatched(p: TestPage, proposal: ProposedDecision): Promise<FinalDecision> {
  const latched = p.coordinator.latch(proposal)
  await p.coordinator.persist(proposal.occurrenceId)
  return latched
}

describe('mint', () => {
  it('commits an owned typed PENDING row with the captured identity', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
    expect(a.coordinator.view(scan.occurrenceId)?.mint).toBe('committed')
  })

  it('mints a refusal as owned PENDING, never directly QUEUED (I6-01)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a, { type: 'refusal' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
    expect((await a.store().queued()).entries).toEqual([])
    // Only the refusal queue decision makes it reconcilable.
    await persistLatched(a, queue(scan.occurrenceId, { family: 'queue', cause: 'local_refusal' }))
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({
      ...pendingRow(scan),
      state: 'QUEUED',
      localOutcome: { family: 'queue', cause: 'local_refusal' },
      decidedAt: decided,
    })
  })

  it('resolves only after the transaction completes, not at request success (I3-01, I3-02)', async () => {
    const a = page()
    await a.ready()
    instrument = instrumentTransactions()
    const hold = instrument.hold(OCC_RW_STRICT)
    const scan = captured()
    a.coordinator.register(scan)
    let resolved = false
    const minting = a.store().mintNormal(scan.occurrenceId).then(() => { resolved = true })
    const call = await bounded(hold.started, 'mint transaction')
    await eventually(() => call.events.includes('write-success'), 'mint request success')
    await settle(10)
    expect(resolved).toBe(false)
    expect(a.coordinator.view(scan.occurrenceId)?.mint).toBe('pending')
    hold.release()
    await bounded(minting, 'mint')
    expect(call.events).toContain('complete')
    expect(a.coordinator.view(scan.occurrenceId)?.mint).toBe('committed')
  })

  it.each(['throw', 'abort'] as const)('reports a confirmed %s as an aborted mint with no row', async (kind) => {
    const a = page()
    await a.ready()
    instrument = instrumentTransactions()
    instrument.inject(kind, OCC_RW_STRICT)
    const scan = captured()
    a.coordinator.register(scan)
    await expect(a.store().mintNormal(scan.occurrenceId)).rejects.toBeInstanceOf(StorageWriteError)
    expect(a.coordinator.view(scan.occurrenceId)).toBeUndefined()
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toBeUndefined()
  })
})

describe('actuation reservation', () => {
  async function marking(a: TestPage, overrides = {}) {
    const minted = await mintOn(a, overrides)
    expect(a.coordinator.beginMarking(minted.scan.occurrenceId)).toBe(true)
    return minted
  }

  it('reserves an own eligible PENDING row once and latches ACT', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: true })
    const row = await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>
    expect(row).toStrictEqual({
      ...pendingRow(scan),
      state: 'ACTUATED',
      actuated: true,
      localOutcome: { family: 'actuation', cause: 'reserved' },
      decidedAt: new Date(T0).toISOString(),
    })
    expect(a.coordinator.latched(scan.occurrenceId)?.kind).toBe('actuation')
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'not_marking' })
  })

  it('resolves and latches ACT only after the mark transaction completes (I3-07, store half)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    instrument = instrumentTransactions()
    const hold = instrument.hold(OCC_RW_STRICT)
    let settled = false
    const reserving = a.store().reserveActuation(scan.occurrenceId).then((result) => { settled = true; return result })
    const call = await bounded(hold.started, 'mark transaction')
    await eventually(() => call.events.includes('write-success'), 'mark request success')
    await settle(10)
    expect(settled).toBe(false)
    expect(a.coordinator.latched(scan.occurrenceId)).toBeUndefined()
    expect(a.coordinator.view(scan.occurrenceId)?.mark).toBe('pending')
    hold.release()
    expect(await bounded(reserving, 'reservation')).toEqual({ reserved: true })
    expect(a.coordinator.latched(scan.occurrenceId)?.kind).toBe('actuation')
  })

  it('refuses a row owned by another page, with every other predicate passing (I3-03)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    const foreign = { ...pendingRow(scan), ownerId: 'owner-b' }
    await rawPut(dbName, STORE.occurrences, foreign)
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'foreign' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(foreign)
  })

  it.each([
    ['QUEUED', { state: 'QUEUED', localOutcome: transport, decidedAt: decided }],
    ['RESOLVED', { state: 'RESOLVED', localOutcome: denied('refunded'), decidedAt: decided }],
  ])('refuses an own %s row with actuated false (I3-04)', async (_state, change) => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    const row = { ...pendingRow(scan), ...change }
    await rawPut(dbName, STORE.occurrences, row)
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'ineligible_state' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
  })

  it('refuses an own PENDING row already marked actuated (I3-05)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    const row = { ...pendingRow(scan), actuated: true }
    await rawPut(dbName, STORE.occurrences, row)
    // The enforcing guard is the record validation: a PENDING row is never actuated.
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'invalid_record' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
  })

  it('refuses an own refusal row in MARKING (I3-06)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a, { type: 'refusal' })
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'refusal' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
  })

  it('reports this page\'s own existing ACT apart from a missing row', async () => {
    const a = page()
    await a.ready()
    const { scan } = await marking(a)
    await rawPut(dbName, STORE.occurrences, { ...pendingRow(scan), state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided })
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'own_existing_actuation' })
    const missing = captured()
    a.coordinator.register(missing)
    // A row that was never minted here cannot be reserved.
    expect(await a.store().reserveActuation(missing.occurrenceId)).toEqual({ reserved: false, why: 'not_marking' })
  })

  it('refuses a scan that is not in MARKING', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: false, why: 'not_marking' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
  })
})

describe('guarded decisions', () => {
  it('writes the latched terminal decision on the owned PENDING row', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    await persistLatched(a, terminal(scan.occurrenceId, { family: 'terminal', cause: 'auth_refused' }))
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({
      ...pendingRow(scan),
      state: 'RESOLVED',
      localOutcome: { family: 'terminal', cause: 'auth_refused' },
      decidedAt: decided,
    })
    expect(a.coordinator.view(scan.occurrenceId)?.durable).toBe('committed')
  })

  it('refuses a queue write for a terminal-decided PENDING row; only the queued control is listed (I1-02)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const control = await mintOn(a)
    await persistLatched(a, queue(control.scan.occurrenceId, transport))
    instrument = instrumentTransactions()
    instrument.inject('abort', OCC_RW_STRICT)
    const latch = a.coordinator.latch(terminal(scan.occurrenceId, denied('refunded')))
    expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
    // A caller that offers a queue decision for the same id is refused, and the latch stays.
    const offered = queue(scan.occurrenceId, transport)
    expect(await a.store().persistDecision(offered as FinalDecision)).toEqual({ status: 'ineligible', why: 'not the latched decision' })
    expect(a.coordinator.latch(offered)).toBe(latch)
    clock = T0 + 60_000
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
  })

  it.each([
    ['ACT (I1-10a)', { state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided }],
    ['TERM (I1-10b)', { state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'auth_refused' }, decidedAt: decided }],
  ])('never queues an own %s row; the PENDING control queues', async (_name, change) => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const control = await mintOn(a)
    const row = { ...pendingRow(scan), ...change }
    await rawPut(dbName, STORE.occurrences, row)
    a.coordinator.latch(queue(scan.occurrenceId, transport))
    a.coordinator.latch(queue(control.scan.occurrenceId, transport))
    expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('unrepairable')
    expect((await a.coordinator.persist(control.scan.occurrenceId)).durable).toBe('committed')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
  })

  it('confirms the same committed target as alreadySame and never reports a missing row as repaired', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const latch = await persistLatched(a, queue(scan.occurrenceId, transport))
    expect(await a.store().persistDecision(latch)).toEqual({ status: 'alreadySame' })
    const lost = await mintOn(a)
    const lostLatch = a.coordinator.latch(terminal(lost.scan.occurrenceId, { family: 'terminal', cause: 'mark_failure' }))
    const deleting = a.store().db.transaction(STORE.occurrences, 'readwrite')
    deleting.objectStore(STORE.occurrences).delete(lost.scan.occurrenceId)
    await new Promise((resolve) => { deleting.oncomplete = resolve })
    expect(await a.store().persistDecision(lostLatch)).toEqual({ status: 'missing' })
    expect(await rawGet(dbName, STORE.occurrences, lost.scan.occurrenceId)).toBeUndefined()
  })
})

describe('denial enrichment (TKT-558 D11)', () => {
  async function committedUnknownDenial(a: TestPage) {
    const { scan } = await mintOn(a)
    const latch = await persistLatched(a, terminal(scan.occurrenceId, denied(null)))
    const row = await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>
    return { scan, latch, row }
  }

  it('changes only the reason of the matching unknown denial', async () => {
    const a = page()
    await a.ready()
    const { scan, row } = await committedUnknownDenial(a)
    expect(a.coordinator.captureDenialReason(scan.occurrenceId, 'refunded')).toBe(true)
    expect(await a.coordinator.persist(scan.occurrenceId)).toEqual({ occurrenceId: scan.occurrenceId, durable: 'committed', enrichment: 'committed' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({ ...row, localOutcome: denied('refunded') })
  })

  it.each([
    ['another latch', (row: Record<string, unknown>) => ({ ...row, localOutcome: denied(null, 'latch-other') })],
    ['an ACT row', (row: Record<string, unknown>) => ({ ...row, state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' } })],
    ['a QUEUED row', (row: Record<string, unknown>) => ({ ...row, state: 'QUEUED', localOutcome: transport })],
    ['a known reason', (row: Record<string, unknown>) => ({ ...row, localOutcome: denied('exchanged') })],
  ])('refuses %s and leaves it unchanged', async (_name, change) => {
    const a = page()
    await a.ready()
    const { scan, latch, row } = await committedUnknownDenial(a)
    const changed = change(row)
    await rawPut(dbName, STORE.occurrences, changed)
    expect((await a.store().enrichDenial(latch as Extract<FinalDecision, { kind: 'terminal' }>, 'refunded')).status).toBe('ineligible')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(changed)
  })

  it('keeps an unknown denial terminal when enrichment aborts, and retries the reason later', async () => {
    const a = page()
    await a.ready()
    const { scan, row } = await committedUnknownDenial(a)
    a.coordinator.captureDenialReason(scan.occurrenceId, 'refunded')
    instrument = instrumentTransactions()
    instrument.inject('abort', OCC_RW_STRICT)
    expect((await a.coordinator.persist(scan.occurrenceId)).enrichment).toBe('pending')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
    expect((await a.store().queued()).entries).toEqual([])
    expect((await a.coordinator.persist(scan.occurrenceId)).enrichment).toBe('committed')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({ ...row, localOutcome: denied('refunded') })
  })

  it('treats a later repair of the unknown target as alreadySame after enrichment', async () => {
    const a = page()
    await a.ready()
    const { scan, latch, row } = await committedUnknownDenial(a)
    a.coordinator.captureDenialReason(scan.occurrenceId, 'refunded')
    await a.coordinator.persist(scan.occurrenceId)
    expect(await a.store().persistDecision(latch)).toEqual({ status: 'alreadySame' })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({ ...row, localOutcome: denied('refunded') })
  })
})

describe('reconcile retirement', () => {
  async function queued(a: TestPage, count: number) {
    const ids: string[] = []
    for (let index = 0; index < count; index += 1) {
      const { scan } = await mintOn(a)
      await persistLatched(a, queue(scan.occurrenceId, transport))
      ids.push(scan.occurrenceId)
    }
    return ids
  }

  it('retires an exact validated batch atomically, keeping the local decision', async () => {
    const a = page()
    await a.ready()
    const [q1, q2] = await queued(a, 2)
    const before = await rawGet(dbName, STORE.occurrences, q2) as Record<string, unknown>
    expect(await a.store().applyReconcileResults([q1, q2], [{ occurrence_id: q1, result: 'recorded' }, { occurrence_id: q2, result: 'rejected' }]))
      .toEqual({ status: 'applied', retired: 2, alreadyApplied: 0 })
    expect(await rawGet(dbName, STORE.occurrences, q2)).toStrictEqual({ ...before, state: 'RESOLVED', result: 'rejected' })
    expect((await rawGet(dbName, STORE.occurrences, q1) as Record<string, unknown>).state).toBe('SYNCED')
    // The same result for the same snapshot is idempotent.
    expect(await a.store().applyReconcileResults([q1], [{ occurrence_id: q1, result: 'recorded' }]))
      .toEqual({ status: 'applied', retired: 0, alreadyApplied: 1 })
  })

  it.each([
    ['ACT (I1-11a)', { state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt: decided }],
    ['TERM (I1-11b)', { state: 'RESOLVED', localOutcome: denied('refunded'), decidedAt: decided }],
    ['PEND (I1-11c)', {}],
  ])('never retires an own %s row; a separate queued control retires', async (_name, change) => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const [control] = await queued(a, 1)
    const row = { ...pendingRow(scan), ...change }
    await rawPut(dbName, STORE.occurrences, row)
    expect(await a.store().applyReconcileResults([scan.occurrenceId], [{ occurrence_id: scan.occurrenceId, result: 'recorded' }]))
      .toEqual({ status: 'incompatible', key: scan.occurrenceId })
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
    expect(await a.store().applyReconcileResults([control], [{ occurrence_id: control, result: 'synced' }]))
      .toEqual({ status: 'applied', retired: 1, alreadyApplied: 0 })
  })

  it.each([
    ['a foreign id', (q: string[]) => [{ occurrence_id: q[0], result: 'recorded' }, { occurrence_id: crypto.randomUUID(), result: 'recorded' }]],
    ['a duplicate id', (q: string[]) => [{ occurrence_id: q[0], result: 'recorded' }, { occurrence_id: q[0], result: 'recorded' }]],
    ['an omitted id', (q: string[]) => [{ occurrence_id: q[0], result: 'recorded' }]],
    ['an unknown result last (TKT-558 D17a)', (q: string[]) => [{ occurrence_id: q[0], result: 'recorded' }, { occurrence_id: q[1], result: 'accepted' }]],
  ])('retires nothing for %s (I7-03)', async (_name, results) => {
    const a = page()
    await a.ready()
    const ids = await queued(a, 2)
    const before = await Promise.all(ids.map((id) => rawGet(dbName, STORE.occurrences, id)))
    expect((await a.store().applyReconcileResults(ids, results(ids))).status).toBe('invalid')
    expect(await Promise.all(ids.map((id) => rawGet(dbName, STORE.occurrences, id)))).toStrictEqual(before)
    // The valid retry retires the same ids.
    expect((await a.store().applyReconcileResults(ids, ids.map((id) => ({ occurrence_id: id, result: 'recorded' })))).status).toBe('applied')
  })

  it('retires no row of a batch whose write aborts, then retires the whole batch on retry (TKT-558 D17b)', async () => {
    const a = page()
    await a.ready()
    const ids = await queued(a, 3)
    const before = await Promise.all(ids.map((id) => rawGet(dbName, STORE.occurrences, id)))
    instrument = instrumentTransactions()
    const originalPut = IDBObjectStore.prototype.put
    const successfulWrites: string[] = []
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      const request = originalPut.call(this, value, key)
      if (this.name === STORE.occurrences && value.state === 'SYNCED') {
        request.addEventListener('success', () => {
          successfulWrites.push(value.occurrenceId)
          if (value.occurrenceId === ids[1]) this.transaction.abort()
        })
      }
      return request
    })
    const results = ids.map((id) => ({ occurrence_id: id, result: 'recorded' }))
    await expect(a.store().applyReconcileResults(ids, results)).rejects.toBeInstanceOf(StorageWriteError)
    expect(successfulWrites).toEqual(ids.slice(0, 2))
    expect(await Promise.all(ids.map((id) => rawGet(dbName, STORE.occurrences, id)))).toStrictEqual(before)
    vi.restoreAllMocks()
    expect(await a.store().applyReconcileResults(ids, results)).toEqual({ status: 'applied', retired: 3, alreadyApplied: 0 })
  })
})

describe('queue listing (I7-04)', () => {
  it('lists only committed QUEUED rows, never pending, actuated, terminal or synced rows', async () => {
    const a = page()
    await a.ready()
    const own = await mintOn(a)
    const act = await mintOn(a)
    a.coordinator.beginMarking(act.scan.occurrenceId)
    await a.store().reserveActuation(act.scan.occurrenceId)
    const term = await mintOn(a)
    await persistLatched(a, terminal(term.scan.occurrenceId, { family: 'terminal', cause: 'auth_refused' }))
    const synced = await mintOn(a)
    await persistLatched(a, queue(synced.scan.occurrenceId, transport))
    await a.store().applyReconcileResults([synced.scan.occurrenceId], [{ occurrence_id: synced.scan.occurrenceId, result: 'synced' }])
    const control = await mintOn(a)
    await persistLatched(a, queue(control.scan.occurrenceId, transport))
    const foreign = captured()
    await rawPut(dbName, STORE.occurrences, pendingRow(foreign, 'owner-b'))
    const listed = await a.store().queued()
    expect(listed.entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
    expect(listed.warnings).toEqual([])
    expect(own.scan.occurrenceId).not.toBe(control.scan.occurrenceId)
  })

  it('keeps a corrupt current-format row local with a warning and still lists valid rows', async () => {
    const a = page()
    await a.ready()
    const control = await mintOn(a)
    await persistLatched(a, queue(control.scan.occurrenceId, transport))
    const corrupt = { ...pendingRow(captured()), state: 'QUEUED', direction: undefined }
    await rawPut(dbName, STORE.occurrences, corrupt)
    const listed = await a.store().queued()
    expect(listed.entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
    expect(listed.warnings).toEqual([{ key: corrupt.occurrenceId, problem: expect.any(String) }])
  })
})

describe('lease renewal', () => {
  it('renews only own current-format PENDING leases, with default durability (TKT-558 D23)', async () => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const foreign = captured()
    await rawPut(dbName, STORE.occurrences, pendingRow(foreign, 'owner-b'))
    clock = T0 + 5_000
    instrument = instrumentTransactions()
    expect(await a.store().renewOwnedPending()).toBe(1)
    expect(instrument.calls.map((call) => [call.storeNames, call.mode, call.options])).toEqual([
      [[STORE.occurrences], 'readwrite', { durability: 'default' }],
    ])
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan, 'owner-a', T0 + 35_000))
    expect(await rawGet(dbName, STORE.occurrences, foreign.occurrenceId)).toStrictEqual(pendingRow(foreign, 'owner-b'))
  })

  it.each(['renewal first', 'decision first'] as const)('never puts a stale PENDING snapshot over a committed decision: %s (PR1-12)', async (order) => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    const latch = a.coordinator.latch(terminal(scan.occurrenceId, denied('refunded')))
    clock = T0 + 5_000
    const writes = order === 'renewal first'
      ? [a.store().renewOwnedPending(), a.store().persistDecision(latch)]
      : [a.store().persistDecision(latch), a.store().renewOwnedPending()]
    await bounded(Promise.all(writes), 'renewal and decision')
    const expectedLease = order === 'renewal first' ? T0 + 35_000 : T0 + 30_000
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({
      ...pendingRow(scan, 'owner-a', expectedLease),
      state: 'RESOLVED',
      localOutcome: denied('refunded'),
      decidedAt: decided,
    })
  })
})

describe('durability options (TKT-558 D14)', () => {
  type Case = [string, (a: TestPage) => Promise<unknown>, string[]]
  const cases: Case[] = [
    ['normal mint', async (a) => { const scan = captured(); a.coordinator.register(scan); await a.store().mintNormal(scan.occurrenceId) }, [STORE.occurrences]],
    ['refusal mint', async (a) => { const scan = captured({ type: 'refusal' }); a.coordinator.register(scan); await a.store().mintRefusal(scan.occurrenceId) }, [STORE.occurrences]],
    ['terminal decision', async (a) => { const { scan } = await mintOn(a); instrumentOnly(); await persistLatched(a, terminal(scan.occurrenceId, { family: 'terminal', cause: 'auth_refused' })) }, [STORE.occurrences]],
    ['queue decision', async (a) => { const { scan } = await mintOn(a); instrumentOnly(); await persistLatched(a, queue(scan.occurrenceId, transport)) }, [STORE.occurrences]],
    ['denial enrichment', async (a) => {
      const { scan } = await mintOn(a)
      await persistLatched(a, terminal(scan.occurrenceId, denied(null)))
      a.coordinator.captureDenialReason(scan.occurrenceId, 'refunded')
      instrumentOnly()
      await a.coordinator.persist(scan.occurrenceId)
    }, [STORE.occurrences]],
    ['actuation mark', async (a) => { const { scan } = await mintOn(a); a.coordinator.beginMarking(scan.occurrenceId); instrumentOnly(); await a.store().reserveActuation(scan.occurrenceId) }, [STORE.occurrences]],
    ['reconcile retirement', async (a) => {
      const { scan } = await mintOn(a)
      await persistLatched(a, queue(scan.occurrenceId, transport))
      instrumentOnly()
      await a.store().applyReconcileResults([scan.occurrenceId], [{ occurrence_id: scan.occurrenceId, result: 'recorded' }])
    }, [STORE.occurrences, STORE.legacyClasses]],
    ['notice acknowledgement', async (a) => {
      await rawPut(dbName, STORE.recoveryNotices, { occurrenceId: 'n1', cause: 'uncertain_recovery', recoveredAt: decided })
      instrumentOnly()
      await a.store().acknowledgeNotice('n1')
    }, [STORE.recoveryNotices]],
    ['export receipt', async (a) => { await a.store().commitExportReceipt({ receiptId: 'r1', exportedAt: decided, held: [] }) }, [STORE.exportReceipts]],
    ['held acknowledgement', async (a) => {
      await rawPut(dbName, STORE.occurrences, { occurrenceId: 'h1', state: 'PENDING' })
      await rawPut(dbName, STORE.legacyClasses, { occurrenceId: 'h1', legacyClass: 'M01', sourceVersion: 2 })
      const { encodeHeldSnapshot } = await import('./occurrence-export')
      await a.store().commitExportReceipt({ receiptId: 'r1', exportedAt: decided, held: [{ occurrenceId: 'h1', snapshot: encodeHeldSnapshot('h1', { occurrenceId: 'h1', state: 'PENDING' }) }] })
      instrumentOnly()
      await a.store().acknowledgeHeld('r1', ['h1'])
    }, [STORE.exportReceipts, STORE.occurrences, STORE.legacyClasses, STORE.heldDisposals]],
  ]

  let active: Instrument | undefined
  function instrumentOnly() {
    active?.restore()
    active = instrumentTransactions()
    instrument = active
  }

  it.each(cases)('passes strict durability on the %s transaction', async (_name, run, stores) => {
    const a = page()
    await a.ready()
    instrumentOnly()
    await run(a)
    const writes = active!.calls.filter((call) => call.mode === 'readwrite')
    expect(writes.length).toBeGreaterThan(0)
    for (const call of writes) {
      expect([call.storeNames.slice().sort(), call.options]).toEqual([stores.slice().sort(), { durability: 'strict' }])
    }
    active = undefined
  })

  it('passes strict durability on the recovery transaction', async () => {
    const dead = page('owner-dead')
    await dead.ready()
    await mintOn(dead)
    dead.destroy()
    clock = T0 + 60_000
    instrument = instrumentTransactions()
    const b = page('owner-b')
    await b.ready()
    const recovery = instrument.calls.filter((call) => call.storeNames.includes(STORE.recoveryNotices) && call.mode === 'readwrite')
    expect(recovery).toHaveLength(1)
    expect(recovery[0].options).toEqual({ durability: 'strict' })
  })
})

describe('reservation through a connection event (I3-09, store half)', () => {
  const forceClose = (db: IDBDatabase) => forceCloseDatabase(db as never)
  function versionChangeThenAbortUpgrade(): Promise<void> {
    return new Promise((resolve) => {
      const request = indexedDB.open(dbName, 4)
      request.onupgradeneeded = () => request.transaction!.abort()
      request.onerror = () => resolve()
      request.onsuccess = () => { request.result.close(); resolve() }
    })
  }

  it.each(['unexpected close', 'version change'] as const)('commits an own reservation held through an %s and latches ACT', async (event) => {
    const a = page()
    await a.ready()
    const { scan } = await mintOn(a)
    a.coordinator.beginMarking(scan.occurrenceId)
    instrument = instrumentTransactions()
    const hold = instrument.hold(OCC_RW_STRICT)
    const reserving = a.store().reserveActuation(scan.occurrenceId)
    await bounded(hold.started, 'mark held')
    const db = a.store().db
    const changing = event === 'version change' ? versionChangeThenAbortUpgrade() : (forceClose(db), Promise.resolve())
    // A version change drains before the held mark settles. fake-indexeddb's forced close
    // waits for the active transaction, so its close follows the release.
    if (event === 'version change') await eventually(() => a.storage.state().status === 'draining', 'draining')
    hold.release()
    expect(await bounded(reserving, 'reservation')).toEqual({ reserved: true })
    expect(a.coordinator.latched(scan.occurrenceId)?.kind).toBe('actuation')
    await bounded(changing, 'connection event settles')
    await a.waitFor(event === 'version change' ? 'closed' : 'failed')
    expect((await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>).state).toBe('ACTUATED')
  })
})

describe('revocation set', () => {
  const ticket = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'

  it('keeps ids from one pull after another fingerprint completes a pull', async () => {
    const a = page()
    await a.ready()
    await a.store().mergeRevoked([ticket])
    await a.store().completeRevocationPull('fingerprint-b', '2026-09-25T12:00:00Z')
    expect(await a.store().isRevoked(ticket)).toBe(true)
  })

  it('records completion separately for each token fingerprint', async () => {
    const a = page()
    await a.ready()
    await a.store().completeRevocationPull('fingerprint-a', '2026-09-25T12:00:00Z')
    expect(await a.store().revocationPull('fingerprint-a')).toEqual({ completedAt: '2026-09-25T12:00:00Z' })
    expect(await a.store().revocationPull('fingerprint-b')).toEqual({})
    await a.store().completeRevocationPull('fingerprint-b', '2026-09-25T12:15:00Z')
    expect(await a.store().revocationPull('fingerprint-a')).toEqual({ completedAt: '2026-09-25T12:00:00Z' })
    expect(await a.store().revocationPull('fingerprint-b')).toEqual({ completedAt: '2026-09-25T12:15:00Z' })
  })

  it('normalizes merged ids before lookup', async () => {
    const a = page()
    await a.ready()
    await a.store().mergeRevoked([ticket.toUpperCase()])
    expect(await a.store().isRevoked(ticket)).toBe(true)
    expect(await a.store().isRevoked(ticket.toUpperCase())).toBe(true)
  })

  it('never removes an id when another pull completes or a write fails', async () => {
    const a = page()
    await a.ready()
    await a.store().mergeRevoked([ticket])
    await a.store().completeRevocationPull('fingerprint-a', '2026-09-25T12:00:00Z')
    instrument = instrumentTransactions()
    instrument.inject('abort', { stores: [STORE.revocations], mode: 'readwrite' })
    await expect(a.store().mergeRevoked(['3f8fb96c-44f9-4a66-8c12-7d91cfae4d72'])).rejects.toThrow()
    await a.store().completeRevocationPull('fingerprint-b', '2026-09-25T12:15:00Z')
    expect(await a.store().isRevoked(ticket)).toBe(true)
    expect(await a.store().isRevoked('3f8fb96c-44f9-4a66-8c12-7d91cfae4d72')).toBe(false)
  })
})
