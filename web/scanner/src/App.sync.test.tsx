import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { forceCloseDatabase } from 'fake-indexeddb'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { Activity, StrictMode } from 'react'
import App, { reconcileTimeoutMs } from './App'
import type { CurrentRecord as OccurrenceRecord } from './occurrence-model'
import type { QueuedEntry } from './occurrence-store'
import type { StorageController } from './occurrence-readiness'
import { BOUND_MS, bounded, createTestPage, destroyAllPages, mintOn, type TestPage } from './test/page-fixture'
import { rawGetAll, rawPut, seedDatabase, V2_STORES } from './test/raw-db'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => {
  const actual = await load<typeof import('./occurrence-readiness')>()
  return { ...actual, get pageStorage() { return runtime.storage! } }
})
let page: TestPage
let dbName: string


// Controls the failure-path read without changing the stored rows.
const queueReads = vi.hoisted(() => ({
  started: 0,
  finished: 0,
  failNext: false,
  failMarkSyncedNext: false,
  onStarted: null as (() => void) | null,
  holdNext: null as { result: Promise<OccurrenceRecord[]>; started: () => void } | null,
}))

function wrapQueueReads() {
  const store = page.store()
  const apply = store.applyReconcileResults.bind(store)
  vi.spyOn(store, 'applyReconcileResults').mockImplementation(async (...args) => {
    if (queueReads.failMarkSyncedNext) { queueReads.failMarkSyncedNext = false; throw new Error('terminal write failed') }
    return apply(...args)
  })
  const queued = store.queued.bind(store)
  vi.spyOn(store, 'queued').mockImplementation(async () => {
    queueReads.started += 1
    queueReads.onStarted?.()
    try {
      if (queueReads.failNext) { queueReads.failNext = false; throw new Error('queue read failed') }
      const held = queueReads.holdNext
      if (held) {
        queueReads.holdNext = null; held.started()
        const records = await held.result
        return { entries: records.map((record): QueuedEntry => ({ format: 1, key: record.occurrenceId, record })), warnings: [] }
      }
      return await queued()
    } finally { queueReads.finished += 1 }
  })
}

// TKT-315: a failed sync of the offline queue must SAY so. It used to return silently on
// a 5xx and swallow a rejected fetch or an unreadable body in a bare catch, so a venue
// could not tell "all reconciled" from "failing for three hours". Every failure leaves the
// queue exactly as it was (ADR-066): these tests compare the stored rows, not only the text.

const feedResponse = () => new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
const checkingOfflineNote = 'Sync failed: No connection. Checking the scans saved on this device…'
const unknownOfflineNote = 'Sync failed: No connection. Any scans saved on this device will be sent on the next sync. Last successful sync this session: none.'
const offlineNote = /Sync failed: No connection\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./
const unreadableNote = /Sync failed: The server answered but the reply could not be read\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./
const unknownUnreadableNote = 'Sync failed: The server answered but the reply could not be read. Any scans saved on this device will be sent on the next sync. Last successful sync this session: none.'
const unreadableTwoNote = /Sync failed: The server answered but the reply could not be read\. 2 scans are saved on this device and will be sent on the next sync\. Last successful sync this session: none\./
// TKT-512 copy. Each message says what did and did not happen. None claims a storage outage.
const writeDiagnostic = 'The last sync result could not be saved on this device, so that batch was not applied. Scans from that batch that are still queued will be retried on a later sync.'
const refusedDiagnostic = 'A scan in the last sync batch changed on this device, so that batch was not applied. Scans that are still queued will be retried on a later sync.'
const pairingError = 'Pairing could not be completed on this device. Submit the pairing token again to retry.'
const waitingNote = 'Waiting for local storage to finish before this sync can continue.'
const readAlert = 'Storage is unavailable or incompatible. Keep this page open and retry storage.'
const corruptWarning = 'Local record corrupt-row cannot be read: record type is not exactly one of entry, exit or refusal. It stays on this device and will not sync.'
const REPAIR_NEEDED_TEXT = 'The outcome write needs repair. Keep this page open and retry storage.'

const mixedSyncNote = 'Synced 92 offline scan(s). 7 conflicts flagged for the operator. 6 rejected scans were not recorded.'
const partialSyncNote = 'Completed batches: Synced 90 offline scan(s). 6 conflicts flagged for the operator. 4 rejected scans were not recorded.'
type SyncResult = 'recorded' | 'synced' | 'conflict' | 'rejected'
type SyncFixture = { before: OccurrenceRecord[]; results: Map<string, SyncResult> }

async function queueMixedBatches(): Promise<SyncFixture> {
  // The literal partition sets both the 100+5 boundary and the expected 92/7/6 totals.
  const partition: SyncResult[] = [
    ...Array<SyncResult>(45).fill('recorded'), ...Array<SyncResult>(45).fill('synced'),
    ...Array<SyncResult>(6).fill('conflict'), ...Array<SyncResult>(4).fill('rejected'),
    'recorded', 'synced', 'conflict', 'rejected', 'rejected',
  ]
  const results = new Map<string, SyncResult>()
  for (const [index, result] of partition.entries()) {
    const kind = index % 3 === 0 ? 'refusal' : index % 3 === 1 ? 'entry' : 'exit'
    const instant = Date.parse('2026-10-20T12:00:00Z') + index * 1000
    const { scan } = await mintOn(page, {
      occurrenceId: `00000000-0551-4000-8000-${String(index + 1).padStart(12, '0')}`,
      occurredAt: new Date(instant).toISOString(), createdAt: new Date(instant + 1).toISOString(),
      ...(kind === 'refusal' ? { type: 'refusal' } : { direction: kind }),
    })
    page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId,
      outcome: { family: 'queue', cause: kind === 'refusal' ? 'local_refusal' : 'transport_failure' },
      decidedAt: new Date(instant + 2).toISOString() })
    await page.coordinator.persist(scan.occurrenceId)
    results.set(scan.occurrenceId, result)
  }
  const before = await storedRows() as OccurrenceRecord[]
  expect(before).toHaveLength(105)
  expect(before.every(row => row.state === 'QUEUED')).toBe(true)
  expect(before[98]).toMatchObject({ direction: 'exit' })
  expect(before[104]).toMatchObject({ direction: 'exit' })
  expect(before[99]).toMatchObject({ localDecision: 'revocation_refused' })
  return { before, results }
}

function mixedResponse(body: ReconcileBody, fixture: SyncFixture) {
  return Response.json({ results: body.occurrences.map(row => {
    const result = fixture.results.get(row.occurrence_id)
    expect(result).toBeDefined()
    return { occurrence_id: row.occurrence_id, result }
  }) })
}

function terminalRow(row: OccurrenceRecord, result: SyncResult): OccurrenceRecord {
  return { ...row, state: result === 'recorded' || result === 'synced' ? 'SYNCED' : 'RESOLVED', result }
}

function expectedMixedRows(fixture: SyncFixture, count: number) {
  return fixture.before.map((row, index) => index < count ? terminalRow(row, fixture.results.get(row.occurrenceId)!) : row)
}

function expectRejectionWarnings(fixture: SyncFixture, count: number) {
  const ids = fixture.before.slice(0, count).filter(row => fixture.results.get(row.occurrenceId) === 'rejected').map(row => row.occurrenceId)
  const warnings = screen.queryAllByText(/^Reconciliation rejected for /)
  expect(warnings.map(node => node.textContent)).toStrictEqual(ids.map(id => `Reconciliation rejected for ${id}. This result is not recorded.`))
}

async function startMixedSync() {
  const fixture = await queueMixedBatches()
  const held = holdReconciles()
  render(<App />)
  const button = await screen.findByRole('button', { name: 'Sync queued scans' }, { timeout: BOUND_MS })
  await readsSettled()
  expect(screen.getByText(/^105 queued offline scans awaiting sync\./)).toBeDefined()
  fireEvent.click(button)
  const first = await bounded(held.next(), 'first mixed batch')
  expect(first.body.occurrences.map(row => row.occurrence_id)).toStrictEqual(fixture.before.slice(0, 100).map(row => row.occurrenceId))
  await act(async () => { first.answer(mixedResponse(first.body, fixture)) })
  const second = await bounded(held.next(), 'second mixed batch')
  expect(second.body.occurrences.map(row => row.occurrence_id)).toStrictEqual(fixture.before.slice(100).map(row => row.occurrenceId))
  return { fixture, held, second }
}

async function expectPartial(fixture: SyncFixture) {
  await screen.findByText(partialSyncNote, {}, { timeout: BOUND_MS })
  expectRejectionWarnings(fixture, 100)
  expect(screen.getAllByRole('alert')[0].textContent).toBe(partialSyncNote)
  await waitFor(async () => expect(await storedRows()).toStrictEqual(expectedMixedRows(fixture, 100)), { timeout: BOUND_MS })
}

describe('aggregate reconciliation reporting', () => {
  it.each([false, true])('counts every result across real batches, including matching retired rows: %s', async alreadyApplied => {
    const { fixture, second } = await startMixedSync()
    if (alreadyApplied) {
      const rows = fixture.before.slice(100)
      const report = await page.store().applyReconcileResults(rows.map(row => row.occurrenceId), rows.map(row => ({ occurrence_id: row.occurrenceId, result: fixture.results.get(row.occurrenceId)! })))
      expect(report).toStrictEqual({ status: 'applied', retired: 5, alreadyApplied: 0 })
    }
    await act(async () => { second.answer(mixedResponse(second.body, fixture)) })
    await screen.findByText(mixedSyncNote, {}, { timeout: BOUND_MS })
    expectRejectionWarnings(fixture, 105)
    expect(screen.queryByText(/^Completed batches:/)).toBeNull()
    await waitFor(async () => expect(await storedRows()).toStrictEqual(expectedMixedRows(fixture, 105)), { timeout: BOUND_MS })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull(), { timeout: BOUND_MS })
  }, 15_000)

  it.each(['transport', '500', '401', 'malformed', 'write', 'incompatible'] as const)('retains applied-batch totals when the next batch fails: %s', async failure => {
    const { fixture, second } = await startMixedSync()
    await expectPartial(fixture)
    let expected = expectedMixedRows(fixture, 100)
    if (failure === 'incompatible') {
      const row = fixture.before[103]
      expect(fixture.results.get(row.occurrenceId)).toBe('rejected')
      expect(await page.store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])).toStrictEqual({ status: 'applied', retired: 1, alreadyApplied: 0 })
      expected = expected.map(item => item.occurrenceId === row.occurrenceId ? terminalRow(row, 'recorded') : item)
    }
    if (failure === 'write') queueReads.failMarkSyncedNext = true
    await act(async () => {
      if (failure === 'transport') second.fail(new TypeError('network down'))
      else if (failure === '500' || failure === '401') second.answer(new Response('', { status: Number(failure) }))
      else if (failure === 'malformed') second.answer(Response.json({ results: [] }))
      else second.answer(mixedResponse(second.body, fixture))
    })
    if (failure === '401') {
      await screen.findByText('This device is not paired. Enter its pairing token to sync the queued scans.', {}, { timeout: BOUND_MS })
      // D10 pins this stale display, not queue eligibility. See docs/development.md.
      expect(screen.getByText('105 offline scans are still saved on this device and will sync once it is paired.')).toBeDefined()
    } else if (failure === 'write') {
      await screen.findByText(writeDiagnostic, {}, { timeout: BOUND_MS })
      // D10 pins this stale display, not queue eligibility. See docs/development.md.
      expect(screen.getByText(/^105 queued offline scans awaiting sync\./)).toBeDefined()
    } else if (failure === 'incompatible') {
      // The refused batch changed no row, so the refresh counts the four rows the store still holds.
      await waitFor(() => expect(screen.getByText(/^4 queued offline scans awaiting sync\./)).toBeDefined(), { timeout: BOUND_MS })
      // Only after that refresh: a refused batch is not a storage outage, and it adds no diagnostic of its own.
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByText(writeDiagnostic)).toBeNull()
      expect(screen.queryByText(refusedDiagnostic)).toBeNull()
    } else {
      const cause = failure === 'transport' ? 'No connection' : 'The server answered but the reply could not be read'
      await screen.findByText(`Sync failed: ${cause}. 5 scans are saved on this device and will be sent on the next sync. Last successful sync this session: none.`, {}, { timeout: BOUND_MS })
    }
    expect(screen.getByText(partialSyncNote)).toBeDefined()
    expectRejectionWarnings(fixture, 100)
    expect(screen.queryByText(mixedSyncNote)).toBeNull()
    await waitFor(async () => expect(await storedRows()).toStrictEqual(expected), { timeout: BOUND_MS })
  }, 15_000)

  it('replaces the partial report at automatic retry start even if that retry applies nothing', async () => {
    const { fixture, held, second } = await startMixedSync()
    await expectPartial(fixture)
    await act(async () => { second.fail(new TypeError('connection lost')) })
    await screen.findByText(/^Sync failed: No connection\. 5 scans are saved/, {}, { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.getByText(partialSyncNote)).toBeDefined()
    await act(async () => { window.dispatchEvent(new Event('online')) })
    const retry = await bounded(held.next(), 'automatic retry')
    expect(retry.body.occurrences.map(row => row.occurrence_id)).toStrictEqual(fixture.before.slice(100).map(row => row.occurrenceId))
    await waitFor(() => expect(screen.queryByText(partialSyncNote)).toBeNull(), { timeout: BOUND_MS })
    expectRejectionWarnings(fixture, 0)
    await act(async () => { retry.fail(new TypeError('still unavailable')) })
    await readsSettled()
    expect(screen.getByText(/^Sync failed: No connection\. 5 scans are saved/)).toBeDefined()
    expect(screen.queryByText(/^Completed batches:/)).toBeNull()
    expectRejectionWarnings(fixture, 0)
    expect(await storedRows()).toStrictEqual(expectedMixedRows(fixture, 100))
  }, 15_000)

  it('keeps the final report and rejection warning when an online retry reads an empty queue', async () => {
    const reconcile = vi.fn((body: ReconcileBody) => Promise.resolve(Response.json({
      results: body.occurrences.map(row => ({ occurrence_id: row.occurrence_id, result: 'rejected' })),
    })))
    stubFetch(reconcile)
    render(<App />)
    await queueOneScan('empty-retry-ticket')
    const before = await storedRows() as OccurrenceRecord[]
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const note = 'Synced 0 offline scan(s). 1 rejected scan was not recorded.'
    await screen.findByText(note, {}, { timeout: BOUND_MS })
    await readsSettled()
    expect(await page.store().queued()).toStrictEqual({ entries: [], warnings: [] })
    const reads = queueReads.started
    await act(async () => { window.dispatchEvent(new Event('online')) })
    await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.getByText(note)).toBeDefined()
    expect(screen.getByText(`Reconciliation rejected for ${before[0].occurrenceId}. This result is not recorded.`)).toBeDefined()
    expect(reconcile).toHaveBeenCalledTimes(1)
    expect(await storedRows()).toStrictEqual([terminalRow(before[0], 'rejected')])
  })

  it('reports a single rejected result and preserves recorded-only copy on the next successful pass', async () => {
    stubFetch(body => Promise.resolve(Response.json({ results: body.occurrences.map(row => ({ occurrence_id: row.occurrence_id, result: 'rejected' })) })))
    render(<App />)
    await queueOneScan('rejected-ticket')
    const before = await storedRows() as OccurrenceRecord[]
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 0 offline scan(s). 1 rejected scan was not recorded.', {}, { timeout: BOUND_MS })
    await waitFor(async () => expect(await storedRows()).toStrictEqual([terminalRow(before[0], 'rejected')]), { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.getByText(`Reconciliation rejected for ${before[0].occurrenceId}. This result is not recorded.`)).toBeDefined()
    await queueOneScan('recorded-next')
    const beforeNext = await storedRows() as OccurrenceRecord[]
    stubFetch(body => Promise.resolve(recordedResponse(body)))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    expect(screen.queryByText(/^Reconciliation rejected for /)).toBeNull()
    expect(screen.queryByText(/^Completed batches:/)).toBeNull()
    expect(await storedRows()).toStrictEqual(beforeNext.map(row => row.qrPayload === 'recorded-next' ? terminalRow(row, 'recorded') : row))
  })
})

beforeEach(async () => {
  dbName = `app-sync-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  wrapQueueReads()
  // A read held by a failed test never finishes. Counting from zero keeps it out of the next test's settle wait.
  queueReads.started = 0
  queueReads.finished = 0
  queueReads.failNext = false
  queueReads.failMarkSyncedNext = false
  queueReads.holdNext = null
  queueReads.onStarted = null
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  destroyAllPages()
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

type ReconcileBody = { occurrences: { occurrence_id: string }[] }
type Reconcile = (body: ReconcileBody, init: RequestInit) => Promise<Response>

// Every fetch the page makes: the revocation feed always answers, a scan is always offline
// (that is how a row gets queued), and the reconcile answer is the test's.
function stubFetch(reconcile: Reconcile) {
  const fetchMock = vi.fn((url: string | Request, init?: RequestInit) => {
    const u = typeof url === 'string' ? url : url.url
    if (u.includes('voided-tickets')) return Promise.resolve(feedResponse())
    if (u.endsWith('/reconciliations')) return reconcile(JSON.parse(init!.body as string), init!)
    return Promise.reject(new TypeError('Failed to fetch'))
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// A reconcile request the test answers by hand. It waits in `held` until the test calls answer or
// fail on it, so the order of the answers is the test's choice, not the timer's.
type HeldReconcile = { body: ReconcileBody; answer: (response: Response) => void; fail: (error: Error) => void }

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

function holdReconciles() {
  const held: HeldReconcile[] = []
  const arrivals: ReturnType<typeof deferred<HeldReconcile>>[] = []
  let next = 0
  stubFetch((body) => {
    const response = deferred<Response>()
    const request = { body, answer: response.resolve, fail: response.reject }
    held.push(request)
    arrivals[held.length - 1]?.resolve(request)
    return response.promise
  })
  return Object.assign(held, {
    next: () => {
      const index = next++
      if (held[index]) return Promise.resolve(held[index])
      arrivals[index] = deferred<HeldReconcile>()
      return arrivals[index].promise
    },
  })
}

function holdNextQueueRead() {
  const result = deferred<OccurrenceRecord[]>()
  const started = deferred<void>()
  queueReads.holdNext = { result: result.promise, started: () => started.resolve() }
  return { result, started: started.promise }
}

// A valid reconcile answer that records every occurrence the page sent.
function recordedResponse(body: ReconcileBody) {
  return new Response(JSON.stringify({
    results: body.occurrences.map((o) => ({ occurrence_id: o.occurrence_id, result: 'recorded' })),
  }), { status: 200 })
}

// Database barriers let pending work finish without a timed sleep.
async function settle() {
  await act(async () => {
    await storedRows()
    await storedRows()
  })
}

// Waits for all queue reads, then renders their corrections.
async function readsSettled() {
  await settle()
  await waitFor(() => expect(queueReads.finished).toBe(queueReads.started))
  await settle()
}

async function queueOneScan(value = 'offline-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
  await screen.findByRole('heading', { name: 'Offline decision' })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  await waitFor(async () => expect((await storedRows() as Array<{ qrPayload?: string; state?: string }>).some(
    row => row.qrPayload === value && row.state === 'QUEUED',
  )).toBe(true))
  await readsSettled()
}

// The stored occurrence rows, read straight from IndexedDB — not through the app.
async function storedRows(): Promise<unknown[]> { return rawGetAll(dbName, STORE.occurrences) }

async function markStoredRowsSynced() {
  const queue = (await page.store().queued()).entries
  const keys = queue.map((entry) => entry.key)
  await page.store().applyReconcileResults(keys, keys.map((key) => ({ occurrence_id: String(key), result: 'recorded' })))
}

async function failSyncWith(reconcile: Reconcile, note: RegExp) {
  render(<App />)
  stubFetch(() => Promise.reject(new TypeError('not yet')))
  await queueOneScan()
  const before = await storedRows()
  expect(before).toHaveLength(1)
  const fetchMock = stubFetch(reconcile)
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  expect(await screen.findByText(note)).toBeDefined()
  expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(true)
  // COS4: the queue is untouched, row for row.
  expect(await storedRows()).toEqual(before)
  expect(screen.queryByText(/^Completed batches:/)).toBeNull()
  return before
}

describe('a failed sync of the offline queue', () => {
  it('says "No connection" when the request never reached a server (COS1)', async () => {
    await failSyncWith(() => Promise.reject(new TypeError('network down')), offlineNote)
  })

  it('says the reply was unreadable on a non-JSON 502 (COS2)', async () => {
    await failSyncWith(() => Promise.resolve(new Response('<html>Bad Gateway</html>', { status: 502 })), unreadableNote)
  })

  it('preserves both complete rows on an HTML 502 (COS4)', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('ticket-x')
    await queueOneScan('ticket-y')
    await readsSettled()
    const before = await storedRows()
    expect(before).toHaveLength(2)
    stubFetch(() => Promise.resolve(new Response('<html>Bad Gateway</html>', { status: 502 })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText(/^Sync failed: .* Last successful sync this session:/)
    expect(await storedRows()).toEqual(before)
    expect(screen.getByText('Sync failed: The server answered but the reply could not be read. 2 scans are saved on this device and will be sent on the next sync. Last successful sync this session: none.')).toBeDefined()
  })

  it('says the reply was unreadable on a 2xx that is not JSON (COS3)', async () => {
    await failSyncWith(() => Promise.resolve(new Response('not json', { status: 200 })), unreadableNote)
  })

  it('says the reply was unreadable on a 2xx the decoder refuses — a result for another occurrence (COS3)', async () => {
    await failSyncWith(
      () => Promise.resolve(new Response(JSON.stringify({ results: [{ occurrence_id: '00000000-0000-4000-8000-000000000000', result: 'accepted' }] }), { status: 200 })),
      unreadableNote,
    )
  })

  it('is replaced by the success line once a sync succeeds, and the next failure shows when that was (COS5)', async () => {
    await failSyncWith(() => Promise.resolve(new Response('', { status: 502 })), unreadableNote)
    stubFetch((body) => Promise.resolve(new Response(JSON.stringify({
      results: body.occurrences.map((o) => ({ occurrence_id: o.occurrence_id, result: 'recorded' })),
    }), { status: 200 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText('Synced 1 offline scan(s).')).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()

    // A new queued scan, a new failure: the note now names the last successful sync.
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('second-ticket')
    stubFetch(() => Promise.reject(new TypeError('network down')))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    const note = await screen.findByText(/^Sync failed: No connection\. 1 scan is saved on this device/)
    expect(note.textContent).toMatch(/Last successful sync this session: (?!none).+\.$/)
  })

  it('clears a standing failure note when a later sync finds nothing left to send', async () => {
    await failSyncWith(() => Promise.reject(new TypeError('network down')), offlineNote)
    // Another tab syncs the row behind this page's back. This page's next sync finds the
    // queue empty and sends nothing: its note, "1 scan is saved on this device", is no
    // longer true and must go, with the count.
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
    await markStoredRowsSynced()
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    // Waits for the change itself, so it cannot pass before the sync has run.
    await waitFor(() => expect(screen.queryByText(/^Sync failed:/)).toBeNull())
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(false)
  })

  it('reports a terminal-write failure without publishing success or its time', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const before = await storedRows()
    stubFetch((body) => Promise.resolve(recordedResponse(body)))
    queueReads.failMarkSyncedNext = true
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(writeDiagnostic)).toBeDefined()
    expect(screen.queryByText(readAlert)).toBeNull()
    expect(queueReads.failMarkSyncedNext).toBe(false)
    expect(screen.queryByText(/^Synced /)).toBeNull()
    expect(await storedRows()).toEqual(before)

    stubFetch(() => Promise.reject(new TypeError('network down')))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(offlineNote)).toBeDefined()
    // The next nonempty sync's first request replaces the write diagnostic before it is sent.
    expect(screen.queryByText(writeDiagnostic)).toBeNull()
  })

  it('reads the exact last-success time through the first-render online listener', async () => {
    const successfulAt = Date.parse('2026-10-09T16:23:45Z')
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(successfulAt)
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    stubFetch((body) => Promise.resolve(recordedResponse(body)))
    window.dispatchEvent(new Event('online'))
    expect(await screen.findByText('Synced 1 offline scan(s).')).toBeDefined()
    await readsSettled()

    vi.setSystemTime(successfulAt + 60_000)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('second-ticket')
    await readsSettled()
    stubFetch(() => Promise.reject(new TypeError('network down')))
    window.dispatchEvent(new Event('online'))
    expect(await screen.findByText(`Sync failed: No connection. 1 scan is saved on this device and will be sent on the next sync. Last successful sync this session: ${new Date(successfulAt).toLocaleString()}.`)).toBeDefined()
  })

  it('serialises requests and coalesces an online event and a button click into one follow-up', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    window.dispatchEvent(new Event('online'))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await settle()
    expect(held).toHaveLength(1)
    first.fail(new TypeError('network down'))
    const second = await held.next()
    expect(held).toHaveLength(2)
    second.answer(recordedResponse(second.body))
    await readsSettled()
    expect(screen.getByText('Synced 1 offline scan(s).')).toBeDefined()
    expect(held).toHaveLength(2)
  })

  it('sends only Y in the follow-up after A succeeds for X, then reports Y failing', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('ticket-x')
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    await queueOneScan('ticket-y')
    await readsSettled()
    const rows = await storedRows() as OccurrenceRecord[]
    const y = rows.find((row) => row.qrPayload === 'ticket-y')!
    window.dispatchEvent(new Event('online'))
    await settle()
    expect(held).toHaveLength(1)
    first.answer(recordedResponse(first.body))
    const second = await held.next()
    expect(second.body.occurrences.map((row) => row.occurrence_id)).toEqual([y.occurrenceId])
    second.fail(new TypeError('network down'))
    await readsSettled()
    expect(screen.getByText(/^Sync failed: No connection\. 1 scan is saved on this device/)).toBeDefined()
    expect(screen.queryByText('Synced 1 offline scan(s).')).toBeNull()
  })

  it('a reconcile deadline shows no connection and releases the requested follow-up', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const before = await storedRows()
    const started = deferred<AbortSignal>()
    const followUp = deferred<void>()
    let calls = 0
    stubFetch((_, init) => {
      calls += 1
      if (calls > 1) {
        followUp.resolve()
        return new Promise<Response>(() => {})
      }
      return new Promise<Response>((_, reject) => {
        init.signal!.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
        started.resolve(init.signal!)
      })
    })
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const signal = await started.promise
    window.dispatchEvent(new Event('online'))
    await act(async () => { await vi.advanceTimersByTimeAsync(reconcileTimeoutMs) })
    vi.useRealTimers()
    expect(await storedRows()).toEqual(before)
    expect(await screen.findByText(offlineNote)).toBeDefined()
    expect(signal.aborted).toBe(true)
    await followUp.promise
    expect(calls).toBe(2)
  })

  it('a deadline during the response body shows unreadable', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const before = await storedRows()
    const bodyStarted = deferred<void>()
    stubFetch((_, init) => Promise.resolve({
      ok: true,
      status: 200,
      json: () => new Promise((_, reject) => {
        init.signal!.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
        bodyStarted.resolve()
      }),
    } as Response))
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await bodyStarted.promise
    await act(async () => { await vi.advanceTimersByTimeAsync(reconcileTimeoutMs) })
    expect(await storedRows()).toEqual(before)
    vi.useRealTimers()
    expect(await screen.findByText(unreadableNote)).toBeDefined()
  })

  it('shows no count before the failure queue read settles, then clears it for an empty queue', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const read = holdNextQueueRead()
    await act(async () => {
      first.fail(new TypeError('network down'))
      await read.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    expect(screen.queryByText(/\d+ scans? (is|are) saved/)).toBeNull()
    expect(queueReads.finished).toBe(queueReads.started - 1)
    await act(async () => { read.result.resolve([]) })
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
  })

  it('corrects a failure through pairing when its queue read resolves empty', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const read = holdNextQueueRead()
    await act(async () => {
      first.fail(new TypeError('network down'))
      await read.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    const revocation = deferred<Response>()
    const pullStarted = deferred<void>()
    vi.stubGlobal('fetch', vi.fn(() => {
      pullStarted.resolve()
      return revocation.promise
    }))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Refresh revocation list' }))
      await pullStarted.promise
      revocation.resolve(new Response('', { status: 401 }))
    })
    expect(screen.getByLabelText(/pairing token/i)).toBeDefined()
    await act(async () => { read.result.resolve([]) })
    vi.useRealTimers()
    stubFetch(() => Promise.reject(new TypeError('must not be called')))
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
    expect(await screen.findByRole('button', { name: 'Check ticket' })).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
  })

  it('corrects a failure after an intervening render in StrictMode', async () => {
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    const startup = deferred<void>()
    queueReads.onStarted = () => startup.resolve()
    const view = render(<StrictMode><App /></StrictMode>)
    await startup.promise
    queueReads.onStarted = null
    await readsSettled()
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'another-ticket' } })
    // Render with no pending input update to reach React's eager updater path.
    view.rerender(<StrictMode><App /></StrictMode>)
    const read = holdNextQueueRead()
    await act(async () => {
      first.fail(new TypeError('network down'))
      await read.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    await act(async () => { read.result.resolve([]) })
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
  })

  it('a delayed failure correction does not overwrite a newer follow-up failure', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('older-a')
    await queueOneScan('older-b')
    await readsSettled()
    const [rowA, rowB] = await storedRows() as OccurrenceRecord[]
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    // The first failure's correction reads both rows. That read is held, so its old two-row result can arrive last.
    const olderCorrection = holdNextQueueRead()
    await act(async () => {
      first.fail(new TypeError('network down'))
      await olderCorrection.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    // Another tab retires B through a second page on the same database. The follow-up sync sends only A and fails.
    // Its own correction reads one row.
    const other = createTestPage({ dbName })
    await other.ready()
    await other.store().applyReconcileResults([rowB.occurrenceId], [{ occurrence_id: rowB.occurrenceId, result: 'recorded' }])
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const second = await held.next()
    expect(second.body.occurrences.map((row) => row.occurrence_id)).toStrictEqual([rowA.occurrenceId])
    const newerCorrection = holdNextQueueRead()
    await act(async () => {
      second.answer(new Response('', { status: 502 }))
      await newerCorrection.started
    })
    expect(screen.getByText('Sync failed: The server answered but the reply could not be read. Checking the scans saved on this device…')).toBeDefined()
    await act(async () => { newerCorrection.result.resolve([rowA]) })
    expect(screen.getByText(unreadableNote)).toBeDefined()
    // The older correction now resolves with its two-row snapshot. It must not replace the newer count or failure.
    await act(async () => { olderCorrection.result.resolve([rowA, rowB]) })
    await settle()
    expect(screen.getByText(unreadableNote)).toBeDefined()
    expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    expect(await storedRows()).toStrictEqual([rowA, terminalRow(rowB, 'recorded')])
  })

  it('uses the fresh count when another scan was queued before the failure', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('ticket-x')
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    await queueOneScan('ticket-y')
    await readsSettled()
    const rows = await storedRows() as OccurrenceRecord[]
    const read = holdNextQueueRead()
    await act(async () => {
      first.fail(new TypeError('network down'))
      await read.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    await act(async () => { read.result.resolve(rows) })
    expect(screen.getByText(/^Sync failed: No connection\. 2 scans are saved on this device/)).toBeDefined()
  })

  it('a delayed failure correction does not replace a newer follow-up success', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    const staleRows = await storedRows() as OccurrenceRecord[]
    const read = holdNextQueueRead()
    window.dispatchEvent(new Event('online'))
    await act(async () => {
      first.fail(new TypeError('network down'))
      await read.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    const second = await held.next()
    second.answer(recordedResponse(second.body))
    await settle()
    expect(screen.getByText('Synced 1 offline scan(s).')).toBeDefined()
    await act(async () => { read.result.resolve(staleRows) })
    expect(screen.getByText('Synced 1 offline scan(s).')).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
  })

  it('a late failure is cleared when another tab has emptied the queue', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    await markStoredRowsSynced()
    first.fail(new TypeError('network down'))
    await readsSettled()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
  })

  it('a failure that finds the queue empty clears a standing failure note', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    // A 502 shows a failure note for X.
    stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(unreadableNote)).toBeDefined()
    // Attempt B (the online edge) sends X and hangs. Another tab marks X synced meanwhile.
    const held = holdReconciles()
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(1))
    await markStoredRowsSynced()
    // B fails after the queue is empty. The standing note says scans are saved, so it must go.
    held[0].fail(new TypeError('network down'))
    await readsSettled()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
  })


  it('a failure whose queue read throws reports an unknown count and leaves the storage alert alone', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const before = queueReads.started
    queueReads.failNext = true
    await act(async () => { first.fail(new TypeError('network down')) })
    expect(queueReads.failNext).toBe(false)
    expect(queueReads.started).toBe(before + 1)
    expect(queueReads.finished).toBe(queueReads.started)
    expect(screen.getByText(unknownOfflineNote)).toBeDefined()
    expect(screen.queryByText(/\d+ scans? (is|are) saved/)).toBeNull()
    expect(screen.queryByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')).toBeNull()
  })


  it('renders in the paired view', async () => {
    await failSyncWith(() => Promise.reject(new TypeError('network down')), offlineNote)
    const button = screen.getByRole('button', { name: 'Sync queued scans' })
    expect(within(button.closest('main')!).getByText(offlineNote)).toBeDefined()
  })


  it('a 401 after a failure carries that failure under the pairing instruction', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    // A 502 shows the failure while the device is paired.
    stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(unreadableNote)).toBeDefined()
    // A 401 unpairs the device. The instruction shows, and the failure stands under it, unshown.
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(/This device is not paired. Enter its pairing token to sync the queued scans./)).toBeDefined()
    expect(screen.getByText(/Sync failed:/)).toBeDefined()
    // Pairing keeps the failure with an unknown count until its own refresh reads the queue.
    const read = holdNextQueueRead()
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await bounded(read.started, 'pairing refresh')
    expect(screen.queryByText(/This device is not paired/)).toBeNull()
    expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
    await act(async () => { read.result.resolve(await storedRows() as OccurrenceRecord[]) })
    await readsSettled()
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })
})

// TKT-512: a shared queue count and a carried failure stay current across tabs, focus and pairing,
// and the sync line shows initial storage waits. Each test names the mechanism it exercises.
describe('shared queue refresh, carried failures and sync lifecycle (TKT-512)', () => {
  const focus = () => act(() => { window.dispatchEvent(new Event('focus')) })
  const tokenEvent = (newValue: string | null) => act(() => {
    window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue }))
  })
  const setVisibility = (state: 'hidden' | 'visible') => act(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state })
    document.dispatchEvent(new Event('visibilitychange'))
  })
  const restoreVisibility = () => { delete (document as { visibilityState?: unknown }).visibilityState }
  const sharedPage = async () => {
    const other = createTestPage({ dbName })
    await other.ready()
    return other
  }
  const reconcileCalls = (fetchMock: ReturnType<typeof stubFetch>) =>
    fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/reconciliations')).length

  // One row is queued and its sync has failed with a 502. The failure carries a count of one.
  async function failedSyncWithOneRow(): Promise<OccurrenceRecord> {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const [row] = await storedRows() as OccurrenceRecord[]
    stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(unreadableNote)).toBeDefined()
    return row
  }

  it('refreshes the count and corrects the carried failure on focus, without starting a sync', async () => {
    const row = await failedSyncWithOneRow()
    await (await sharedPage()).store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
    const reads = queueReads.started
    focus()
    await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    expect(reconcileCalls(fetchMock)).toBe(0)
  })

  it('refreshes on a visible transition and not on a hidden one', async () => {
    const row = await failedSyncWithOneRow()
    await (await sharedPage()).store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])
    stubFetch(() => Promise.reject(new TypeError('must not be called')))
    try {
      const reads = queueReads.started
      await setVisibility('hidden')
      await settle()
      expect(queueReads.started).toBe(reads)
      expect(screen.getByText(unreadableNote)).toBeDefined()
      await setVisibility('visible')
      await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    } finally { restoreVisibility() }
  })

  it('ignores a storage event that repeats the token this page already uses', async () => {
    await failedSyncWithOneRow()
    const reads = queueReads.started
    await tokenEvent('paired-device-token')
    await readsSettled()
    expect(queueReads.started).toBe(reads)
  })

  it('refreshes on cross-tab token removal without starting a sync', async () => {
    const row = await failedSyncWithOneRow()
    await (await sharedPage()).store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
    const reads = queueReads.started
    await tokenEvent(null)
    await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.queryByText(/offline scan.* still saved on this device/)).toBeNull()
    expect(reconcileCalls(fetchMock)).toBe(0)
  })

  it('drops the pairing instruction and carries the failure as unknown while a cross-tab pairing refreshes', async () => {
    const row = await failedSyncWithOneRow()
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(/This device is not paired\. Enter its pairing token to sync the queued scans\./)).toBeDefined()
    const read = holdNextQueueRead()
    await tokenEvent('new-device-token')
    await bounded(read.started, 'cross-tab pairing refresh')
    expect(screen.queryByText(/This device is not paired/)).toBeNull()
    expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
    await act(async () => { read.result.resolve([row]) })
    await readsSettled()
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })

  it('does not let an older queue read that resolves nonempty restore the count after a newer read published empty', async () => {
    const row = await failedSyncWithOneRow()
    const older = holdNextQueueRead()
    focus()
    await bounded(older.started, 'older focus read')
    await (await sharedPage()).store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])
    focus()
    await waitFor(() => expect(screen.queryByText(/^Sync failed:/)).toBeNull(), { timeout: BOUND_MS })
    await act(async () => { older.result.resolve([row]) })
    await readsSettled()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    expect(screen.queryByText(readAlert)).toBeNull()
  })

  it('reports an incompatible batch without storage-outage text and refreshes the count from the store', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('shared-row')
    await readsSettled()
    const [row] = await storedRows() as OccurrenceRecord[]
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const request = await held.next()
    await (await sharedPage()).store().applyReconcileResults([row.occurrenceId], [{ occurrence_id: row.occurrenceId, result: 'recorded' }])
    await act(async () => { request.answer(Response.json({ results: [{ occurrence_id: row.occurrenceId, result: 'synced' }] })) })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull(), { timeout: BOUND_MS })
    await readsSettled()
    expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
    expect(screen.queryByText(readAlert)).toBeNull()
    expect(screen.queryByText(/^Synced /)).toBeNull()
    expect(screen.queryByText(refusedDiagnostic)).toBeNull()
    expect(screen.queryByText(writeDiagnostic)).toBeNull()
    await focus()
    await readsSettled()
    expect(screen.queryByText(refusedDiagnostic)).toBeNull()
  })

  it('treats an invalid store result as an unreadable sync and leaves the storage alert alone', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const before = await storedRows()
    vi.spyOn(page.store(), 'applyReconcileResults').mockResolvedValueOnce({ status: 'invalid', why: 'a result is outside the contract' })
    stubFetch((body) => Promise.resolve(recordedResponse(body)))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(unreadableNote)).toBeDefined()
    expect(screen.queryByText(readAlert)).toBeNull()
    expect(screen.queryByText(writeDiagnostic)).toBeNull()
    expect(await storedRows()).toStrictEqual(before)
  })

  it('shows a pairing-form error for a failed fingerprint, without storage-unavailable text or Retry storage', async () => {
    localStorage.removeItem('scanner.device-token')
    render(<App />)
    vi.spyOn(crypto.subtle, 'digest').mockRejectedValueOnce(new Error('digest unavailable'))
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
    expect(await screen.findByText(pairingError)).toBeDefined()
    expect(screen.queryByText(readAlert)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Retry storage' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
    expect(await screen.findByRole('button', { name: 'Check ticket' })).toBeDefined()
    expect(screen.queryByText(pairingError)).toBeNull()
  })

  it('shows waiting at 1,000 ms of initial storage work and clears it before the first request is sent', async () => {
    await failedSyncWithOneRow()
    const rows = await storedRows() as OccurrenceRecord[]
    const read = holdNextQueueRead()
    const fetchMock = stubFetch(() => new Promise<Response>(() => {}))
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(read.started, 'held initial read')
      await act(async () => { await vi.advanceTimersByTimeAsync(999) })
      expect(screen.queryByText(waitingNote)).toBeNull()
      await act(async () => { await vi.advanceTimersByTimeAsync(1) })
      expect(screen.getByText(waitingNote)).toBeDefined()
      // Releasing the read ends the initial storage phase. Waiting clears before the request is made.
      await act(async () => { read.result.resolve(rows) })
      expect(screen.queryByText(waitingNote)).toBeNull()
      await act(async () => { await vi.advanceTimersByTimeAsync(0) })
      expect(reconcileCalls(fetchMock)).toBe(1)
    } finally {
      read.result.resolve(rows)
      vi.useRealTimers()
    }
  })

  it('waiting does not return after a fast initial read, even when the timer passes while the request is held', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const [row] = await storedRows() as OccurrenceRecord[]
    const answer = deferred<Response>()
    const sent = deferred<{ body: ReconcileBody; signal: AbortSignal }>()
    const fetchMock = stubFetch((body, init) => {
      sent.resolve({ body, signal: init.signal! })
      return answer.promise
    })
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      // The initial storage work finishes in real time, before any virtual time passes.
      const request = await sent.promise
      await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
      expect(screen.queryByText(waitingNote)).toBeNull()
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000) })
      expect(screen.queryByText(waitingNote)).toBeNull()
      // 3,000 ms of virtual time stays under the reconcile deadline, so the held request is still open.
      expect(3_000).toBeLessThan(reconcileTimeoutMs)
      expect(request.signal.aborted).toBe(false)
      expect(reconcileCalls(fetchMock)).toBe(1)
    } finally {
      vi.useRealTimers()
      // Release the held response even when an assertion above failed.
      void sent.promise.then(({ body }) => answer.resolve(recordedResponse(body)))
    }
    await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
    expect(screen.queryByText(waitingNote)).toBeNull()
  })

  it('aborts the owned reconcile request on cleanup and does not apply its late response', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const before = await storedRows()
    const sent = deferred<{ body: ReconcileBody; signal: AbortSignal; answer: (response: Response) => void }>()
    stubFetch((body, init) => new Promise<Response>((answer) => sent.resolve({ body, signal: init.signal!, answer })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const request = await sent.promise
    cleanup()
    expect(request.signal.aborted).toBe(true)
    request.answer(recordedResponse(request.body))
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(await storedRows()).toStrictEqual(before)
  })

  it('keeps waiting through a token removal and clears it when the initial read settles without a token', async () => {
    await failedSyncWithOneRow()
    const rows = await storedRows() as OccurrenceRecord[]
    const read = holdNextQueueRead()
    const fetchMock = stubFetch(() => new Promise<Response>(() => {}))
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(read.started, 'held initial read')
      await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
      expect(screen.getByText(waitingNote)).toBeDefined()
      // A token removal does not end the running prelude or its waiting line.
      tokenEvent(null)
      expect(screen.getByText(waitingNote)).toBeDefined()
      await act(async () => { read.result.resolve(rows) })
      expect(screen.queryByText(waitingNote)).toBeNull()
      await act(async () => { await vi.advanceTimersByTimeAsync(0) })
      expect(reconcileCalls(fetchMock)).toBe(0)
    } finally {
      read.result.resolve(rows)
      vi.useRealTimers()
    }
  })

  it('applies a sent old-pairing result once, reports nothing from it, and does not resend it after a pairing change', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('sent-old')
    await readsSettled()
    const [row] = await storedRows() as OccurrenceRecord[]
    const sent = deferred<{ body: ReconcileBody; signal: AbortSignal; answer: (response: Response) => void }>()
    const fetchMock = stubFetch((body, init) => new Promise<Response>((answer) => sent.resolve({ body, signal: init.signal!, answer })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const request = await sent.promise
    // The replacement token and a requested follow-up arrive while the old batch is in flight.
    tokenEvent('replacement-token')
    window.dispatchEvent(new Event('online'))
    await readsSettled()
    expect(request.signal.aborted).toBe(false)
    expect(reconcileCalls(fetchMock)).toBe(1)
    await act(async () => { request.answer(Response.json({ results: [{ occurrence_id: row.occurrenceId, result: 'conflict' }] })) })
    await readsSettled()
    expect(await storedRows()).toStrictEqual([terminalRow(row, 'conflict')])
    expect(screen.queryByText(/^Synced /)).toBeNull()
    expect(screen.queryByText(/^Completed batches:/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    expect(reconcileCalls(fetchMock)).toBe(1)
  })

  it('does not let an empty read that captured no failure remove a failure created after it started', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const rows = await storedRows() as OccurrenceRecord[]
    // The sync's own prelude reads and publishes first. Its fetch then waits for the test.
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    // An ordinary refresh is invoked before the failure exists, so it captures no failure. It resolves empty after the failure.
    const older = holdNextQueueRead()
    focus()
    await bounded(older.started, 'older refresh')
    // The failure's direct correction read is held, so the older empty result can arrive before it.
    const correction = holdNextQueueRead()
    await act(async () => { first.answer(new Response('', { status: 502 })) })
    await bounded(correction.started, 'failure correction read')
    await act(async () => { older.result.resolve([]) })
    // The older read captured no failure, so it leaves the failure checking and removes only the count.
    expect(screen.getByText('Sync failed: The server answered but the reply could not be read. Checking the scans saved on this device…')).toBeDefined()
    expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    await act(async () => { correction.result.resolve(rows) })
    await readsSettled()
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })

  it('refreshes the device queue after a sent old-pairing batch applies, with no requested follow-up', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('applied-old')
    await readsSettled()
    const [row] = await storedRows() as OccurrenceRecord[]
    const sent = deferred<{ body: ReconcileBody; answer: (response: Response) => void }>()
    const fetchMock = stubFetch((body) => new Promise<Response>((answer) => sent.resolve({ body, answer })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const request = await sent.promise
    // Another tab removes the pairing. Its refresh reads the row still queued. No follow-up is requested.
    const reads = queueReads.started
    tokenEvent(null)
    await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
    await readsSettled()
    expect(screen.getByText(/offline scan.* still saved on this device/)).toBeDefined()
    await act(async () => { request.answer(recordedResponse(request.body)) })
    await readsSettled()
    expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
    expect(screen.queryByText(/offline scan.* still saved on this device/)).toBeNull()
    expect(screen.queryByText(/^Synced /)).toBeNull()
    expect(reconcileCalls(fetchMock)).toBe(1)
  })

  // Helpers for the completion schedules. A gate holds the next calls to one production method. Each hold
  // runs the real method on release, or throws the error given to release. Holds are never left open.
  type Callable = (...args: unknown[]) => Promise<unknown>
  function gateMethod(target: object, method: string) {
    const owner = target as unknown as Record<string, Callable>
    // A method this test's fixture already spied is restored first. Wrapping the spy would call the gate from itself.
    ;(owner[method] as unknown as { mockRestore?: () => void }).mockRestore?.()
    const real = owner[method].bind(target)
    const plans: Array<{ started: () => void; released: Promise<Error | undefined> }> = []
    vi.spyOn(owner, method).mockImplementation(async (...args: unknown[]) => {
      const plan = plans.shift()
      if (!plan) return real(...args)
      plan.started()
      const error = await plan.released
      if (error) throw error
      return real(...args)
    })
    return {
      hold() {
        const started = deferred<void>()
        const released = deferred<Error | undefined>()
        plans.push({ started: () => started.resolve(), released: released.promise })
        return { started: started.promise, release: (error?: Error) => released.resolve(error) }
      },
    }
  }
  const focusWindow = () => act(() => { window.dispatchEvent(new Event('focus')) })
  // A cross-tab pairing commit, simulated in this one jsdom window: it writes the token and dispatches a synthetic StorageEvent.
  // It is not evidence of another browser page. The browser journeys B1 and B3 supply that proof.
  const commitPairing = (token: string) => {
    localStorage.setItem('scanner.device-token', token)
    tokenEvent(token)
  }
  const sentTokens = (fetchMock: ReturnType<typeof stubFetch>) => fetchMock.mock.calls
    .filter(([url]) => String(url).endsWith('/reconciliations'))
    .map(([, init]) => ((init as RequestInit).headers as Record<string, string>)['X-Scanner-Token'])
  const sentIds = (fetchMock: ReturnType<typeof stubFetch>) => fetchMock.mock.calls
    .filter(([url]) => String(url).endsWith('/reconciliations'))
    .flatMap(([, init]) => (JSON.parse((init as RequestInit).body as string) as ReconcileBody).occurrences.map((row) => row.occurrence_id))
  const pairLocally = (token: string) => {
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: token } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
  }
  // A second page fixture on the same database, with its own owner, retires these rows as another tab's sync would.
  // In this jsdom test it is a second store connection, not a second browser page.
  async function retireElsewhere(rows: OccurrenceRecord[]) {
    const other = await sharedPage()
    await other.store().applyReconcileResults(rows.map((row) => row.occurrenceId), rows.map((row) => ({ occurrence_id: row.occurrenceId, result: 'recorded' })))
  }
  // One row is queued and its sync failed with a 401 that unpairs the device. The failure is carried under the instruction.
  async function unpairedWithOneRow(): Promise<OccurrenceRecord> {
    const row = await failedSyncWithOneRow()
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(/This device is not paired\. Enter its pairing token to sync the queued scans\./)).toBeDefined()
    return row
  }
  const byId = (a: OccurrenceRecord, b: OccurrenceRecord) => (a.occurrenceId < b.occurrenceId ? -1 : 1)

  describe('T1 refresh triggers, pairing notes and preservation', () => {
    it('a focus read after another tab retires the row removes the failure and keeps the scan result', async () => {
      const row = await failedSyncWithOneRow()
      await retireElsewhere([row])
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      const reads = queueReads.started
      focusWindow()
      await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
      await readsSettled()
      expect(screen.getByRole('heading', { name: 'Offline decision' })).toBeDefined()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
      expect(sentIds(fetchMock)).toStrictEqual([])
      expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
    })

    it('a focus read that still finds one of two rows corrects the failure to that exact count', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('first-ticket')
      await queueOneScan('second-ticket')
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      expect(rows).toHaveLength(2)
      stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
      fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(/^Sync failed: The server answered but the reply could not be read\. 2 scans are saved/)).toBeDefined()
      const [retired] = rows.slice().sort(byId)
      await retireElsewhere([retired])
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      focusWindow()
      await readsSettled()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
      expect(screen.getByText(unreadableNote)).toBeDefined()
      expect(sentIds(fetchMock)).toStrictEqual([])
      const after = await storedRows() as OccurrenceRecord[]
      expect(after.sort(byId)).toStrictEqual([terminalRow(retired, 'recorded'), rows.find((row) => row.occurrenceId !== retired.occurrenceId)!].sort(byId))
    })

    it('a local 401 adds no queue refresh, and the carried count stays as it was', async () => {
      await failedSyncWithOneRow()
      await readsSettled()
      stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(/This device is not paired\./)).toBeDefined()
      // The sync's own preparation read ran before the 401. Only reads after the unpairing count here.
      await readsSettled()
      const reads = queueReads.started
      await settle()
      expect(queueReads.started).toBe(reads)
      expect(screen.getByText(/Sync failed: The server answered but the reply could not be read\. 1 scan is saved/)).toBeDefined()
    })

    it('a cross-tab nonempty pairing drops the instruction, holds the failure as unknown, and corrects it from the held read', async () => {
      const row = await unpairedWithOneRow()
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      const read = holdNextQueueRead()
      commitPairing('new-device-token')
      await bounded(read.started, 'cross-tab pairing refresh')
      expect(screen.queryByText(/This device is not paired/)).toBeNull()
      expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
      expect(screen.queryByRole('heading', { name: 'Offline decision' })).toBeNull()
      await act(async () => { read.result.resolve([row]) })
      await readsSettled()
      expect(screen.getByText(unreadableNote)).toBeDefined()
      expect(sentIds(fetchMock)).toStrictEqual([])
      expect(fetchMock.mock.calls.some(([url]) => String(url).includes('voided-tickets'))).toBe(true)
    })

    it('a cross-tab pairing whose refresh reads empty removes the captured failure', async () => {
      const row = await unpairedWithOneRow()
      const read = holdNextQueueRead()
      commitPairing('new-device-token')
      await bounded(read.started, 'cross-tab pairing refresh')
      expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
      await retireElsewhere([row])
      await act(async () => { read.result.resolve([]) })
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    })

    it('a cross-tab pairing whose refresh fails keeps the count unknown, the pairing, and the read alert', async () => {
      await unpairedWithOneRow()
      const read = holdNextQueueRead()
      commitPairing('new-device-token')
      await bounded(read.started, 'cross-tab pairing refresh')
      await act(async () => { read.result.reject(new Error('queue read failed')) })
      await readsSettled()
      expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(screen.queryByText(/This device is not paired/)).toBeNull()
      expect(screen.getByRole('button', { name: 'Check ticket' })).toBeDefined()
    })

    it('a local pairing whose refresh reads empty removes the captured failure', async () => {
      const row = await unpairedWithOneRow()
      const read = holdNextQueueRead()
      pairLocally('new-device-token')
      await bounded(read.started, 'local pairing refresh')
      expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
      await retireElsewhere([row])
      await act(async () => { read.result.resolve([]) })
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    })

    it('a local pairing whose refresh fails keeps the count unknown and raises the read alert', async () => {
      await unpairedWithOneRow()
      const read = holdNextQueueRead()
      pairLocally('new-device-token')
      await bounded(read.started, 'local pairing refresh')
      await act(async () => { read.result.reject(new Error('queue read failed')) })
      await readsSettled()
      expect(screen.getByText(unknownUnreadableNote)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
    })

    it('a failure removed while unpaired is not restored by a later local pairing', async () => {
      const row = await unpairedWithOneRow()
      await retireElsewhere([row])
      focusWindow()
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      pairLocally('new-device-token')
      expect(await screen.findByRole('button', { name: 'Check ticket' })).toBeDefined()
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    })

    it('a cross-tab removal refresh corrects a carried failure from a nonempty read', async () => {
      await failedSyncWithOneRow()
      const read = holdNextQueueRead()
      tokenEvent(null)
      await bounded(read.started, 'cross-tab removal refresh')
      await act(async () => { read.result.resolve(await storedRows() as OccurrenceRecord[]) })
      await readsSettled()
      expect(screen.getByText(unreadableNote)).toBeDefined()
      expect(screen.getByText(/offline scan.* still saved on this device/)).toBeDefined()
    })

    it('a focus refresh keeps applied-batch totals, per-row warnings and the held second batch', async () => {
      const { fixture, held, second } = await startMixedSync()
      await expectPartial(fixture)
      focusWindow()
      await readsSettled()
      await expectPartial(fixture)
      expect(screen.getByText(/^5 queued offline scans awaiting sync\./)).toBeDefined()
      expect(held).toHaveLength(2)
      await act(async () => { second.answer(mixedResponse(second.body, fixture)) })
      await readsSettled()
    }, 15_000)
  })

  describe('T2 observation order and captured failures', () => {
    it('an older read that resolves first publishes, then a newer empty read clears the count and its captured failure', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('older-a')
      await queueOneScan('older-b')
      await readsSettled()
      const [rowA, rowB] = await storedRows() as OccurrenceRecord[]
      stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(unreadableTwoNote)).toBeDefined()
      await readsSettled()
      // Another tab retires B. The older read starts after that and captures only A.
      await retireElsewhere([rowB])
      const remaining = (await storedRows() as OccurrenceRecord[]).filter((row) => row.state === 'QUEUED')
      expect(remaining).toStrictEqual([rowA])
      const older = holdNextQueueRead()
      focusWindow()
      await bounded(older.started, 'older focus read')
      // Another tab retires A before the newer read starts, so the newer read finds nothing.
      await retireElsewhere([rowA])
      const newer = holdNextQueueRead()
      focusWindow()
      await bounded(newer.started, 'newer focus read')
      // The older read publishes while the newer read is still held. Its count and failure must change now.
      await act(async () => { older.result.resolve(remaining) })
      await waitFor(() => expect(screen.getByText(unreadableNote)).toBeDefined(), { timeout: BOUND_MS })
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
      await act(async () => { newer.result.resolve([]) })
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
      expect(await storedRows()).toStrictEqual([terminalRow(rowA, 'recorded'), terminalRow(rowB, 'recorded')])
    })

    it('a newer empty read that publishes first is not restored by an older read that later rejects', async () => {
      await failedSyncWithOneRow()
      const older = holdNextQueueRead()
      focusWindow()
      await bounded(older.started, 'older focus read')
      const newer = holdNextQueueRead()
      focusWindow()
      await bounded(newer.started, 'newer focus read')
      await act(async () => { newer.result.resolve([]) })
      await waitFor(() => expect(screen.queryByText(/^Sync failed:/)).toBeNull(), { timeout: BOUND_MS })
      await act(async () => { older.result.reject(new Error('older queue read failed')) })
      await readsSettled()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    })

    it('a prelude held while a newer observation completes publishes from its later queue call', async () => {
      const row = await failedSyncWithOneRow()
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      focusWindow()
      await bounded(prelude.started, 'older prelude')
      focusWindow()
      await readsSettled()
      expect(screen.getByText(unreadableNote)).toBeDefined()
      await retireElsewhere([row])
      prelude.release()
      await readsSettled()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    })

    it('an older failure correction that resolves after a newer rejection cannot overwrite its unknown count', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      const correction = holdNextQueueRead()
      await act(async () => { first.fail(new TypeError('network down')) })
      await bounded(correction.started, 'failure correction read')
      expect(screen.getByText(checkingOfflineNote)).toBeDefined()
      const newer = holdNextQueueRead()
      focusWindow()
      await bounded(newer.started, 'newer observation')
      await act(async () => { newer.result.reject(new Error('queue read failed')) })
      await settle()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
      await act(async () => { correction.result.resolve(await storedRows() as OccurrenceRecord[]) })
      await readsSettled()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    })

    it('an older correction held across a newer successful read and a later failed sync removes nothing', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      const correction = holdNextQueueRead()
      await act(async () => { first.fail(new TypeError('network down')) })
      await bounded(correction.started, 'first correction read')
      focusWindow()
      await waitFor(() => expect(screen.getByText(offlineNote)).toBeDefined(), { timeout: BOUND_MS })
      stubFetch(() => Promise.reject(new TypeError('network down again')))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(screen.getByText(offlineNote)).toBeDefined(), { timeout: BOUND_MS })
      await act(async () => { correction.result.resolve([]) })
      await readsSettled()
      expect(screen.getByText(offlineNote)).toBeDefined()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    })

    it('an ordinary read that rejects after a failure appeared captured no failure and cannot rewrite it', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      const older = holdNextQueueRead()
      focusWindow()
      await bounded(older.started, 'older ordinary read')
      const correction = holdNextQueueRead()
      await act(async () => { first.fail(new TypeError('network down')) })
      await bounded(correction.started, 'failure correction read')
      expect(screen.getByText(checkingOfflineNote)).toBeDefined()
      await act(async () => { older.result.reject(new Error('older read failed')) })
      await settle()
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(screen.getByText(checkingOfflineNote)).toBeDefined()
      await act(async () => { correction.result.resolve(await storedRows() as OccurrenceRecord[]) })
      await readsSettled()
      expect(screen.getByText(offlineNote)).toBeDefined()
    })
  })

  describe('T3 error sources and application outcomes', () => {
    it('an ordinary refresh whose queued() rejects keeps the count and raises the read source', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      queueReads.failNext = true
      focusWindow()
      await readsSettled()
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    })

    it('a rejected repair in the refresh prelude raises the read source and keeps the count', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const repairs = vi.spyOn(page.coordinator, 'repairAll').mockRejectedValueOnce(new Error('repair unavailable'))
      focusWindow()
      await readsSettled()
      expect(repairs).toHaveBeenCalledTimes(1)
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    })

    it('a prelude that rejects after a newer successful read raises no read source', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const older = gateMethod(page.coordinator, 'repairAll').hold()
      focusWindow()
      await bounded(older.started, 'older prelude')
      focusWindow()
      await readsSettled()
      older.release(new Error('older prelude failed'))
      await readsSettled()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
    })

    it('a sync read that rejects after waiting is visible clears the wait, raises the read source, and sends nothing', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const read = holdNextQueueRead()
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(read.started, 'sync preparation read')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
        await act(async () => { read.result.reject(new Error('queue read failed')) })
        expect(screen.queryByText(waitingNote)).toBeNull()
        expect(screen.getByText(readAlert)).toBeDefined()
        await act(async () => { await vi.advanceTimersByTimeAsync(0) })
        expect(sentIds(fetchMock)).toStrictEqual([])
      } finally { vi.useRealTimers() }
    })

    it('a sync read that rejects after a pairing change still raises the read source and sends nothing', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const read = holdNextQueueRead()
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(read.started, 'sync preparation read')
      // The pairing refresh is held too, so it cannot publish a newer result before the sync read rejects.
      const refresh = holdNextQueueRead()
      commitPairing('replacement-token')
      await bounded(refresh.started, 'pairing refresh')
      await act(async () => { read.result.reject(new Error('queue read failed')) })
      await settle()
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(sentIds(fetchMock)).toStrictEqual([])
      await act(async () => { refresh.result.resolve([]) })
      await readsSettled()
      expect(sentIds(fetchMock)).toStrictEqual([])
    })

    it('a rejected failure correction sets the unknown count and neither raises nor clears the read source', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      queueReads.failNext = true
      await act(async () => { first.fail(new TypeError('network down')) })
      await readsSettled()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.queryByText(readAlert)).toBeNull()
    })

    it('a rejected failure correction leaves an existing read alert in place', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      queueReads.failNext = true
      focusWindow()
      await readsSettled()
      expect(screen.getByText(readAlert)).toBeDefined()
      queueReads.failNext = true
      await act(async () => { first.fail(new TypeError('network down')) })
      await readsSettled()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
    })

    it('a readiness notification alone does not clear the read source', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      queueReads.failNext = true
      focusWindow()
      await readsSettled()
      expect(screen.getByText(readAlert)).toBeDefined()
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      page.storage.stop()
      page.storage.retry()
      await bounded(prelude.started, 'refresh after readiness returns')
      expect(screen.getByText(readAlert)).toBeDefined()
      prelude.release(new Error('prelude failed'))
      await readsSettled()
      expect(screen.getByText(readAlert)).toBeDefined()
    })

    it('an accepted ordinary read clears the read alert while a coordinator repair warning remains', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const instrument: Instrument = instrumentTransactions()
      try {
        const { scan } = await mintOn(page)
        page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-20T12:00:01.000Z' })
        for (let attempt = 0; attempt < 6; attempt += 1) instrument.inject('abort', { stores: ['occurrences'], mode: 'readwrite', durability: 'strict' })
        expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
        focusWindow()
        await readsSettled()
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
        queueReads.failNext = true
        focusWindow()
        await readsSettled()
        expect(screen.getByText(readAlert)).toBeDefined()
        focusWindow()
        await readsSettled()
        expect(screen.queryByText(readAlert)).toBeNull()
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
      } finally { instrument.restore() }
    })

    it('a repair warning from a returned failed report coexists with a queued control, and the sync sends only the control', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const [control] = await storedRows() as OccurrenceRecord[]
      const instrument: Instrument = instrumentTransactions()
      try {
        const { scan } = await mintOn(page)
        page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-20T12:00:01.000Z' })
        for (let attempt = 0; attempt < 6; attempt += 1) instrument.inject('abort', { stores: ['occurrences'], mode: 'readwrite', durability: 'strict' })
        expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
        const fetchMock = stubFetch(body => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
        expect(sentIds(fetchMock)).toStrictEqual([control.occurrenceId])
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
        expect(screen.queryByText(readAlert)).toBeNull()
      } finally { instrument.restore() }
    })

    it('a pairing-metadata error followed by a committed cross-tab pairing clears the form error, and an obsolete rejection cannot restore it', async () => {
      localStorage.removeItem('scanner.device-token')
      render(<App />)
      const digest = deferred<ArrayBuffer>()
      vi.spyOn(crypto.subtle, 'digest').mockImplementationOnce(() => digest.promise)
      pairLocally('new-device-token')
      commitPairing('cross-tab-token')
      await act(async () => { digest.reject(new Error('digest unavailable')) })
      await settle()
      // The paired view has no pairing form, so the late local rejection has nowhere to show.
      expect(screen.queryByLabelText(/pairing token/i)).toBeNull()
      expect(screen.queryByText(pairingError)).toBeNull()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Retry storage' })).toBeNull()
      expect(await screen.findByRole('button', { name: 'Check ticket' })).toBeDefined()
      // Removing the token shows the pairing form again, and the obsolete rejection must not appear there.
      // This removal is one synthetic StorageEvent in this window. It is not a second browser tab.
      localStorage.removeItem('scanner.device-token')
      tokenEvent(null)
      expect(await screen.findByLabelText(/pairing token/i)).toBeDefined()
      expect(screen.queryByText(pairingError)).toBeNull()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Retry storage' })).toBeNull()
    })

    it('an incompatible batch with a queued sibling applies none of its rows and leaves the sibling queued', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('shared-row')
      await queueOneScan('sibling-row')
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      const shared = rows.find((row) => row.qrPayload === 'shared-row')!
      const sibling = rows.find((row) => row.qrPayload === 'sibling-row')!
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const request = await held.next()
      await retireElsewhere([shared])
      await act(async () => { request.answer(Response.json({ results: [
        { occurrence_id: shared.occurrenceId, result: 'synced' }, { occurrence_id: sibling.occurrenceId, result: 'recorded' },
      ] })) })
      await waitFor(() => expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined(), { timeout: BOUND_MS })
      await readsSettled()
      const after = await storedRows() as OccurrenceRecord[]
      expect(after).toHaveLength(2)
      expect(after).toEqual(expect.arrayContaining([terminalRow(shared, 'recorded'), sibling]))
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByText(writeDiagnostic)).toBeNull()
      expect(screen.queryByText(/^Synced /)).toBeNull()
    })

    it('a write diagnostic survives reads, empty syncs, a failed prelude, a pairing change and an unrelated write', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      queueReads.failMarkSyncedNext = true
      stubFetch((body) => Promise.resolve(recordedResponse(body)))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(writeDiagnostic)).toBeDefined()
      // The diagnostic alone shows no Retry storage. The page is ready, and no read or repair has failed.
      expect(screen.queryByRole('button', { name: 'Retry storage' })).toBeNull()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByText(REPAIR_NEEDED_TEXT)).toBeNull()
      focusWindow()
      await readsSettled()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
      expect(screen.queryByRole('button', { name: 'Retry storage' })).toBeNull()
      const empty = holdNextQueueRead()
      const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(empty.started, 'empty sync read')
      await act(async () => { empty.result.resolve([]) })
      await readsSettled()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
      expect(sentIds(fetchMock)).toStrictEqual([])
      // The empty read hid the sync control. A new offline scan brings it back for the failed-prelude sync.
      await queueOneScan('second-ticket')
      const prelude = vi.spyOn(page.coordinator, 'repairAll').mockRejectedValueOnce(new Error('repair unavailable'))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await readsSettled()
      expect(prelude).toHaveBeenCalledTimes(1)
      // The failed prelude is a real read error. Its source, not the diagnostic, is what shows Retry storage.
      expect(screen.getByText(readAlert)).toBeDefined()
      expect(screen.getByRole('button', { name: 'Retry storage' })).toBeDefined()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
      commitPairing('replacement-token')
      await readsSettled()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
      const other = await sharedPage()
      await mintOn(other)
      await readsSettled()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
      expect(sentIds(fetchMock)).toStrictEqual([])
    })

    it('a new nonempty sync replaces the write diagnostic at its first request, and its own failure then shows', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      queueReads.failMarkSyncedNext = true
      stubFetch((body) => Promise.resolve(recordedResponse(body)))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(writeDiagnostic)).toBeDefined()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const request = await held.next()
      // The replacement is a state update made before the request is answered. Wait for React to paint it.
      await waitFor(() => expect(screen.queryByText(writeDiagnostic)).toBeNull(), { timeout: BOUND_MS })
      await act(async () => { request.fail(new TypeError('network down')) })
      expect(await screen.findByText(offlineNote)).toBeDefined()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const retry = await held.next()
      await act(async () => { retry.answer(recordedResponse(retry.body)) })
      expect(await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })).toBeDefined()
    })
  })

  describe('T3 write diagnostic beside coordinator repair warnings and readiness', () => {
    it('a readiness notification leaves the write diagnostic in place until the next sync request', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      queueReads.failMarkSyncedNext = true
      stubFetch((body) => Promise.resolve(recordedResponse(body)))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(await screen.findByText(writeDiagnostic)).toBeDefined()
      page.storage.stop()
      page.storage.retry()
      await page.waitFor('ready')
      await readsSettled()
      expect(screen.getByText(writeDiagnostic)).toBeDefined()
    })

    it('a genuine repair warning stays while the next sync request replaces the write diagnostic', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const instrument: Instrument = instrumentTransactions()
      try {
        const { scan } = await mintOn(page)
        page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-20T12:00:01.000Z' })
        for (let attempt = 0; attempt < 8; attempt += 1) instrument.inject('abort', { stores: ['occurrences'], mode: 'readwrite', durability: 'strict' })
        expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
        queueReads.failMarkSyncedNext = true
        stubFetch((body) => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        expect(await screen.findByText(writeDiagnostic)).toBeDefined()
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
        // The coordinator warning is an independent source, so Retry storage shows for it.
        expect(screen.getByRole('button', { name: 'Retry storage' })).toBeDefined()
        const held = holdReconciles()
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const request = await held.next()
        await waitFor(() => expect(screen.queryByText(writeDiagnostic)).toBeNull(), { timeout: BOUND_MS })
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
        expect(screen.getByRole('button', { name: 'Retry storage' })).toBeDefined()
        await act(async () => { request.fail(new TypeError('network down')) })
        await waitFor(() => expect(screen.getByText(/^Sync failed: No connection\./)).toBeDefined(), { timeout: BOUND_MS })
        expect(screen.getByText(REPAIR_NEEDED_TEXT)).toBeDefined()
      } finally { instrument.restore() }
    })
  })

  describe('T4 waiting and coalescing', () => {
    // One production await of the initial storage phase. The hold is released by finish or fail, and never left open.
    function holdPhase(phase: 'repair' | 'recovery' | 'queue') {
      if (phase === 'queue') {
        const read = holdNextQueueRead()
        return { started: read.started, finish: (rows: OccurrenceRecord[]) => read.result.resolve(rows), fail: (error: Error) => read.result.reject(error) }
      }
      const hold = gateMethod(phase === 'repair' ? page.coordinator : page.store(), phase === 'repair' ? 'repairAll' : 'recoverDeadOwners').hold()
      return { started: hold.started, finish: (_rows: OccurrenceRecord[]) => hold.release(), fail: (error: Error) => hold.release(error) }
    }
    type Sent = { waiting: boolean; body: ReconcileBody; answer: (response: Response) => void }
    // Records whether the waiting line was visible when a request was made. The React state update that clears
    // the line is committed first, so the record reads the committed DOM. Requests stay open until answered.
    function recordRequests(sent: Sent[]) {
      return stubFetch(async (body) => {
        await act(async () => {})
        return new Promise<Response>((answer) => {
          sent.push({ waiting: screen.queryByText(waitingNote) !== null, body, answer })
        })
      })
    }

    it.each(['repair', 'recovery', 'queue'] as const)('waiting follows a held %s at 1,000 ms and clears before the request it precedes', async (phase) => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      const sent: Sent[] = []
      recordRequests(sent)
      const hold = holdPhase(phase)
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(hold.started, `held ${phase}`)
        await act(async () => { await vi.advanceTimersByTimeAsync(999) })
        expect(screen.queryByText(waitingNote)).toBeNull()
        await act(async () => { await vi.advanceTimersByTimeAsync(1) })
        expect(screen.getByText(waitingNote)).toBeDefined()
        expect(sent).toHaveLength(0)
      } finally { vi.useRealTimers(); hold.finish(rows) }
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      expect(sent[0].waiting).toBe(false)
      expect(screen.queryByText(waitingNote)).toBeNull()
      sent[0].answer(recordedResponse(sent[0].body))
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    })

    it.each(['empty', 'rejected', 'no token'] as const)('waiting clears when the held queue read settles %s, and no request follows', async (outcome) => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      const sent: Sent[] = []
      recordRequests(sent)
      const read = holdPhase('queue')
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(read.started, 'held queue read')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
        // A token removal does not end the running read. Its settlement clears the wait.
        if (outcome === 'no token') tokenEvent(null)
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      if (outcome === 'rejected') read.fail(new Error('queue read failed'))
      else read.finish(outcome === 'empty' ? [] : rows)
      await waitFor(() => expect(screen.queryByText(waitingNote)).toBeNull(), { timeout: BOUND_MS })
      await readsSettled()
      expect(sent).toHaveLength(0)
      if (outcome === 'rejected') expect(screen.getByText(readAlert)).toBeDefined()
    })

    it('a token replacement during a held prelude keeps waiting visible and starts no second pass', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const sent: Sent[] = []
      recordRequests(sent)
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(prelude.started, 'held prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
        commitPairing('replacement-token')
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      prelude.release()
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      expect(sent[0].waiting).toBe(false)
      sent[0].answer(recordedResponse(sent[0].body))
      await readsSettled()
      expect(sentTokens(globalThis.fetch as unknown as ReturnType<typeof stubFetch>)).toStrictEqual(['replacement-token'])
      expect(sent).toHaveLength(1)
    })

    it('a token replacement before the threshold keeps the original timer, which shows waiting at 1,000 ms', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(prelude.started, 'held prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(500) })
        commitPairing('replacement-token')
        await act(async () => { await vi.advanceTimersByTimeAsync(499) })
        expect(screen.queryByText(waitingNote)).toBeNull()
        await act(async () => { await vi.advanceTimersByTimeAsync(1) })
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      prelude.release()
      await readsSettled()
      expect(screen.queryByText(waitingNote)).toBeNull()
    })

    it('repeated requests during an active pass produce one coalesced follow-up pass', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const sent: Sent[] = []
      recordRequests(sent)
      const passes = vi.spyOn(page.coordinator, 'repairAll')
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      expect(passes).toHaveBeenCalledTimes(1)
      sent[0].answer(recordedResponse(sent[0].body))
      await readsSettled()
      // Each pass prepares once and refreshes once after its batch. The two clicks add one follow-up pass,
      // which prepares again, finds nothing left to send, and starts no request: 1 + 1 + 1 calls.
      expect(passes).toHaveBeenCalledTimes(3)
      expect(sent).toHaveLength(1)
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    })

    // cleanup and render make a new component. React ignores state updates from an unmounted component, so the old
    // finalizer cannot change the new wait. This test proves only independent remount behavior. The new wait stays
    // visible, and obsolete work publishes nothing. The same-component case is in the next test.
    it('a remount during a held wait keeps the new lifecycle waiting until its own prelude settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const repairs = gateMethod(page.coordinator, 'repairAll')
      const oldPrelude = repairs.hold()
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(oldPrelude.started, 'old prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      cleanup()
      render(<App />)
      await readsSettled()
      const newPrelude = repairs.hold()
      const sent: Sent[] = []
      recordRequests(sent)
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(newPrelude.started, 'new prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      // The old prelude now settles. Its component is unmounted, so the new wait stays visible and no obsolete request starts.
      oldPrelude.release()
      await settle()
      expect(screen.getByText(waitingNote)).toBeDefined()
      expect(sent).toHaveLength(0)
      newPrelude.release()
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      expect(sent[0].waiting).toBe(false)
      sent[0].answer(recordedResponse(sent[0].body))
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    })

    // A hidden Activity runs the old effect cleanup. Showing it runs a new setup on the same component, so both
    // lifecycles share one set of state setters. The old finalizer must not clear the new lifecycle's wait.
    // main.tsx does not render App inside an Activity, so the scanner does not take this path in production.
    it('a remount during a held wait keeps the new wait when an Activity re-runs effects on the same component', async () => {
      const view = render(<Activity mode="visible"><App /></Activity>)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const sent: Sent[] = []
      recordRequests(sent)
      const repairs = gateMethod(page.coordinator, 'repairAll')
      const oldPrelude = repairs.hold()
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(oldPrelude.started, 'old prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      view.rerender(<Activity mode="hidden"><App /></Activity>)
      view.rerender(<Activity mode="visible"><App /></Activity>)
      await readsSettled()
      const newPrelude = repairs.hold()
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      try {
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await bounded(newPrelude.started, 'new prelude')
        await act(async () => { await vi.advanceTimersByTimeAsync(1_000) })
        expect(screen.getByText(waitingNote)).toBeDefined()
      } finally { vi.useRealTimers() }
      // The old prelude settles after its lifecycle ended. The new wait stays visible, and no obsolete request starts.
      oldPrelude.release()
      await settle()
      expect(screen.getByText(waitingNote)).toBeDefined()
      expect(sent).toHaveLength(0)
      newPrelude.release()
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      expect(sent[0].waiting).toBe(false)
      sent[0].answer(recordedResponse(sent[0].body))
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
    })
  })

  describe('T5 identify production storage stages', () => {
    it('the rendered sync click runs the strict repair transaction alone, after no competing repair or read', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const instrument: Instrument = instrumentTransactions()
      const repairs = vi.spyOn(page.coordinator, 'repairAll')
      try {
        const { scan } = await mintOn(page)
        page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-20T12:00:01.000Z' })
        instrument.inject('abort', { stores: ['occurrences'], mode: 'readwrite', durability: 'strict' })
        expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
        const failedAt = instrument.calls.length
        const reads = queueReads.started
        expect(repairs).not.toHaveBeenCalled()
        const hold = instrument.hold({ stores: ['occurrences'], mode: 'readwrite', durability: 'strict' }, 5_000)
        const fetchMock = stubFetch((body) => Promise.resolve(recordedResponse(body)))
        // Nothing has repaired or read since the failed persist. The next strict write is the one the click starts.
        expect(queueReads.started).toBe(reads)
        expect(instrument.calls.slice(failedAt).filter((entry) => entry.mode === 'readwrite' && entry.options?.durability === 'strict')).toStrictEqual([])
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const call = await bounded(hold.started, 'strict repair transaction')
        expect(instrument.calls.slice(failedAt).find((entry) => entry.mode === 'readwrite' && entry.options?.durability === 'strict')).toBe(call)
        expect(call).toMatchObject({ storeNames: ['occurrences'], mode: 'readwrite', options: { durability: 'strict' } })
        expect(repairs).toHaveBeenCalledTimes(1)
        await screen.findByText(waitingNote, {}, { timeout: 3_000 })
        expect(reconcileCalls(fetchMock)).toBe(0)
        hold.release('commit')
        await waitFor(() => expect(call.events).toContain('complete'), { timeout: BOUND_MS })
        expect(call.events).not.toContain('abort')
        await screen.findByText('Synced 2 offline scan(s).', {}, { timeout: BOUND_MS })
        expect(reconcileCalls(fetchMock)).toBe(1)
        expect(await storedRows()).toHaveLength(2)
      } finally { instrument.restore() }
    }, 15_000)

    it('the recovery list and the queue read each take their own readonly transaction, in that order', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const instrument: Instrument = instrumentTransactions()
      try {
        const base = instrument.calls.length
        const reads = queueReads.started
        const recovery = instrument.hold({ stores: [STORE.occurrences, STORE.legacyClasses], mode: 'readonly', when: (call) => instrument.calls.indexOf(call) >= base }, 5_000)
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const recoveryCall = await bounded(recovery.started, 'recovery list transaction')
        expect(queueReads.started).toBe(reads)
        recovery.release('commit')
        await waitFor(() => expect(recoveryCall.events).toContain('complete'), { timeout: BOUND_MS })
        await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
        await readsSettled()
        const recoveryAt = instrument.calls.indexOf(recoveryCall)
        const queueAt = instrument.calls.findIndex((call, index) => index > recoveryAt && call.mode === 'readonly' && call.storeNames.includes(STORE.legacyClasses))
        // Log entries are "create#N mode stores" and "complete#N". Find each by its id, in log order.
        const logged = (entry: string) => instrument.log.findIndex((line) => line === entry || line.startsWith(`${entry} `))
        expect(recoveryAt).toBeGreaterThanOrEqual(base)
        expect(queueAt).toBeGreaterThan(recoveryAt)
        expect(logged(`complete#${recoveryAt}`)).toBeGreaterThanOrEqual(0)
        expect(logged(`complete#${recoveryAt}`)).toBeLessThan(logged(`create#${queueAt}`))
      } finally { instrument.restore() }
    }, 15_000)
  })

  describe('publication after the store is replaced or the lifecycle ends', () => {
    it('an application that settles after cleanup starts no repair, refresh, or diagnostic', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const apply = gateMethod(page.store(), 'applyReconcileResults').hold()
      // The answer differs from the other connection's recorded result, so the late application is refused as a whole batch.
      stubFetch((body) => Promise.resolve(Response.json({ results: body.occurrences.map((row) => ({ occurrence_id: row.occurrence_id, result: 'synced' })) })))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(apply.started, 'held application')
      cleanup()
      // Another connection retires the row first, so the application that settles after cleanup is refused as a whole batch.
      await retireElsewhere([row])
      const repairs = vi.spyOn(page.coordinator, 'repairAll')
      apply.release()
      await settle()
      expect(repairs).not.toHaveBeenCalled()
    })

    // Storage that stops being ready settles a checking failure to unknown. The held correction belongs to the old store,
    // so its late result publishes nothing. A retry on the current store then corrects the failure from a live read.
    it.each([
      { cause: 'a controller stop', loss: 'stop', late: 'success' },
      { cause: 'a controller stop', loss: 'stop', late: 'rejection' },
      { cause: 'an unexpected close', loss: 'close', late: 'success' },
      { cause: 'an unexpected close', loss: 'close', late: 'rejection' },
    ])('a correction held across $cause settles to unknown, and a late $late publishes nothing', async ({ loss, late }) => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      // A corrupt copy of the queued row adds a per-row warning. A stale correction that published would clear that warning.
      const [queuedRow] = await storedRows() as OccurrenceRecord[]
      await rawPut(dbName, STORE.occurrences, { ...queuedRow, occurrenceId: 'corrupt-row', direction: 'sideways' })
      focus()
      await readsSettled()
      expect(screen.getByText(corruptWarning)).toBeDefined()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      const correction = holdNextQueueRead()
      await act(async () => { first.fail(new TypeError('network down')) })
      await bounded(correction.started, 'failure correction read')
      expect(screen.getByText(checkingOfflineNote)).toBeDefined()
      if (loss === 'stop') page.storage.stop()
      else {
        forceCloseDatabase(page.store().db as never)
        await page.waitFor('failed')
      }
      await settle()
      expect(screen.queryByText(checkingOfflineNote)).toBeNull()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
      expect(screen.getByText(corruptWarning)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
      // An empty result would remove the failure, the count and the warning if the old store could still publish.
      await act(async () => {
        if (late === 'success') correction.result.resolve([])
        else correction.result.reject(new Error('queue read failed'))
      })
      await settle()
      expect(screen.queryByText(checkingOfflineNote)).toBeNull()
      expect(screen.getByText(unknownOfflineNote)).toBeDefined()
      expect(screen.queryByText(offlineNote)).toBeNull()
      expect(screen.getByText(/^1 queued offline scan awaiting sync\./)).toBeDefined()
      expect(screen.getByText(corruptWarning)).toBeDefined()
      expect(screen.getByText(readAlert)).toBeDefined()
      fireEvent.click(screen.getByRole('button', { name: 'Retry storage' }))
      await waitFor(() => expect(page.storage.state().status).toBe('ready'), { timeout: BOUND_MS })
      await readsSettled()
      expect(await screen.findByText(offlineNote, {}, { timeout: BOUND_MS })).toBeDefined()
      expect(screen.queryByText(unknownOfflineNote)).toBeNull()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.getByText(corruptWarning)).toBeDefined()
    })

    // The sync prelude waits in repair while cleanup, a remount or a storage stop happens. When the wait ends, the obsolete
    // lifecycle must start neither recovery nor a queue read. An atomic write that already started is not covered here.
    it('a prelude held across cleanup starts no recovery and no queue read when it settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const recovery = vi.spyOn(page.store(), 'recoverDeadOwners')
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      focus()
      await bounded(prelude.started, 'held prelude')
      cleanup()
      const reads = queueReads.started
      prelude.release()
      await settle()
      expect(recovery).not.toHaveBeenCalled()
      expect(queueReads.started).toBe(reads)
    })

    // Repair finishes, then recovery waits while cleanup happens. The earlier cases hold repair only, so this case checks
    // the guard after recovery. A recovery that settles after cleanup must start no repair and no queue read.
    it('a prelude held in recovery across cleanup starts no queue read when recovery settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const realRecovery = page.store().recoverDeadOwners.bind(page.store())
      const recoveryStarted = deferred<void>()
      const recoveryRelease = deferred<void>()
      const recoveryDone = deferred<void>()
      vi.spyOn(page.store(), 'recoverDeadOwners').mockImplementation(async () => {
        recoveryStarted.resolve()
        await recoveryRelease.promise
        try { return await realRecovery() } finally { recoveryDone.resolve() }
      })
      focus()
      await bounded(recoveryStarted.promise, 'held recovery')
      cleanup()
      const repairs = vi.spyOn(page.coordinator, 'repairAll')
      const reads = queueReads.started
      recoveryRelease.resolve()
      await bounded(recoveryDone.promise, 'released recovery')
      await settle()
      expect(repairs).not.toHaveBeenCalled()
      expect(queueReads.started).toBe(reads)
    })

    it('a prelude held across a remount starts no recovery and no queue read from the old lifecycle', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const recovery = vi.spyOn(page.store(), 'recoverDeadOwners')
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      focus()
      await bounded(prelude.started, 'held prelude')
      cleanup()
      render(<App />)
      await readsSettled()
      const recoveries = recovery.mock.calls.length
      const reads = queueReads.started
      prelude.release()
      await settle()
      expect(recovery).toHaveBeenCalledTimes(recoveries)
      expect(queueReads.started).toBe(reads)
      expect(screen.getByRole('button', { name: 'Sync queued scans' })).toBeDefined()
    })

    it('a prelude held across a storage stop starts no recovery and no queue read when it settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const recovery = vi.spyOn(page.store(), 'recoverDeadOwners')
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      focus()
      await bounded(prelude.started, 'held prelude')
      page.storage.stop()
      const reads = queueReads.started
      prelude.release()
      await settle()
      expect(recovery).not.toHaveBeenCalled()
      expect(queueReads.started).toBe(reads)
    })

    it('an applied batch whose store was replaced while it committed publishes no completed-batch report', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const instrument: Instrument = instrumentTransactions()
      try {
        const hold = instrument.hold({ stores: [STORE.occurrences, STORE.legacyClasses], mode: 'readwrite', durability: 'strict' }, 5_000)
        stubFetch((body) => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const call = await bounded(hold.started, 'held application')
        page.storage.stop()
        hold.release('commit')
        await waitFor(() => expect(call.events).toContain('complete'), { timeout: BOUND_MS })
        await settle()
        expect(screen.queryByText(/^Completed batches:/)).toBeNull()
        expect(screen.queryByText(/^Synced /)).toBeNull()
      } finally { instrument.restore() }
    })
  })

  // A response whose status and headers arrive at once. Its body waits until the test releases it.
  function heldBody(result: unknown) {
    const release = deferred<void>()
    const stream = new ReadableStream<Uint8Array>({
      async pull(controller) {
        await release.promise
        controller.enqueue(new TextEncoder().encode(JSON.stringify(result)))
        controller.close()
      },
    })
    return { response: new Response(stream, { status: 200, headers: { 'Content-Type': 'application/json' } }), release: () => release.resolve() }
  }
  const refusalQr = (tid: string) => `header.${btoa(JSON.stringify({ tid })).replace(/=+$/, '')}.signature`
  const listedTicket = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
  const lastFetch = () => globalThis.fetch as unknown as ReturnType<typeof stubFetch>

  describe('T6 pairing, sent results, requested work and cleanup', () => {
    it('a sent old-pairing conflict, held across a replacement, applies once without abort or resend, and the follow-up waits for it', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('sent-conflict')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const body = heldBody({ results: [{ occurrence_id: row.occurrenceId, result: 'conflict' }] })
      const signals: AbortSignal[] = []
      const fetchMock = stubFetch((_body, init) => { signals.push(init.signal!); return Promise.resolve(body.response) })
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(signals).toHaveLength(1), { timeout: BOUND_MS })
      commitPairing('replacement-token')
      await readsSettled()
      const beforeRequest = queueReads.started
      window.dispatchEvent(new Event('online'))
      await readsSettled()
      expect(signals[0].aborted).toBe(false)
      expect(reconcileCalls(fetchMock)).toBe(1)
      body.release()
      await waitFor(async () => expect(await storedRows()).toStrictEqual([terminalRow(row, 'conflict')]), { timeout: BOUND_MS })
      await readsSettled()
      expect(queueReads.started).toBeGreaterThan(beforeRequest)
      expect(reconcileCalls(fetchMock)).toBe(1)
      expect(sentTokens(fetchMock)).toStrictEqual(['paired-device-token'])
      expect(screen.queryByText(/^Synced /)).toBeNull()
      expect(screen.queryByText(/^Completed batches:/)).toBeNull()
      expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
    })

    it('a sent old-pairing batch applied after a cross-tab removal is refreshed with no follow-up, and shows the exact final count', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('applied-old')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const body = heldBody({ results: [{ occurrence_id: row.occurrenceId, result: 'recorded' }] })
      const fetchMock = stubFetch(() => Promise.resolve(body.response))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(reconcileCalls(fetchMock)).toBe(1), { timeout: BOUND_MS })
      // Another tab removes the pairing. Its refresh reads the row that is still queued.
      const reads = queueReads.started
      tokenEvent(null)
      await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
      await readsSettled()
      expect(screen.getByText(/offline scan.* still saved on this device/)).toBeDefined()
      const beforeApply = queueReads.started
      body.release()
      await waitFor(async () => expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')]), { timeout: BOUND_MS })
      await readsSettled()
      expect(queueReads.started).toBeGreaterThan(beforeApply)
      expect(screen.queryByText(/still saved on this device/)).toBeNull()
      expect(screen.queryByText(/^Synced /)).toBeNull()
      expect(reconcileCalls(fetchMock)).toBe(1)
    })

    it('an incompatible old-pairing batch after a cross-tab removal refreshes the exact final count with no follow-up', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('refused-old')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const body = heldBody({ results: [{ occurrence_id: row.occurrenceId, result: 'synced' }] })
      const fetchMock = stubFetch(() => Promise.resolve(body.response))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(reconcileCalls(fetchMock)).toBe(1), { timeout: BOUND_MS })
      const reads = queueReads.started
      tokenEvent(null)
      await waitFor(() => expect(queueReads.started).toBeGreaterThan(reads), { timeout: BOUND_MS })
      await readsSettled()
      expect(screen.getByText(/offline scan.* still saved on this device/)).toBeDefined()
      // Another connection retires the row while the response is held. The refused batch then changes nothing.
      await retireElsewhere([row])
      const beforeRefusal = queueReads.started
      body.release()
      await waitFor(async () => expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')]), { timeout: BOUND_MS })
      await readsSettled()
      expect(queueReads.started).toBeGreaterThan(beforeRefusal)
      expect(screen.queryByText(/still saved on this device/)).toBeNull()
      expect(screen.queryByText(readAlert)).toBeNull()
      expect(screen.queryByText(writeDiagnostic)).toBeNull()
      expect(reconcileCalls(fetchMock)).toBe(1)
    })

    it('a held application transaction holds back a new pass across a replacement and a new request until it settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('held-apply')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const instrument: Instrument = instrumentTransactions()
      try {
        const hold = instrument.hold({ stores: [STORE.occurrences, STORE.legacyClasses], mode: 'readwrite', durability: 'strict' }, 5_000)
        const fetchMock = stubFetch((body) => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const call = await bounded(hold.started, 'held application')
        commitPairing('replacement-token')
        window.dispatchEvent(new Event('online'))
        // Storage reads queue behind the held write, so this pause reads nothing from storage.
        await act(async () => { await new Promise((resolve) => setTimeout(resolve, 50)) })
        expect(reconcileCalls(fetchMock)).toBe(1)
        hold.release('commit')
        await waitFor(() => expect(call.events).toContain('complete'), { timeout: BOUND_MS })
        await readsSettled()
        expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
        expect(reconcileCalls(fetchMock)).toBe(1)
        expect(sentTokens(fetchMock)).toStrictEqual(['paired-device-token'])
      } finally { instrument.restore() }
    }, 15_000)

    it('a request made before a replacement survives into the follow-up, which sends the remaining row with the replacement token', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('first-row')
      await readsSettled()
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      await queueOneScan('second-row')
      window.dispatchEvent(new Event('online'))
      commitPairing('replacement-token')
      await settle()
      await act(async () => { first.answer(recordedResponse(first.body)) })
      const follow = await bounded(held.next(), 'requested follow-up')
      const second = (await storedRows() as OccurrenceRecord[]).find((row) => row.qrPayload === 'second-row')!
      expect(follow.body.occurrences).toStrictEqual([{ occurrence_id: second.occurrenceId, qr_payload: 'second-row', occurred_at: second.occurredAt, event_type: 'entry' }])
      // A request made while the new pass is held coalesces into one further pass, which finds nothing to send.
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await act(async () => { follow.answer(recordedResponse(follow.body)) })
      await readsSettled()
      expect(held).toHaveLength(2)
      expect(sentTokens(lastFetch())).toStrictEqual(['paired-device-token', 'replacement-token'])
    })

    it('a listed Entry refusal submitted while old sent work is held keeps its requested follow-up across a replacement', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      await page.store().mergeRevoked([listedTicket])
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      const qr = refusalQr(listedTicket)
      fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: qr } })
      fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
      await screen.findByRole('heading', { name: 'Not valid for entry' })
      await waitFor(async () => expect((await storedRows() as OccurrenceRecord[]).some((row) => row.localDecision === 'revocation_refused' && row.state === 'QUEUED')).toBe(true), { timeout: BOUND_MS })
      commitPairing('replacement-token')
      await settle()
      await act(async () => { first.answer(recordedResponse(first.body)) })
      const follow = await bounded(held.next(), 'requested follow-up')
      const refusal = (await storedRows() as OccurrenceRecord[]).find((row) => row.localDecision === 'revocation_refused')!
      expect(follow.body.occurrences).toStrictEqual([{ occurrence_id: refusal.occurrenceId, qr_payload: qr, occurred_at: refusal.occurredAt, local_decision: 'revocation_refused' }])
      await act(async () => { follow.answer(recordedResponse(follow.body)) })
      await readsSettled()
      expect(sentTokens(lastFetch())).toStrictEqual(['paired-device-token', 'replacement-token'])
    })

    it('a listed refusal whose pass waits in its prelude sends one request, with the replacement token and the exact refusal identity', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      await page.store().mergeRevoked([listedTicket])
      const held = holdReconciles()
      const prelude = gateMethod(page.coordinator, 'repairAll').hold()
      const qr = refusalQr(listedTicket)
      fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: qr } })
      fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
      await bounded(prelude.started, 'refusal-triggered prelude')
      commitPairing('replacement-token')
      prelude.release()
      const request = await bounded(held.next(), 'refusal-triggered request')
      const refusal = (await storedRows() as OccurrenceRecord[]).find((row) => row.localDecision === 'revocation_refused')!
      expect(request.body.occurrences).toHaveLength(2)
      expect(request.body.occurrences).toContainEqual({ occurrence_id: refusal.occurrenceId, qr_payload: qr, occurred_at: refusal.occurredAt, local_decision: 'revocation_refused' })
      await act(async () => { request.answer(recordedResponse(request.body)) })
      await readsSettled()
      expect(held).toHaveLength(1)
      expect(sentTokens(lastFetch())).toStrictEqual(['replacement-token'])
    })

    it.each(['fetch rejection', 'unauthorized', 'malformed body', 'valid body'] as const)('an old-pairing sent batch ending in %s cannot unpair the replacement or send another old batch', async (outcome) => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('old-outcome')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const held = holdReconciles()
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      const first = await held.next()
      commitPairing('replacement-token')
      await settle()
      await act(async () => {
        if (outcome === 'fetch rejection') first.fail(new TypeError('network down'))
        else if (outcome === 'unauthorized') first.answer(new Response('', { status: 401 }))
        else if (outcome === 'malformed body') first.answer(Response.json({ results: [] }))
        else first.answer(recordedResponse(first.body))
      })
      await readsSettled()
      expect(screen.queryByText(/This device is not paired/)).toBeNull()
      expect(screen.getByRole('button', { name: 'Check ticket' })).toBeDefined()
      expect(screen.queryByText(/^Sync failed:/)).toBeNull()
      expect(held).toHaveLength(1)
      expect(sentTokens(lastFetch())).toStrictEqual(['paired-device-token'])
      expect(await storedRows()).toStrictEqual(outcome === 'valid body' ? [terminalRow(row, 'recorded')] : [row])
    })

    // A legacy row whose credential is not a string goes alone in its own request. The row sorts first, so it is sent first.
    // Its 422 under a replaced pairing ends the pass. A 422 under the current pairing reports the row and continues.
    it('an isolated legacy 422 that arrives after a replacement reports nothing, unpairs nothing and sends nothing more', async () => {
      page.destroy()
      dbName = `app-sync-legacy-${crypto.randomUUID()}`
      const time = '2026-10-02T10:00:00.123Z'
      const odd = { occurrenceId: 'a-odd', qrPayload: 42, occurredAt: time, state: 'QUEUED', actuated: false, createdAt: time }
      const control = { occurrenceId: 'z-control', qrPayload: 'qr-z-control', occurredAt: time, state: 'QUEUED', actuated: false, createdAt: time }
      await seedDatabase(dbName, 2, V2_STORES, { occurrences: [odd, control] })
      page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
      const seeded = await storedRows() as OccurrenceRecord[]
      const seededClasses = await rawGetAll(dbName, STORE.legacyClasses)
      const sent: Array<{ token: string; raw: string }> = []
      const answers: Array<(response: Response) => void> = []
      stubFetch((_body, init) => {
        sent.push({ token: (init.headers as Record<string, string>)['X-Scanner-Token'], raw: init.body as string })
        return new Promise<Response>((resolve) => { answers.push(resolve) })
      })
      render(<App />)
      fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(sent).toHaveLength(1), { timeout: BOUND_MS })
      const oddWire = `{"occurrences":[{"qr_payload":42,"occurrence_id":"a-odd","occurred_at":"${time}"}]}`
      expect(sent[0]).toStrictEqual({ token: 'paired-device-token', raw: oddWire })
      commitPairing('replacement-token')
      await readsSettled()
      await act(async () => { answers[0](new Response('{"error":"refused"}', { status: 422 })) })
      await readsSettled()
      expect(sent).toHaveLength(1)
      expect(localStorage.getItem('scanner.device-token')).toBe('replacement-token')
      expect(screen.queryByText(/This device is not paired/)).toBeNull()
      expect(screen.queryByText(/^Pre-upgrade record a-odd could not sync/)).toBeNull()
      expect(screen.queryByText(/^Completed batches:/)).toBeNull()
      expect(screen.queryByText(/^Synced /)).toBeNull()
      expect(await storedRows()).toStrictEqual(seeded)
      expect(await rawGetAll(dbName, STORE.legacyClasses)).toStrictEqual(seededClasses)
      // Control: under the replacement token, the same refusal reports its row, and the pass goes on to the control row.
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(sent).toHaveLength(2), { timeout: BOUND_MS })
      expect(sent[1]).toStrictEqual({ token: 'replacement-token', raw: oddWire })
      await act(async () => { answers[1](new Response('{"error":"refused"}', { status: 422 })) })
      await waitFor(() => expect(sent).toHaveLength(3), { timeout: BOUND_MS })
      expect(sent[2]).toStrictEqual({ token: 'replacement-token', raw: `{"occurrences":[{"qr_payload":"qr-z-control","occurrence_id":"z-control","occurred_at":"${time}"}]}` })
      await act(async () => { answers[2](new Response(JSON.stringify({ results: [{ occurrence_id: 'z-control', result: 'recorded' }] }), { status: 200 })) })
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
      expect(screen.getByText('Pre-upgrade record a-odd could not sync. It remains queued and listed in the export.')).toBeDefined()
      expect(await storedRows()).toStrictEqual([seeded[0], { ...seeded[1], state: 'SYNCED', result: 'recorded' }])
    })

    it('cleanup during a held initial read starts no request when that read later settles', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      const read = holdNextQueueRead()
      const fetchMock = stubFetch(() => Promise.resolve(recordedResponse({ occurrences: [] })))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await bounded(read.started, 'held initial read')
      cleanup()
      read.result.resolve(rows)
      await settle()
      expect(reconcileCalls(fetchMock)).toBe(0)
      expect(await storedRows()).toStrictEqual(rows)
    })

    it.each(['fetch, abort aware', 'fetch, abort ignoring', 'body, abort ignoring'] as const)('cleanup during a held %s applies nothing and starts no further request', async (variant) => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('cleanup-row')
      await readsSettled()
      const rows = await storedRows() as OccurrenceRecord[]
      const [row] = rows
      const late = deferred<Response>()
      const body = heldBody({ results: [{ occurrence_id: row.occurrenceId, result: 'recorded' }] })
      const signals: AbortSignal[] = []
      const fetchMock = stubFetch((_body, init) => {
        signals.push(init.signal!)
        if (variant === 'fetch, abort aware') return new Promise<Response>((_, fail) => init.signal!.addEventListener('abort', () => fail(new DOMException('aborted', 'AbortError'))))
        if (variant === 'fetch, abort ignoring') return late.promise
        return Promise.resolve(body.response)
      })
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await waitFor(() => expect(signals).toHaveLength(1), { timeout: BOUND_MS })
      cleanup()
      expect(signals[0].aborted).toBe(true)
      late.resolve(recordedResponse({ occurrences: [{ occurrence_id: row.occurrenceId }] }))
      body.release()
      await settle()
      expect(await storedRows()).toStrictEqual(rows)
      expect(reconcileCalls(fetchMock)).toBe(1)
      render(<App />)
      await readsSettled()
      expect(screen.getByRole('button', { name: 'Sync queued scans' })).toBeDefined()
      expect(screen.queryByText(/^Synced /)).toBeNull()
    })
  })

  describe('T7 already-started commits and page lifetime', () => {
    it('a held application transaction that started before cleanup commits, with no obsolete report or further batch', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('applied-across-cleanup')
      await readsSettled()
      const [row] = await storedRows() as OccurrenceRecord[]
      const instrument: Instrument = instrumentTransactions()
      try {
        const hold = instrument.hold({ stores: [STORE.occurrences, STORE.legacyClasses], mode: 'readwrite', durability: 'strict' }, 5_000)
        const fetchMock = stubFetch((body) => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        const call = await bounded(hold.started, 'held application')
        cleanup()
        hold.release('commit')
        await waitFor(() => expect(call.events).toContain('complete'), { timeout: BOUND_MS })
        render(<App />)
        await readsSettled()
        expect(await storedRows()).toStrictEqual([terminalRow(row, 'recorded')])
        expect(screen.queryByText(/^Synced /)).toBeNull()
        expect(screen.queryByText(/^Completed batches:/)).toBeNull()
        expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
        expect(reconcileCalls(fetchMock)).toBe(1)
      } finally { instrument.restore() }
    })

    it('a replacement during a held second batch lets its valid result apply and refresh, with no new old-pairing report', async () => {
      const { fixture, held, second } = await startMixedSync()
      await expectPartial(fixture)
      commitPairing('replacement-token')
      await settle()
      const reads = queueReads.started
      await act(async () => { second.answer(mixedResponse(second.body, fixture)) })
      await waitFor(async () => expect(await storedRows()).toStrictEqual(expectedMixedRows(fixture, 105)), { timeout: BOUND_MS })
      await readsSettled()
      expect(queueReads.started).toBeGreaterThan(reads)
      expect(screen.queryByText(mixedSyncNote)).toBeNull()
      expect(screen.getByText(partialSyncNote)).toBeDefined()
      expectRejectionWarnings(fixture, 100)
      expect(held).toHaveLength(2)
    }, 15_000)

    it('cleanup while the second batch is in flight keeps the first batch reports and applies nothing from the second', async () => {
      const { fixture, held, second } = await startMixedSync()
      await expectPartial(fixture)
      cleanup()
      await act(async () => { second.answer(mixedResponse(second.body, fixture)) })
      await settle()
      expect(await storedRows()).toStrictEqual(expectedMixedRows(fixture, 100))
      expect(held).toHaveLength(2)
    }, 15_000)

    it('a remount on the same page keeps a protected pending row and the queued control, and the page lock stays held', async () => {
      render(<App />)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan('control')
      await readsSettled()
      const [control] = await storedRows() as OccurrenceRecord[]
      const instrument: Instrument = instrumentTransactions()
      try {
        const { scan } = await mintOn(page)
        page.coordinator.latch({ kind: 'terminal', occurrenceId: scan.occurrenceId, outcome: { family: 'terminal', cause: 'local_failure', requestMayHaveBeenSent: false }, decidedAt: '2026-10-20T12:00:02.000Z' })
        for (let attempt = 0; attempt < 8; attempt += 1) instrument.inject('abort', { stores: ['occurrences'], mode: 'readwrite', durability: 'strict' })
        expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('failed')
        const protectedBefore = (await storedRows() as OccurrenceRecord[]).find((row) => row.occurrenceId === scan.occurrenceId)
        expect(protectedBefore).toMatchObject({ state: 'PENDING', occurrenceId: scan.occurrenceId, qrPayload: scan.qrPayload })
        cleanup()
        render(<App />)
        await readsSettled()
        expect(page.coordinator.lockHeld()).toBe(true)
        const other = await sharedPage()
        await other.store().recoverDeadOwners()
        // The owner may renew the lease of its own pending row. Every other field of the captured row must stay exactly as it was.
        const protectedAfter = (await storedRows() as OccurrenceRecord[]).find((row) => row.occurrenceId === scan.occurrenceId)
        expect({ ...protectedAfter, leaseExpiresAt: undefined }).toStrictEqual({ ...protectedBefore, leaseExpiresAt: undefined })
        expect(protectedAfter!.leaseExpiresAt).toBeGreaterThanOrEqual(protectedBefore!.leaseExpiresAt)
        const fetchMock = stubFetch((body) => Promise.resolve(recordedResponse(body)))
        fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
        await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
        expect(sentIds(fetchMock)).toStrictEqual([control.occurrenceId])
        // The queued control reaches its exact terminal row. The protected row is still the captured pending row.
        const rowsAfter = await storedRows() as OccurrenceRecord[]
        expect(rowsAfter.find((row) => row.occurrenceId === control.occurrenceId)).toStrictEqual(terminalRow(control, 'recorded'))
        const stillProtected = rowsAfter.find((row) => row.occurrenceId === scan.occurrenceId)
        expect({ ...stillProtected, leaseExpiresAt: undefined }).toStrictEqual({ ...protectedBefore, leaseExpiresAt: undefined })
        expect(stillProtected!.leaseExpiresAt).toBeGreaterThanOrEqual(protectedBefore!.leaseExpiresAt)
      } finally { instrument.restore() }
    })

    it('a sync under StrictMode uses the lifecycle that the double effect leaves in place', async () => {
      render(<StrictMode><App /></StrictMode>)
      stubFetch(() => Promise.reject(new TypeError('not yet')))
      await queueOneScan()
      await readsSettled()
      const fetchMock = stubFetch((body) => Promise.resolve(recordedResponse(body)))
      fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
      await screen.findByText('Synced 1 offline scan(s).', {}, { timeout: BOUND_MS })
      expect(reconcileCalls(fetchMock)).toBe(1)
      await readsSettled()
    })
  })
})
