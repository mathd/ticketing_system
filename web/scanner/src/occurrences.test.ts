import 'fake-indexeddb/auto'
import { beforeEach, describe, expect, it } from 'vitest'
import { openOccurrenceStore, type OccurrenceStore } from './occurrences'

// The ADR-025 §D3 actuation floor: a response — first-time or replay — may
// actuate the gate iff THIS device holds a durably pending, never-actuated
// record for that occurrence id. Copied ids never actuate; a lost-response
// retry completes exactly once; marking happens before the gate opens.

let store: OccurrenceStore

beforeEach(async () => {
  store = await openOccurrenceStore(`test-${crypto.randomUUID()}`)
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

  it('lets a lost-response retry complete exactly once', async () => {
    // The response to the first send was lost: the record is still pending and
    // un-actuated when the replay result arrives — it may actuate, once.
    const record = await store.mint('qr', '2026-07-17T09:00:00Z')
    expect(await store.actuate(record.occurrenceId)).toBe(true)
    // A second identical replay response is a no-op.
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
