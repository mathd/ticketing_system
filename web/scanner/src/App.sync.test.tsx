import { afterEach, beforeEach, describe, expect, it, onTestFinished, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { StrictMode } from 'react'
import App, { reconcileTimeoutMs } from './App'
import type { OccurrenceRecord } from './occurrences'

// Controls the failure-path read without changing the stored rows.
const queueReads = vi.hoisted(() => ({
  started: 0,
  finished: 0,
  failNext: false,
  failMarkSyncedNext: false,
  // Calls the page made to the production store's two record writers.
  mints: 0,
  refusals: 0,
  onStarted: null as (() => void) | null,
  holdNext: null as { result: Promise<OccurrenceRecord[]>; started: () => void } | null,
}))

vi.mock('./occurrences', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./occurrences')>()
  return {
    ...actual,
    async openOccurrenceStore(...args: Parameters<typeof actual.openOccurrenceStore>) {
      const store = await actual.openOccurrenceStore(...args)
      return {
        ...store,
        async mint(...args: Parameters<typeof store.mint>) {
          queueReads.mints += 1
          return store.mint(...args)
        },
        async mintRefusal(...args: Parameters<typeof store.mintRefusal>) {
          queueReads.refusals += 1
          return store.mintRefusal(...args)
        },
        async markSynced(...args: Parameters<typeof store.markSynced>) {
          if (queueReads.failMarkSyncedNext) {
            queueReads.failMarkSyncedNext = false
            throw new Error('terminal write failed')
          }
          return store.markSynced(...args)
        },
        async queued() {
          queueReads.started += 1
          queueReads.onStarted?.()
          try {
            if (queueReads.failNext) {
              queueReads.failNext = false
              throw new Error('queue read failed')
            }
            const held = queueReads.holdNext
            if (held) {
              queueReads.holdNext = null
              held.started()
              return await held.result
            }
            return await store.queued()
          } finally {
            queueReads.finished += 1
          }
        },
      }
    },
  }
})

// TKT-315: a failed sync of the offline queue must SAY so. It used to return silently on
// a 5xx and swallow a rejected fetch or an unreadable body in a bare catch, so a venue
// could not tell "all reconciled" from "failing for three hours". Every failure leaves the
// queue exactly as it was (ADR-066): these tests compare the stored rows, not only the text.

const feedResponse = () => new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
const checkingOfflineNote = 'Sync failed: No connection. Checking the scans saved on this device…'
const unknownOfflineNote = 'Sync failed: No connection. Any scans saved on this device will be sent on the next sync. Last successful sync this session: none.'
const offlineNote = /Sync failed: No connection\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./
const unreadableNote = /Sync failed: The server answered but the reply could not be read\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./

beforeEach(() => {
  // A fresh IndexedDB per test: a queued row left by one test would change the next one's count.
  vi.stubGlobal('indexedDB', new IDBFactory())
  queueReads.failNext = false
  queueReads.failMarkSyncedNext = false
  queueReads.mints = 0
  queueReads.refusals = 0
  queueReads.holdNext = null
  queueReads.onStarted = null
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

type ReconcileBody = { occurrences: { occurrence_id: string }[] }
type Reconcile = (body: ReconcileBody, init: RequestInit) => Promise<Response>

// Every fetch the page makes: the revocation feed always answers, a scan is always offline
// (that is how a row gets queued), and the reconcile answer is the test's.
function stubFetch(reconcile: Reconcile) {
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const u = String(url)
    if (u.includes('voided-tickets')) return Promise.resolve(feedResponse())
    if (u.endsWith('/reconciliations')) return reconcile(JSON.parse(init!.body as string), init!)
    return Promise.reject(new TypeError('network down'))
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

// Waits for a queue read that starts after `before`, which the page makes once a request was sent.
async function readsSince(before: number) {
  await waitFor(() => expect(queueReads.started).toBeGreaterThan(before))
  await readsSettled()
}

async function queueOneScan(value = 'offline-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
  await screen.findByRole('heading', { name: 'Queued offline' })
  // The result shows before submit has finished: it still refreshes the queue
  // count, and the button reads "Checking…" until then. Wait for the scan to end,
  // so the next step does not race it.
  await screen.findByRole('button', { name: 'Check ticket' })
}

// The stored occurrence rows, read straight from IndexedDB — not through the app.
async function storedRows(): Promise<unknown[]> {
  const db = await new Promise<IDBDatabase>((resolve, reject) => {
    const open = indexedDB.open('gate-occurrences')
    open.onsuccess = () => resolve(open.result)
    open.onerror = () => reject(open.error)
  })
  try {
    return await new Promise((resolve, reject) => {
      const request = db.transaction('occurrences', 'readonly').objectStore('occurrences').getAll()
      request.onsuccess = () => resolve(request.result)
      request.onerror = () => reject(request.error)
    })
  } finally {
    db.close()
  }
}

// Marks every stored row SYNCED, as another tab would, without going through the app.
async function markStoredRowsSynced() {
  const db = await new Promise<IDBDatabase>((resolve) => {
    const open = indexedDB.open('gate-occurrences')
    open.onsuccess = () => resolve(open.result)
  })
  await new Promise<void>((resolve) => {
    const tx = db.transaction('occurrences', 'readwrite')
    const os = tx.objectStore('occurrences')
    os.getAll().onsuccess = (e) => {
      for (const row of (e.target as IDBRequest).result) os.put({ ...row, state: 'SYNCED' })
    }
    tx.oncomplete = () => resolve()
  })
  db.close()
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
    expect((await screen.findByRole('alert')).textContent).toBe('This device cannot save scans right now. No ticket was checked. Try again after restoring browser storage.')
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

  it('does not report no connection when reconcile request construction throws', async () => {
    const { default: process } = await vi.importActual<{
      default: {
        on(event: 'unhandledRejection', listener: (reason: unknown) => void): void
        off(event: 'unhandledRejection', listener: (reason: unknown) => void): void
      }
    }>('node:process')
    const { setImmediate } = await vi.importActual<{ setImmediate(callback: () => void): void }>('node:timers')
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    await readsSettled()
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
    const error = new Error('reconcile construction failed')
    const errors: unknown[] = []
    const catchRejection = (reason: unknown) => { errors.push(reason) }
    const constructed = deferred<void>()
    const stringify = JSON.stringify
    const spy = vi.spyOn(JSON, 'stringify').mockImplementation((...args) => {
      const value: unknown = args[0]
      if (value && typeof value === 'object' && 'occurrences' in value) {
        constructed.resolve()
        throw error
      }
      return stringify(...args)
    })
    // The click handler discards the promise. Catch and assert its rejection here. The cleanup
    // runs even when the test times out, so a leaked listener cannot hide a later rejection.
    process.on('unhandledRejection', catchRejection)
    onTestFinished(() => {
      process.off('unhandledRejection', catchRejection)
      spy.mockRestore()
    })
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await constructed.promise
    await readsSettled()
    await new Promise<void>((resolve) => setImmediate(resolve))
    expect(screen.queryByText(/^Sync failed: No connection/)).toBeNull()
    expect(errors).toEqual([error])
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(false)
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
    expect(await storedRows()).toEqual(before)
    expect(screen.getByText(offlineNote)).toBeDefined()
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
    expect(screen.getByText(unreadableNote)).toBeDefined()
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

  it('a failure that finds the queue empty drops a failure carried under the pairing instruction', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    // A 401 unpairs the device. The instruction shows.
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByLabelText(/pairing token/i)
    // A 502 while unpaired carries its failure under the instruction.
    let readsBeforeReject = 0
    stubFetch(() => {
      readsBeforeReject = queueReads.started
      return Promise.resolve(new Response('', { status: 502 }))
    })
    window.dispatchEvent(new Event('online'))
    await readsSince(readsBeforeReject)
    // Attempt B (the online edge) sends X and hangs. Another tab marks X synced meanwhile.
    const held = holdReconciles()
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(1))
    await markStoredRowsSynced()
    // B fails once the queue is empty. The carried failure says scans are saved, so it must go.
    held[0].fail(new TypeError('network down'))
    await readsSettled()
    // Pairing shows any failure that is still carried. There must be none.
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Check ticket' })
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
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
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('an empty queue drops a failure carried under the pairing instruction', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    // A 401 unpairs the device. The instruction stays, and the queue is untouched.
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByLabelText(/pairing token/i)
    // While unpaired, a 502 carries its failure under the instruction. Nothing shows it until pairing.
    let readsBeforeReject = 0
    stubFetch(() => {
      readsBeforeReject = queueReads.started
      return Promise.resolve(new Response('', { status: 502 }))
    })
    window.dispatchEvent(new Event('online'))
    await readsSince(readsBeforeReject)
    // Another tab sends the row. The next sync finds nothing to send, so the carried failure is no longer true.
    await markStoredRowsSynced()
    const empty = stubFetch(() => Promise.reject(new TypeError('must not be called')))
    window.dispatchEvent(new Event('online'))
    // The queue count is the visible part of the empty-queue sync. Waiting for it cannot pass early.
    await waitFor(() => expect(screen.queryByText(/still saved on this device/)).toBeNull())
    // Pairing shows any failure that is still carried. There must be none.
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Check ticket' })
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    expect(empty.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(false)
  })
})

describe('where the failure note renders (COS6)', () => {
  it('renders in the paired view', async () => {
    await failSyncWith(() => Promise.reject(new TypeError('network down')), offlineNote)
    const button = screen.getByRole('button', { name: 'Sync queued scans' })
    expect(within(button.closest('section') ?? document.body).getByText(offlineNote)).toBeDefined()
  })

  it('renders in the pairing view, and pairing does not clear it — nothing has been reconciled yet', async () => {
    // Queue a scan while paired, then unpair and reload the page.
    const first = render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    first.unmount()
    localStorage.removeItem('scanner.device-token')
    stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
    render(<App />)
    await screen.findByLabelText(/pairing token/i)
    window.dispatchEvent(new Event('online'))
    expect(await screen.findByText(unreadableNote)).toBeDefined()

    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Sync queued scans' })
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })

  it('does not replace the standing "not paired" instruction', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    const pairing = 'This device is not paired. Enter its pairing token to sync the queued scans.'
    expect(await screen.findByText(pairing)).toBeDefined()
    const fetchMock = stubFetch(() => Promise.resolve(new Response('', { status: 502 })))
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(true))
    await settle()
    expect(screen.getByText(pairing)).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()

    // Pairing reconciles nothing: the failure carried under the instruction is shown now.
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Sync queued scans' })
    expect(await screen.findByText(unreadableNote)).toBeDefined()
  })

  it('a repeated 401 keeps the failure carried under the pairing instruction', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByLabelText(/pairing token/i)
    // While unpaired, a 502 carries its failure under the instruction. Nothing shows it until pairing.
    let readsBeforeReject = 0
    stubFetch(() => {
      readsBeforeReject = queueReads.started
      return Promise.resolve(new Response('', { status: 502 }))
    })
    window.dispatchEvent(new Event('online'))
    await readsSince(readsBeforeReject)
    // Still unpaired. A second 401 must keep that failure.
    const unauthorized = stubFetch(() => Promise.resolve(new Response('', { status: 401 })))
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(unauthorized.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(true))
    await settle()
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Sync queued scans' })
    expect(await screen.findByText(unreadableNote)).toBeDefined()
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
    expect(await screen.findByText('This device is not paired. Enter its pairing token to sync the queued scans.')).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
    // Pairing shows the failure that was carried.
    fireEvent.change(screen.getByLabelText(/pairing token/i), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: /pair/i }))
    await screen.findByRole('button', { name: 'Sync queued scans' })
    expect(screen.getByText(unreadableNote)).toBeDefined()
  })
})

// Exit mode (TKT-399): the direction is a fact of the stored occurrence. The rows
// below are read and written in IndexedDB directly, so no fake can supply it.
function chooseDirection(name: 'Entry' | 'Exit') {
  fireEvent.click(screen.getByRole('radio', { name }))
}

// Writes rows into the scanner database before the page opens it. Same version and
// stores as the production schema, so the page opens it without an upgrade.
async function seedRows(rows: Record<string, unknown>[]) {
  const db = await new Promise<IDBDatabase>((resolve, reject) => {
    const open = indexedDB.open('gate-occurrences', 2)
    open.onupgradeneeded = () => {
      open.result.createObjectStore('occurrences', { keyPath: 'occurrenceId' })
      open.result.createObjectStore('revocations', { keyPath: 'ticketID' })
      open.result.createObjectStore('revocation_meta', { keyPath: 'key' })
    }
    open.onsuccess = () => resolve(open.result)
    open.onerror = () => reject(open.error)
  })
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction('occurrences', 'readwrite')
    for (const row of rows) tx.objectStore('occurrences').put(row)
    tx.oncomplete = () => resolve()
    tx.onerror = () => reject(tx.error)
  })
  db.close()
}

type SentOccurrence = Record<string, unknown> & { occurrence_id: string }

async function syncAndCapture(): Promise<SentOccurrence[]> {
  const sent: SentOccurrence[][] = []
  stubFetch((body) => {
    sent.push(body.occurrences as SentOccurrence[])
    return Promise.resolve(recordedResponse(body))
  })
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText(/^Synced \d+ offline scan\(s\)\.$/)
  expect(sent).toHaveLength(1)
  return sent[0]
}

describe('the direction of a queued occurrence', () => {
  // A row written before TKT-399 has no direction. Access reads an omitted
  // event_type as an entry, which is what the row always meant. The current mode
  // must not decide it.
  it('reconciles a legacy row as entry while exit mode is selected', async () => {
    const legacy = {
      occurrenceId: 'c0d2a3f4-5b6c-4d7e-8f90-a1b2c3d4e5f6',
      qrPayload: 'legacy-ticket',
      occurredAt: '2026-09-30T21:00:00.000Z',
      state: 'QUEUED',
      actuated: false,
      createdAt: '2026-09-30T21:00:00.000Z',
      ownerId: 'page-before-upgrade',
      leaseExpiresAt: 1,
    }
    await seedRows([legacy])
    render(<App />)
    chooseDirection('Exit')
    expect(await syncAndCapture()).toEqual([
      { qr_payload: 'legacy-ticket', occurrence_id: legacy.occurrenceId, occurred_at: legacy.occurredAt },
    ])
  })

  // D13: a stored value that is not entry or exit is never sent as an entry, and
  // never sent in a form that fails the whole batch. Access refuses the row alone.
  it.each([
    ['absent', false, undefined, undefined],
    ['an own undefined', true, undefined, 'invalid'],
    ['null', true, null, 'invalid'],
    ['an empty string', true, '', 'invalid'],
    ['the number 1', true, 1, 'invalid'],
    ['EXIT in capitals', true, 'EXIT', 'EXIT'],
    ['entry', true, 'entry', 'entry'],
    ['exit', true, 'exit', 'exit'],
  ] as const)('reconciles a stored direction that is %s while exit mode is selected', async (_name, own, value, expected) => {
    const row: Record<string, unknown> = {
      occurrenceId: 'd1e2f3a4-b5c6-4d7e-8f90-a1b2c3d4e5f6',
      qrPayload: 'stored-ticket',
      occurredAt: '2026-10-16T18:00:00.000Z',
      state: 'QUEUED',
      actuated: false,
      createdAt: '2026-10-16T18:00:00.000Z',
    }
    if (own) row.direction = value
    await seedRows([row])
    // The fixture must hold the state it names: structured clone keeps an own
    // undefined, and that is the case a `??` check reads as a legacy row.
    const [stored] = await storedRows() as Record<string, unknown>[]
    expect(Object.hasOwn(stored, 'direction')).toBe(own)
    expect(stored.direction).toBe(value)

    render(<App />)
    chooseDirection('Exit')
    const [sent] = await syncAndCapture()
    expect(Object.hasOwn(sent, 'event_type')).toBe(expected !== undefined)
    expect(sent.event_type).toBe(expected)
    expect(Object.hasOwn(sent, 'local_decision')).toBe(false)
  })

  it('preserves complete mixed-direction rows after failed sync', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('ticket-in')
    chooseDirection('Exit')
    await queueOneScan('ticket-out')
    await readsSettled()
    const before = await storedRows() as OccurrenceRecord[]
    expect(before.map((row) => [row.qrPayload, row.state, row.direction]).sort()).toEqual([
      ['ticket-in', 'QUEUED', 'entry'],
      ['ticket-out', 'QUEUED', 'exit'],
    ])

    stubFetch(() => Promise.reject(new TypeError('network down')))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText(/^Sync failed: No connection\. 2 scans are saved/)
    expect(await storedRows()).toEqual(before)

    stubFetch(() => Promise.resolve(new Response('<html>Bad Gateway</html>', { status: 502 })))
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText(/^Sync failed: The server answered but the reply could not be read\. 2 scans are saved/)
    expect(await storedRows()).toEqual(before)

    chooseDirection('Entry')
    const sent = await syncAndCapture()
    expect(sent.sort((a, b) => String(a.qr_payload).localeCompare(String(b.qr_payload)))).toEqual(
      before
        .map((row) => ({ qr_payload: row.qrPayload, occurrence_id: row.occurrenceId, occurred_at: row.occurredAt, event_type: row.direction }))
        .sort((a, b) => a.qr_payload.localeCompare(b.qr_payload)),
    )
  })

  // D7: in Exit mode a listed ticket mints nothing, stores nothing and sends
  // nothing. Access has no direction on a refusal, so a refusal row would record a
  // refused entry that never happened. The Entry half proves the ticket is listed.
  it('does not record an exit-mode scan of a revoked ticket', async () => {
    const tid = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
    const fetchMock = vi.fn((url: string) => String(url).includes('voided-tickets')
      ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [tid], next_cursor: null }), { status: 200 }))
      : Promise.reject(new TypeError('network down')))
    vi.stubGlobal('fetch', fetchMock)
    const claims = btoa(JSON.stringify({ tid })).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')
    render(<App />)
    await waitFor(() => expect(screen.queryByText(/holds no revocation list yet/i)).toBeNull())

    chooseDirection('Exit')
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: `header.${claims}.signature` } })
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Exit not recorded' })).toBeDefined()
    const result = screen.getByRole('heading', { name: 'Exit not recorded' }).closest('section')!
    expect(Array.from(result.querySelectorAll('p'), (line) => line.textContent)).toEqual([
      "This ticket is on the device's revocation list.",
      'The holder may leave.',
      'This ticket is no longer valid for entry.',
    ])
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
    await readsSettled()
    expect(queueReads.mints).toBe(0)
    expect(queueReads.refusals).toBe(0)
    expect(await storedRows()).toEqual([])
    expect(fetchMock.mock.calls.filter(([url]) => !String(url).includes('voided-tickets'))).toEqual([])
    expect(screen.queryByRole('heading', { name: 'Not valid for entry' })).toBeNull()

    chooseDirection('Entry')
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Not valid for entry' })).toBeDefined()
    await waitFor(() => expect(queueReads.refusals).toBe(1))
    expect(queueReads.mints).toBe(0)
  })
})
