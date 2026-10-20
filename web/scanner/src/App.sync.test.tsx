import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { StrictMode } from 'react'
import App, { reconcileTimeoutMs } from './App'
import type { CurrentRecord as OccurrenceRecord } from './occurrence-model'
import type { QueuedEntry } from './occurrence-store'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { rawGetAll } from './test/raw-db'
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

beforeEach(async () => {
  dbName = `app-sync-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  wrapQueueReads()
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
    expect(await screen.findByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')).toBeDefined()
    expect(queueReads.failMarkSyncedNext).toBe(false)
    expect(screen.queryByText(/^Synced /)).toBeNull()
    expect(await storedRows()).toEqual(before)

    stubFetch(() => Promise.reject(new TypeError('network down')))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    expect(await screen.findByText(offlineNote)).toBeDefined()
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
    await queueOneScan()
    await readsSettled()
    const rows = await storedRows() as OccurrenceRecord[]
    const held = holdReconciles()
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    const first = await held.next()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const firstRead = holdNextQueueRead()
    window.dispatchEvent(new Event('online'))
    await act(async () => {
      first.fail(new TypeError('network down'))
      await firstRead.started
    })
    expect(screen.getByText(checkingOfflineNote)).toBeDefined()
    const second = await held.next()
    const secondRead = holdNextQueueRead()
    await act(async () => {
      second.answer(new Response('', { status: 502 }))
      await secondRead.started
    })
    expect(screen.getByText('Sync failed: The server answered but the reply could not be read. Checking the scans saved on this device…')).toBeDefined()
    await act(async () => { secondRead.result.resolve(rows) })
    expect(screen.getByText(unreadableNote)).toBeDefined()
    await act(async () => { firstRead.result.resolve([]) })
    expect(screen.getByText(unreadableNote)).toBeDefined()
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
    // Pairing preserves the existing failure.
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Sync queued scans' })
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })
})
