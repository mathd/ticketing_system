import 'fake-indexeddb/auto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
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
    const record = await store.mint('qr-payload', '2026-07-17T09:00:00Z')
    expect(record.state).toBe('PENDING')
    expect(record.actuated).toBe(false)
    // UUIDv4 shape — the id is scanner-minted (ADR-025 §D3).
    expect(record.occurrenceId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    // Durably readable back — mint resolves only after the IDB transaction commits.
    const reread = await store.get(record.occurrenceId)
    expect(reread?.state).toBe('PENDING')
  })

  it('mints a distinct id per decision — retries reuse, decisions do not', async () => {
    const a = await store.mint('qr-payload', '2026-07-17T09:00:00Z')
    const b = await store.mint('qr-payload', '2026-07-17T09:01:00Z')
    expect(a.occurrenceId).not.toBe(b.occurrenceId)
  })
})

describe('actuation gating', () => {
  it('actuates exactly once for a pending record, marking before reporting', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    expect(await store.actuate(record.occurrenceId)).toBe(true)
    const after = await store.get(record.occurrenceId)
    expect(after?.actuated).toBe(true)
    expect(after?.state).toBe('ACTUATED')
  })

  it('never actuates a copied occurrence id this device holds no record for', async () => {
    expect(await store.actuate(crypto.randomUUID())).toBe(false)
  })

  it('never actuates an already-actuated occurrence twice', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    expect(await store.actuate(record.occurrenceId)).toBe(true)
    expect(await store.actuate(record.occurrenceId)).toBe(false)
  })

  it('serializes racing actuations of the same occurrence to a single winner', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
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
    const stranded = await firstPage.mint('stranded-qr', '2026-09-03T20:00:00Z')
    const actuated = await firstPage.mint('admitted-qr', '2026-09-03T20:01:00Z')
    const resolved = await firstPage.mint('resolved-qr', '2026-09-03T20:02:00Z')
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

  it('retries reload recovery after the previous page releases its live-owner lock', async () => {
    const dbName = `overlapping-reload-${crypto.randomUUID()}`
    const firstPage = await openStore(dbName, { ownerId: 'page-before-reload' })
    const stranded = await firstPage.mint('stranded-qr', '2026-09-03T20:00:00Z')

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
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z')

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
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z')

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
    const pending = await firstTab.mint('in-flight-qr', '2026-09-03T20:00:00Z')

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
    const abandoned = await firstTab.mint('abandoned-qr', '2026-09-03T20:01:00Z')
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
    const dbName = `abandoned-${crypto.randomUUID()}`
    let timestamp = 1_000
    const abandoned = await openStore(dbName, {
      ownerId: 'abandoned-tab',
      pendingLeaseMs: 100,
      now: () => timestamp,
    })
    const pending = await abandoned.mint('abandoned-qr', '2026-09-03T20:00:00Z')
    abandoned.close()

    timestamp = 1_099
    const beforeExpiry = await openStore(dbName, { ownerId: 'observer-before-expiry', now: () => timestamp })
    expect(await beforeExpiry.queued()).toEqual([])
    beforeExpiry.close()

    timestamp = 1_100
    const afterExpiry = await openStore(dbName, { ownerId: 'observer-after-expiry', now: () => timestamp })
    expect((await afterExpiry.queued()).map((record) => record.occurrenceId)).toEqual([
      pending.occurrenceId,
    ])
    expect((await afterExpiry.queued()).map((record) => record.occurrenceId)).toEqual([
      pending.occurrenceId,
    ])
  })

  it('queues an unsent occurrence and lists it for reconciliation', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    await store.markQueued(record.occurrenceId)
    const queued = await store.queued()
    expect(queued.map((r) => r.occurrenceId)).toEqual([record.occurrenceId])
  })

  it('moves a reconciled occurrence to a terminal state and out of the queue', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    await store.markQueued(record.occurrenceId)
    await store.markSynced(record.occurrenceId, 'recorded')
    expect(await store.queued()).toEqual([])
    const after = await store.get(record.occurrenceId)
    expect(after?.state).toBe('SYNCED')
    expect(after?.result).toBe('recorded')
  })

  it('marks a conflict as RESOLVED — surfaced to the operator, never a gate action', async () => {
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    await store.markQueued(record.occurrenceId)
    await store.markSynced(record.occurrenceId, 'conflict')
    const after = await store.get(record.occurrenceId)
    expect(after?.state).toBe('RESOLVED')
    // A conflict result must never become actuatable.
    expect(await store.actuate(record.occurrenceId)).toBe(false)
  })
})
