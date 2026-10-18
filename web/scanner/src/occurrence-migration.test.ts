import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { REQUIRED_STORES, STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { V1_STORES, V2_STORES, openRaw, rawGet, rawGetAll, rawStoreNames, seedDatabase } from './test/raw-db'

// ST tier: the production version-3 upgrade over literal version-1 and
// version-2 rows (shapes from 98e70e80 and ec336db8). Classification uses the
// stored state only (TKT-558 D12, D25). Expected classes are written out here.

let dbName: string
let clock: number
let instrument: Instrument | undefined
const T0 = 1_000_000

beforeEach(() => {
  dbName = `mig-${crypto.randomUUID()}`
  clock = T0
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
  vi.restoreAllMocks()
})

const page = (ownerId: string, recoverOwnerId?: string) =>
  createTestPage({ dbName, ownerId, recoverOwnerId, now: () => clock, renewalMs: 3_600_000 })

// Literal version-1 rows: no owner, lease or refusal field existed.
const v1 = {
  pending: { occurrenceId: 'v1-pending', qrPayload: 'qr-p', occurredAt: '2026-09-01T10:00:00.111Z', state: 'PENDING', actuated: false, createdAt: '2026-09-01T10:00:00.112Z' },
  queued: { occurrenceId: 'v1-queued', qrPayload: 'qr-q', occurredAt: '2026-09-01T10:00:01.221Z', state: 'QUEUED', actuated: false, createdAt: '2026-09-01T10:00:01.222Z' },
  actuated: { occurrenceId: 'v1-actuated', qrPayload: 'qr-a', occurredAt: '2026-09-01T10:00:02.331Z', state: 'ACTUATED', actuated: true, createdAt: '2026-09-01T10:00:02.332Z' },
  synced: { occurrenceId: 'v1-synced', qrPayload: 'qr-s', occurredAt: '2026-09-01T10:00:03.441Z', state: 'SYNCED', actuated: false, result: 'recorded', createdAt: '2026-09-01T10:00:03.442Z' },
  resolved: { occurrenceId: 'v1-resolved', qrPayload: 'qr-r', occurredAt: '2026-09-01T10:00:04.551Z', state: 'RESOLVED', actuated: false, result: 'conflict', createdAt: '2026-09-01T10:00:04.552Z' },
  unknownState: { occurrenceId: 'v1-unknown', qrPayload: 'qr-u', occurredAt: '2026-09-01T10:00:05.661Z', state: 'LOST', actuated: false, createdAt: '2026-09-01T10:00:05.662Z' },
  noState: { occurrenceId: 'v1-nostate', qrPayload: 'qr-n' },
}

// Literal version-2 rows: owner and lease on minted rows, refusal born QUEUED.
const v2 = {
  pendingExpired: { occurrenceId: 'v2-pending-expired', qrPayload: 'qr-pe', occurredAt: '2026-10-01T10:00:00.111Z', state: 'PENDING', actuated: false, createdAt: '2026-10-01T10:00:00.112Z', ownerId: 'old-owner', leaseExpiresAt: 5 },
  pendingOwnerless: { occurrenceId: 'v2-pending-ownerless', qrPayload: 'qr-po', occurredAt: '2026-10-01T10:00:01.111Z', state: 'PENDING', actuated: false, createdAt: '2026-10-01T10:00:01.112Z' },
  queuedRefusal: { occurrenceId: 'v2-queued-refusal', qrPayload: 'qr-qr', occurredAt: '2026-10-01T10:00:02.111Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-01T10:00:02.112Z', localDecision: 'revocation_refused' },
  queuedOdd: { occurrenceId: 'v2-queued-odd', qrPayload: 42, occurredAt: '2026-10-01T10:00:03.111Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-01T10:00:03.112Z', direction: undefined },
  queuedControl: { occurrenceId: 'v2-queued-control', qrPayload: 'qr-qc', occurredAt: '2026-10-01T10:00:04.111Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-01T10:00:04.112Z', ownerId: 'old-owner', leaseExpiresAt: 7 },
  actuated: { occurrenceId: 'v2-actuated', qrPayload: 'qr-a2', occurredAt: '2026-10-01T10:00:05.111Z', state: 'ACTUATED', actuated: true, createdAt: '2026-10-01T10:00:05.112Z', ownerId: 'old-owner', leaseExpiresAt: 9 },
  unknownState: { occurrenceId: 'v2-unknown', state: 'MYSTERY', extra: { nested: [1, 'two'] } },
}

const classes = async () =>
  (await rawGetAll(dbName, STORE.legacyClasses) as Array<{ occurrenceId: string; legacyClass: string; sourceVersion: number }>)
    .map((entry) => [entry.occurrenceId, entry.legacyClass, entry.sourceVersion])

describe('version-3 upgrade', () => {
  it('creates the complete schema on an empty device (source 0)', async () => {
    const a = page('owner-a')
    await a.ready()
    expect(await rawStoreNames(dbName)).toEqual([...REQUIRED_STORES].sort())
    expect(await rawGetAll(dbName, STORE.legacyClasses)).toEqual([])
    expect(a.storage.initialReads()?.disposal).toEqual({ held: [], legacyQueued: [] })
  })

  it('classifies literal version-1 rows by state only and leaves each row unchanged (source 1)', async () => {
    await seedDatabase(dbName, 1, V1_STORES, { occurrences: Object.values(v1) })
    const a = page('owner-a')
    await a.ready()
    expect(await rawStoreNames(dbName)).toEqual([...REQUIRED_STORES].sort())
    expect(await classes()).toEqual([
      ['v1-actuated', 'other', 1],
      ['v1-nostate', 'unclassifiable', 1],
      ['v1-pending', 'M01', 1],
      ['v1-queued', 'M02', 1],
      ['v1-resolved', 'other', 1],
      ['v1-synced', 'other', 1],
      ['v1-unknown', 'unclassifiable', 1],
    ])
    for (const row of Object.values(v1)) expect(await rawGet(dbName, STORE.occurrences, row.occurrenceId)).toStrictEqual(row)
    expect((await a.store().queued()).entries).toEqual([{ format: 0, key: 'v1-queued', legacyClass: 'M02', raw: v1.queued }])
  })

  it('classifies literal version-2 rows, keeps revocation data and holds legacy PENDING even with an absent owner (source 2, I8-06)', async () => {
    await seedDatabase(dbName, 2, V2_STORES, {
      occurrences: Object.values(v2),
      revocations: [{ ticketID: 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d' }],
      revocation_meta: [{ key: 'pull:fingerprint-a', completedAt: '2026-10-01T09:00:00Z' }],
    })
    clock = T0 + 60_000
    const a = page('owner-a', 'old-owner')
    await a.ready()
    expect(await classes()).toEqual([
      ['v2-actuated', 'other', 2],
      ['v2-pending-expired', 'M01', 2],
      ['v2-pending-ownerless', 'M01', 2],
      ['v2-queued-control', 'M02', 2],
      ['v2-queued-odd', 'M02', 2],
      ['v2-queued-refusal', 'M02', 2],
      ['v2-unknown', 'unclassifiable', 2],
    ])
    for (const row of Object.values(v2)) expect(await rawGet(dbName, STORE.occurrences, row.occurrenceId)).toStrictEqual(row)
    expect(await a.store().isRevoked('A8E94ED1-A02B-4CB7-A47B-A607F7B3872D')).toBe(true)
    expect(await a.store().revocationPull('fingerprint-a')).toEqual({ completedAt: '2026-10-01T09:00:00Z' })
    // The old owner's lock is absent, its lease expired and this page names it as its
    // reload predecessor. Legacy PENDING still stays held (owner D13-3).
    expect(a.storage.initialReads()?.recovery).toEqual({ recovered: [], liveOwner: [], notEligible: [], failed: [] })
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual(['v2-queued-control', 'v2-queued-odd', 'v2-queued-refusal'])
    // A new-format dead PENDING control does recover.
    const dead = page('owner-dead')
    await dead.ready()
    const { scan } = await mintOn(dead)
    dead.destroy()
    clock = T0 + 120_000
    expect((await a.store().recoverDeadOwners()).recovered).toEqual([scan.occurrenceId])
    expect(await rawGet(dbName, STORE.occurrences, 'v2-pending-expired')).toStrictEqual(v2.pendingExpired)
    expect(await rawGet(dbName, STORE.occurrences, 'v2-pending-ownerless')).toStrictEqual(v2.pendingOwnerless)
  })

  it('keeps every pre-upgrade QUEUED row in M02, odd fields included (MG-01)', async () => {
    await seedDatabase(dbName, 2, V2_STORES, { occurrences: [v2.queuedOdd, v2.queuedControl] })
    const a = page('owner-a')
    await a.ready()
    expect(await classes()).toEqual([['v2-queued-control', 'M02', 2], ['v2-queued-odd', 'M02', 2]])
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual(['v2-queued-control', 'v2-queued-odd'])
    expect(a.storage.initialReads()?.disposal.held).toEqual([])
  })

  it('holds a valid-key unknown-state record without aborting the upgrade (TKT-558 D12 unknown shape)', async () => {
    await seedDatabase(dbName, 2, V2_STORES, {
      occurrences: [v2.unknownState, v2.queuedControl],
      revocations: [{ ticketID: 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d' }],
    })
    const a = page('owner-a')
    await a.ready()
    expect(a.storage.initialReads()?.disposal.held).toEqual([
      { key: 'v2-unknown', legacyClass: 'unclassifiable', raw: v2.unknownState, disposal: undefined },
    ])
    expect(await rawGet(dbName, STORE.occurrences, 'v2-unknown')).toStrictEqual(v2.unknownState)
    expect(await a.store().isRevoked('a8e94ed1-a02b-4cb7-a47b-a607f7b3872d')).toBe(true)
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual(['v2-queued-control'])
  })

  it('aborts atomically on a real upgrade failure and leaves version 1 unchanged', async () => {
    await seedDatabase(dbName, 1, V1_STORES, { occurrences: [v1.pending, v1.queued] })
    const put = IDBObjectStore.prototype.put
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, ...args: Parameters<IDBObjectStore['put']>) {
      if (this.name === STORE.legacyClasses) throw new DOMException('injected upgrade failure', 'QuotaExceededError')
      return put.apply(this, args)
    })
    const a = page('owner-a')
    a.storage.start()
    await a.waitFor('failed')
    expect(a.storage.state()).toMatchObject({ status: 'failed', cause: 'open_failed' })
    vi.restoreAllMocks()
    const raw = await openRaw(dbName)
    expect([raw.version, [...raw.objectStoreNames]]).toEqual([1, ['occurrences']])
    raw.close()
    expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual([v1.pending, v1.queued])
  })

  it('refuses an already-v3 database without the metadata stores and keeps its data (MG-02)', async () => {
    const row = { occurrenceId: 'tkt399-row', qrPayload: 'qr', occurredAt: '2026-10-10T10:00:00Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-10T10:00:00Z', direction: 'exit' }
    await seedDatabase(dbName, 3, V2_STORES, { occurrences: [row] })
    instrument = instrumentTransactions()
    const a = page('owner-a')
    a.storage.start()
    await a.waitFor('failed')
    expect(a.storage.state()).toMatchObject({ status: 'failed', cause: 'incompatible_schema' })
    expect(a.storage.readyStore()).toBeNull()
    expect(instrument.calls).toEqual([])
    instrument.restore()
    instrument = undefined
    expect(await rawStoreNames(dbName)).toEqual(['occurrences', 'revocation_meta', 'revocations'])
    expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual([row])
  })
})

describe('pre-upgrade QUEUED stored shapes (M02)', () => {
  const base = { qrPayload: 'qr-m02', occurredAt: '2026-10-02T10:00:00.123Z', state: 'QUEUED', actuated: false, createdAt: '2026-10-02T10:00:00.124Z' }
  const shapes: Record<string, Record<string, unknown>> = {}
  for (const owner of ['present', 'absent'] as const) {
    for (const lease of ['absent', 'future', 'expired'] as const) {
      shapes[`owner-${owner}-lease-${lease}`] = {
        ...base,
        ...(owner === 'present' ? { ownerId: 'old-owner' } : {}),
        ...(lease === 'future' ? { leaseExpiresAt: 9_999_999_999_999 } : lease === 'expired' ? { leaseExpiresAt: 1 } : {}),
      }
    }
  }
  shapes['actuated-true'] = { ...base, actuated: true }
  shapes['result-present'] = { ...base, result: 'recorded' }
  for (const [name, direction] of Object.entries({ undefined, null: null, empty: '', number: 0, unknown: 'sideways', entry: 'entry', exit: 'exit' })) {
    shapes[`direction-${name}`] = { ...base, direction }
  }
  shapes['refusal-with-exit-direction'] = { ...base, localDecision: 'revocation_refused', direction: 'exit' }
  shapes['refusal'] = { ...base, localDecision: 'revocation_refused' }
  const rows: Array<Record<string, unknown> & { occurrenceId: string }> = Object.entries(shapes).map(([name, shape]) => ({ occurrenceId: `m02-${name}`, ...shape }))

  it('keeps each literal shape, own-property presence included, as M02 and lists it for sync', async () => {
    await seedDatabase(dbName, 2, V2_STORES, { occurrences: rows })
    const a = page('owner-a')
    await a.ready()
    const keys = rows.map((row) => row.occurrenceId).sort()
    expect(await classes()).toEqual(keys.map((key) => [key, 'M02', 2]))
    for (const row of rows) expect(await rawGet(dbName, STORE.occurrences, row.occurrenceId)).toStrictEqual(row)
    expect((await a.store().queued()).entries.map((entry) => entry.key)).toEqual(keys)
    // Retirement keeps the class (DX-01 relies on it).
    const valid = rows.filter((row) => typeof row.qrPayload === 'string').map((row) => row.occurrenceId)
    expect((await a.store().applyReconcileResults(valid, valid.map((id) => ({ occurrence_id: id, result: 'recorded' })))).status).toBe('applied')
    expect(await classes()).toEqual(keys.map((key) => [key, 'M02', 2]))
  })
})
