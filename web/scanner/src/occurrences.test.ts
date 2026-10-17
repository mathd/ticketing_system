import 'fake-indexeddb/auto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  OccurrenceStoreBlockedError,
  OccurrenceStoreClosedError,
  openOccurrenceStore,
  type OccurrenceStore,
  type OccurrenceStoreOptions,
  type OwnerLiveness,
} from './occurrences'

// The ADR-025 §D3 actuation floor: a response — first-time or replay — may
// actuate the gate iff THIS device holds a durably pending, never-actuated
// record for that occurrence id. Copied ids never actuate; a lost-response
// retry completes exactly once; marking happens before the gate opens.

let store: OccurrenceStore
const openedStores: OccurrenceStore[] = []
let ownerLiveness: FakeOwnerLiveness

class FakeOwnerLiveness implements OwnerLiveness {
  private readonly held = new Set<string>()

  async hold(ownerId: string): Promise<() => void> {
    if (this.held.has(ownerId)) throw new Error(`owner already held: ${ownerId}`)
    this.held.add(ownerId)
    return () => this.held.delete(ownerId)
  }

  async runIfOwnerAbsent(ownerId: string, task: () => Promise<void>): Promise<boolean> {
    if (this.held.has(ownerId)) return false
    this.held.add(ownerId)
    try {
      await task()
      return true
    } finally {
      this.held.delete(ownerId)
    }
  }
}

async function openStore(dbName: string, options: OccurrenceStoreOptions = {}): Promise<OccurrenceStore> {
  const opened = await openOccurrenceStore(dbName, { ownerLiveness, ...options })
  openedStores.push(opened)
  return opened
}

async function openBrowserStore(dbName: string, options: OccurrenceStoreOptions): Promise<OccurrenceStore> {
  const opened = await openOccurrenceStore(dbName, options)
  openedStores.push(opened)
  return opened
}

beforeEach(async () => {
  ownerLiveness = new FakeOwnerLiveness()
  store = await openStore(`test-${crypto.randomUUID()}`)
})

afterEach(() => {
  for (const opened of openedStores.splice(0)) opened.close()
  vi.useRealTimers()
})

describe('durable pending record', () => {
  it('commits the PENDING record with a fresh UUIDv4 before anything else can run', async () => {
    const record = await store.mint('qr-payload', '2026-07-17T09:00:00Z', 'entry')
    expect(record.state).toBe('PENDING')
    expect(record.actuated).toBe(false)
    // UUIDv4 shape — the id is scanner-minted (ADR-025 §D3).
    expect(record.occurrenceId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    // Durably readable back — mint resolves only after the IDB transaction commits.
    const reread = await store.get(record.occurrenceId)
    expect(reread?.state).toBe('PENDING')
  })

  it('mints a distinct id per decision — retries reuse, decisions do not', async () => {
    const a = await store.mint('qr-payload', '2026-07-17T09:00:00Z', 'entry')
    const b = await store.mint('qr-payload', '2026-07-17T09:01:00Z', 'entry')
    expect(a.occurrenceId).not.toBe(b.occurrenceId)
  })

  it('commits a revocation refusal directly as queued', async () => {
    const record = await store.mintRefusal('listed-ticket', '2026-07-17T09:00:00Z')
    expect(record).toMatchObject({ state: 'QUEUED', actuated: false, localDecision: 'revocation_refused' })
    expect(await store.get(record.occurrenceId)).toMatchObject({ state: 'QUEUED', localDecision: 'revocation_refused' })
  })

  // The direction is a fact of the occurrence (TKT-399). It is committed with the
  // record, so a later mode change cannot relabel it. A refusal has no direction:
  // Access stores it as a refusal only, and an own property would be serialized.
  it('commits the scan direction with the record and stores none on a refusal', async () => {
    const exit = await store.mint('exit-qr', '2026-07-17T09:00:00Z', 'exit')
    const entry = await store.mint('entry-qr', '2026-07-17T09:01:00Z', 'entry')
    const refusal = await store.mintRefusal('listed-qr', '2026-07-17T09:02:00Z')

    expect(exit.direction).toBe('exit')
    expect(await store.get(exit.occurrenceId)).toMatchObject({ state: 'PENDING', direction: 'exit' })
    expect(await store.get(entry.occurrenceId)).toMatchObject({ state: 'PENDING', direction: 'entry' })
    expect(Object.hasOwn((await store.get(refusal.occurrenceId))!, 'direction')).toBe(false)
  })
})

describe('revocation set', () => {
  it('keeps ids from one pull after another fingerprint completes a pull', async () => {
    await store.mergeRevoked([ticketIDForStore])
    await store.completeRevocationPull('fingerprint-b', '2026-09-25T12:00:00Z')
    expect(await store.isRevoked(ticketIDForStore)).toBe(true)
  })

  it('records completion separately for each token fingerprint', async () => {
    await store.completeRevocationPull('fingerprint-a', '2026-09-25T12:00:00Z')
    expect(await store.revocationPull('fingerprint-a')).toEqual({ completedAt: '2026-09-25T12:00:00Z' })
    expect(await store.revocationPull('fingerprint-b')).toEqual({})
    await store.completeRevocationPull('fingerprint-b', '2026-09-25T12:15:00Z')
    expect(await store.revocationPull('fingerprint-a')).toEqual({ completedAt: '2026-09-25T12:00:00Z' })
    expect(await store.revocationPull('fingerprint-b')).toEqual({ completedAt: '2026-09-25T12:15:00Z' })
  })

  it('normalizes merged ids before lookup', async () => {
    await store.mergeRevoked([ticketIDForStore.toUpperCase()])
    expect(await store.isRevoked(ticketIDForStore.toLowerCase())).toBe(true)
  })

  it('keeps queued rows on a v1 upgrade and merges ids across completed pulls', async () => {
    const dbName = `upgrade-${crypto.randomUUID()}`
    const v1 = indexedDB.open(dbName, 1)
    v1.onupgradeneeded = () => v1.result.createObjectStore('occurrences', { keyPath: 'occurrenceId' })
    const legacyDB = await new Promise<IDBDatabase>((resolve, reject) => {
      v1.onsuccess = () => resolve(v1.result)
      v1.onerror = () => reject(v1.error)
    })
    const legacy = {
      occurrenceId: crypto.randomUUID(), qrPayload: 'old', occurredAt: '2026-09-01T00:00:00Z',
      state: 'QUEUED', actuated: false, createdAt: '2026-09-01T00:00:00Z',
    }
    const tx = legacyDB.transaction('occurrences', 'readwrite')
    tx.objectStore('occurrences').put(legacy)
    await new Promise<void>((resolve, reject) => {
      tx.oncomplete = () => resolve()
      tx.onerror = () => reject(tx.error)
    })
    legacyDB.close()

    const upgraded = await openStore(dbName)
    openedStores.push(upgraded)
    await upgraded.mergeRevoked([ticketIDForStore])
    await upgraded.completeRevocationPull('upgrade-fingerprint', '2026-09-25T12:00:00Z')
    expect(await upgraded.revocationPull('upgrade-fingerprint')).toEqual({ completedAt: '2026-09-25T12:00:00Z' })
    await upgraded.mergeRevoked(['3f8fb96c-44f9-4a66-8c12-7d91cfae4d72'])
    await upgraded.completeRevocationPull('upgrade-fingerprint', '2026-09-25T12:15:00Z')

    expect(await upgraded.isRevoked(ticketIDForStore)).toBe(true)
    expect(await upgraded.isRevoked('3f8fb96c-44f9-4a66-8c12-7d91cfae4d72')).toBe(true)
    expect(await upgraded.queued()).toMatchObject([legacy])
  })

  it('never removes an id when another pull completes', async () => {
    await store.mergeRevoked([ticketIDForStore])
    await store.completeRevocationPull('fingerprint-a', '2026-09-25T12:00:00Z')
    await store.completeRevocationPull('fingerprint-b', '2026-09-25T12:15:00Z')
    expect(await store.isRevoked(ticketIDForStore)).toBe(true)
  })
})

const ticketIDForStore = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'

describe('actuation gating', () => {
  it('actuates exactly once for a pending record, marking before reporting', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    expect(await store.actuate(record.occurrenceId)).toBe(true)
    const after = await store.get(record.occurrenceId)
    expect(after?.actuated).toBe(true)
    expect(after?.state).toBe('ACTUATED')
  })

  it('never actuates a copied occurrence id this device holds no record for', async () => {
    expect(await store.actuate(crypto.randomUUID())).toBe(false)
  })

  it('never actuates an already-actuated occurrence twice', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    expect(await store.actuate(record.occurrenceId)).toBe(true)
    expect(await store.actuate(record.occurrenceId)).toBe(false)
  })

  it('serializes racing actuations of the same occurrence to a single winner', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    const outcomes = await Promise.all([
      store.actuate(record.occurrenceId),
      store.actuate(record.occurrenceId),
      store.actuate(record.occurrenceId),
    ])
    expect(outcomes.filter(Boolean)).toHaveLength(1)
  })
})

describe('offline queue', () => {
  it('recovers this tab after a reload once, with the same occurrence identity', async () => {
    const dbName = `reload-${crypto.randomUUID()}`
    const firstPage = await openStore(dbName, { ownerId: 'page-before-reload' })
    const stranded = await firstPage.mint('stranded-qr', '2026-09-03T20:00:00Z', 'entry')
    const actuated = await firstPage.mint('admitted-qr', '2026-09-03T20:01:00Z', 'entry')
    const resolved = await firstPage.mint('resolved-qr', '2026-09-03T20:02:00Z', 'entry')
    await firstPage.actuate(actuated.occurrenceId)
    await firstPage.markSynced(resolved.occurrenceId, 'conflict')

    expect(await firstPage.queued()).toEqual([])
    firstPage.close()

    const reloadedPage = await openStore(dbName, {
      ownerId: 'page-after-reload',
      recoverOwnerId: 'page-before-reload',
    })
    const queue = await reloadedPage.queued()
    expect(queue).toHaveLength(1)
    expect(queue[0]).toMatchObject({
      occurrenceId: stranded.occurrenceId,
      qrPayload: 'stranded-qr',
      occurredAt: '2026-09-03T20:00:00Z',
      state: 'QUEUED',
      actuated: false,
    })
    expect((await reloadedPage.queued()).map((record) => record.occurrenceId)).toEqual([
      stranded.occurrenceId,
    ])
    expect(await reloadedPage.get(actuated.occurrenceId)).toMatchObject({ state: 'ACTUATED', actuated: true })
    expect(await reloadedPage.get(resolved.occurrenceId)).toMatchObject({ state: 'RESOLVED', result: 'conflict' })
  })

  it('preserves direction through recovery and transitions', async () => {
    const dbName = `direction-${crypto.randomUUID()}`
    const firstPage = await openStore(dbName, { ownerId: 'page-before-reload' })
    const stranded = await firstPage.mint('stranded-exit', '2026-09-03T20:00:00Z', 'exit')
    const actuated = await firstPage.mint('admitted-exit', '2026-09-03T20:01:00Z', 'exit')
    const resolved = await firstPage.mint('resolved-exit', '2026-09-03T20:02:00Z', 'exit')
    expect(await firstPage.actuate(actuated.occurrenceId)).toBe(true)
    await firstPage.markSynced(resolved.occurrenceId, 'conflict')
    firstPage.close()

    const reloadedPage = await openStore(dbName, {
      ownerId: 'page-after-reload',
      recoverOwnerId: 'page-before-reload',
    })
    expect(await reloadedPage.queued()).toMatchObject([
      { occurrenceId: stranded.occurrenceId, state: 'QUEUED', direction: 'exit' },
    ])
    expect(await reloadedPage.get(actuated.occurrenceId)).toMatchObject({ state: 'ACTUATED', direction: 'exit' })
    expect(await reloadedPage.get(resolved.occurrenceId)).toMatchObject({ state: 'RESOLVED', direction: 'exit' })

    await reloadedPage.markQueued(stranded.occurrenceId)
    expect(await reloadedPage.get(stranded.occurrenceId)).toMatchObject({ state: 'QUEUED', direction: 'exit' })
    await reloadedPage.markSynced(stranded.occurrenceId, 'recorded')
    expect(await reloadedPage.get(stranded.occurrenceId)).toMatchObject({ state: 'SYNCED', direction: 'exit' })
  })

  it('retries reload recovery after the previous page releases its live-owner lock', async () => {
    const dbName = `overlapping-reload-${crypto.randomUUID()}`
    const firstPage = await openStore(dbName, { ownerId: 'page-before-reload' })
    const stranded = await firstPage.mint('stranded-qr', '2026-09-03T20:00:00Z', 'entry')

    const reloadedPage = await openStore(dbName, {
      ownerId: 'page-after-reload',
      recoverOwnerId: 'page-before-reload',
    })
    expect(await reloadedPage.queued()).toEqual([])

    firstPage.close()
    expect((await reloadedPage.queued()).map((record) => record.occurrenceId)).toEqual([
      stranded.occurrenceId,
    ])
    expect((await reloadedPage.queued()).map((record) => record.occurrenceId)).toEqual([
      stranded.occurrenceId,
    ])
  })

  it('does not let a second live tab claim or actuate an in-flight occurrence', async () => {
    const dbName = `tabs-${crypto.randomUUID()}`
    const firstTab = await openStore(dbName, { ownerId: 'first-tab', now: () => 1_000 })
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z', 'entry')

    const secondTab = await openStore(dbName, { ownerId: 'second-tab', now: () => 1_000 })
    expect(await secondTab.queued()).toEqual([])
    expect(await secondTab.actuate(pending.occurrenceId)).toBe(false)
    expect(await firstTab.actuate(pending.occurrenceId)).toBe(true)
  })

  it('renews a live owner before its original lease can expire', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const dbName = `heartbeat-${crypto.randomUUID()}`
    let timestamp = 1_000
    const firstTab = await openStore(dbName, {
      ownerId: 'first-tab',
      pendingLeaseMs: 90,
      now: () => timestamp,
    })
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z', 'entry')

    timestamp = 1_060
    await vi.advanceTimersByTimeAsync(30)
    timestamp = 1_091
    const secondTab = await openStore(dbName, {
      ownerId: 'second-tab',
      pendingLeaseMs: 90,
      now: () => timestamp,
    })

    expect(await secondTab.queued()).toEqual([])
    expect(await firstTab.actuate(pending.occurrenceId)).toBe(true)
  })

  it('uses the browser owner lock when heartbeat delivery is delayed past lease expiry', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const dbName = `delayed-heartbeat-${crypto.randomUUID()}`
    let timestamp = 1_000
    const firstTab = await openBrowserStore(dbName, {
      ownerId: 'slow-live-tab',
      pendingLeaseMs: 90,
      now: () => timestamp,
    })
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z', 'entry')

    // The page is alive and still owns its browser lock, but its timer has not
    // run. An expired timestamp alone must not let another tab take the row.
    timestamp = 1_091
    const secondTab = await openBrowserStore(dbName, {
      ownerId: 'observer-tab',
      pendingLeaseMs: 90,
      now: () => timestamp,
    })
    expect(await secondTab.queued()).toEqual([])
    expect(await firstTab.actuate(pending.occurrenceId)).toBe(true)

    // Repeat with another row, then release the owner. The expired row becomes
    // recoverable once, without changing its occurrence identity.
    const abandoned = await firstTab.mint('abandoned-qr', '2026-09-03T20:01:00Z', 'entry')
    timestamp = 1_182
    expect(await secondTab.queued()).toEqual([])
    firstTab.close()
    expect((await secondTab.queued()).map((record) => record.occurrenceId)).toEqual([
      abandoned.occurrenceId,
    ])
    expect((await secondTab.queued()).map((record) => record.occurrenceId)).toEqual([
      abandoned.occurrenceId,
    ])
  })

  it('recovers an abandoned owner only after its bounded lease expires', async () => {
    // Recovery compares the STORED expiry with the reader's clock. It does not
    // use the reader's lease, so the observers keep the default lease. Each store
    // has its own constant clock: a heartbeat renewal at any time writes 1_100
    // again (TKT-333: with one shared clock, a renewal in flight wrote 1_199).
    const dbName = `abandoned-${crypto.randomUUID()}`
    const abandoned = await openStore(dbName, {
      ownerId: 'abandoned-tab',
      pendingLeaseMs: 100,
      now: () => 1_000,
    })
    const pending = await abandoned.mint('abandoned-qr', '2026-09-03T20:00:00Z', 'entry')
    abandoned.close()

    const beforeExpiry = await openStore(dbName, { ownerId: 'observer-before-expiry', now: () => 1_099 })
    expect(await beforeExpiry.queued()).toEqual([])
    expect(await beforeExpiry.get(pending.occurrenceId)).toMatchObject({
      state: 'PENDING',
      ownerId: 'abandoned-tab',
      leaseExpiresAt: 1_100,
    })
    beforeExpiry.close()

    const afterExpiry = await openStore(dbName, { ownerId: 'observer-after-expiry', now: () => 1_100 })
    expect((await afterExpiry.queued()).map((record) => record.occurrenceId)).toEqual([
      pending.occurrenceId,
    ])
    expect((await afterExpiry.queued()).map((record) => record.occurrenceId)).toEqual([
      pending.occurrenceId,
    ])
  })

  it('queues an unsent occurrence and lists it for reconciliation', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    await store.markQueued(record.occurrenceId)
    const queued = await store.queued()
    expect(queued.map((r) => r.occurrenceId)).toEqual([record.occurrenceId])
  })

  it('moves a reconciled occurrence to a terminal state and out of the queue', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    await store.markQueued(record.occurrenceId)
    await store.markSynced(record.occurrenceId, 'recorded')
    expect(await store.queued()).toEqual([])
    const after = await store.get(record.occurrenceId)
    expect(after?.state).toBe('SYNCED')
    expect(after?.result).toBe('recorded')
  })

  it('marks a conflict as RESOLVED — surfaced to the operator, never a gate action', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z', 'entry')
    await store.markQueued(record.occurrenceId)
    await store.markSynced(record.occurrenceId, 'conflict')
    const after = await store.get(record.occurrenceId)
    expect(after?.state).toBe('RESOLVED')
    // A conflict result must never become actuatable.
    expect(await store.actuate(record.occurrenceId)).toBe(false)
  })
})

// Opens a raw connection the way a test or another tab does, with no handlers.
function rawOpen(dbName: string, version: number, upgrade?: (db: IDBDatabase) => void): Promise<IDBDatabase> {
  const open = indexedDB.open(dbName, version)
  if (upgrade) open.onupgradeneeded = () => upgrade(open.result)
  return new Promise((resolve, reject) => {
    open.onsuccess = () => resolve(open.result)
    open.onerror = () => reject(open.error)
  })
}

// Opens a later version and reports which event came first: blocked, or success.
function openLaterVersion(dbName: string, version: number): Promise<{ kind: 'blocked' } | { kind: 'opened'; db: IDBDatabase }> {
  const open = indexedDB.open(dbName, version)
  return new Promise((resolve, reject) => {
    open.onblocked = () => resolve({ kind: 'blocked' })
    open.onsuccess = () => resolve({ kind: 'opened', db: open.result })
    open.onerror = () => reject(open.error)
  })
}

function allRows(db: IDBDatabase): Promise<Array<Record<string, unknown>>> {
  const request = db.transaction('occurrences', 'readonly').objectStore('occurrences').getAll()
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

describe('version 3 barrier between scanner versions (TKT-399)', () => {
  // A tab still on the pre-TKT-399 code opens version 2 and has no versionchange
  // handler, so it never closes for an upgrade. It reconciles every QUEUED row
  // without a direction, so it would send this code's queued exits as entries.
  it('reports a blocked open while an older tab holds version 2, and keeps every row after it closes', async () => {
    const dbName = `barrier-${crypto.randomUUID()}`
    const olderTab = await rawOpen(dbName, 2, (db) => {
      db.createObjectStore('occurrences', { keyPath: 'occurrenceId' })
      db.createObjectStore('revocations', { keyPath: 'ticketID' })
      db.createObjectStore('revocation_meta', { keyPath: 'key' })
    })
    const legacy = {
      occurrenceId: crypto.randomUUID(), qrPayload: 'legacy-qr', occurredAt: '2026-10-01T18:00:00Z',
      state: 'QUEUED', actuated: false, createdAt: '2026-10-01T18:00:00Z',
    }
    const tx = olderTab.transaction('occurrences', 'readwrite')
    tx.objectStore('occurrences').put(legacy)
    await new Promise<void>((resolve, reject) => {
      tx.oncomplete = () => resolve()
      tx.onerror = () => reject(tx.error)
    })

    await expect(openStore(dbName)).rejects.toBeInstanceOf(OccurrenceStoreBlockedError)
    // A second open while the first still waits must not queue behind it. It would
    // wait until the older tab closes and then let a scan through late.
    const retry = openStore(dbName)
    olderTab.close()
    await expect(retry).rejects.toBeInstanceOf(OccurrenceStoreBlockedError)

    // The open that was reported blocked completes once the older tab closes. An
    // open queues behind it, so this one succeeds only after it has finished.
    const afterUpgrade = await rawOpen(dbName, 3)
    expect(afterUpgrade.version).toBe(3)
    expect([...afterUpgrade.objectStoreNames].sort()).toEqual(['occurrences', 'revocation_meta', 'revocations'])
    afterUpgrade.close()

    const reopened = await openStore(dbName)
    const queued = await reopened.queued()
    expect(queued).toEqual([legacy])
    // No own direction: the reconciliation omits event_type and Access reads an entry.
    expect(Object.hasOwn(queued[0], 'direction')).toBe(false)
    reopened.close()

    // The open that was reported blocked closed its own connection, so it does not
    // block a later version, and no open wrote a row.
    const later = await openLaterVersion(dbName, 4)
    expect(later.kind).toBe('opened')
    if (later.kind !== 'opened') return
    expect(await allRows(later.db)).toEqual([legacy])
    later.db.close()
  })

  it('closes itself when a later version opens, and then refuses every use', async () => {
    const dbName = `versionchange-${crypto.randomUUID()}`
    const current = await openStore(dbName, { ownerId: 'current-owner' })
    const inFlight = await current.mint('in-flight-qr', '2026-10-16T19:00:00Z', 'exit')

    const later = await openLaterVersion(dbName, 4)
    expect(later.kind).toBe('opened')
    if (later.kind !== 'opened') return
    // The row stays for the next page to recover and reconcile.
    expect(await allRows(later.db)).toEqual([expect.objectContaining({
      occurrenceId: inFlight.occurrenceId, state: 'PENDING', direction: 'exit',
    })])
    later.db.close()

    // The owner is released, so another page can recover the in-flight row.
    expect(await ownerLiveness.runIfOwnerAbsent('current-owner', async () => {})).toBe(true)
    const uses: Array<() => Promise<unknown>> = [
      () => current.mint('after-close-qr', '2026-10-16T19:01:00Z', 'entry'),
      () => current.mintRefusal('after-close-qr', '2026-10-16T19:01:00Z'),
      () => current.isRevoked(ticketIDForStore),
      () => current.mergeRevoked([ticketIDForStore]),
      () => current.completeRevocationPull('fingerprint', '2026-10-16T19:01:00Z'),
      () => current.revocationPull('fingerprint'),
      () => current.actuate(inFlight.occurrenceId),
      () => current.markQueued(inFlight.occurrenceId),
      () => current.markSynced(inFlight.occurrenceId, 'recorded'),
      () => current.queued(),
      () => current.get(inFlight.occurrenceId),
    ]
    for (const use of uses) await expect(use()).rejects.toBeInstanceOf(OccurrenceStoreClosedError)
  })

  it('refuses to open, and releases its owner, when a later version opens during the owner lock', async () => {
    const dbName = `during-hold-${crypto.randomUUID()}`
    let later: Awaited<ReturnType<typeof openLaterVersion>> | undefined
    const racing: OwnerLiveness = {
      async hold(ownerId) {
        later = await openLaterVersion(dbName, 4)
        return ownerLiveness.hold(ownerId)
      },
      runIfOwnerAbsent: (ownerId, task) => ownerLiveness.runIfOwnerAbsent(ownerId, task),
    }

    await expect(openOccurrenceStore(dbName, { ownerId: 'racing-owner', ownerLiveness: racing }))
      .rejects.toBeInstanceOf(OccurrenceStoreClosedError)
    expect(later?.kind).toBe('opened')
    if (later?.kind === 'opened') later.db.close()
    expect(await ownerLiveness.runIfOwnerAbsent('racing-owner', async () => {})).toBe(true)
  })

  it('refuses to open when a later version opens while it recovers rows', async () => {
    const dbName = `during-recovery-${crypto.randomUUID()}`
    const otherPage = await openStore(dbName, { ownerId: 'other-page' })
    await otherPage.mint('other-page-qr', '2026-10-16T19:00:00Z', 'entry')
    otherPage.close()
    let later: Awaited<ReturnType<typeof openLaterVersion>> | undefined
    const racing: OwnerLiveness = {
      hold: (ownerId) => ownerLiveness.hold(ownerId),
      async runIfOwnerAbsent(ownerId, task) {
        const ran = await ownerLiveness.runIfOwnerAbsent(ownerId, task)
        later = await openLaterVersion(dbName, 4)
        return ran
      },
    }

    await expect(openOccurrenceStore(dbName, { ownerId: 'recovering-page', ownerLiveness: racing }))
      .rejects.toBeInstanceOf(OccurrenceStoreClosedError)
    expect(later?.kind).toBe('opened')
    if (later?.kind === 'opened') later.db.close()
    expect(await ownerLiveness.runIfOwnerAbsent('recovering-page', async () => {})).toBe(true)
  })
})
