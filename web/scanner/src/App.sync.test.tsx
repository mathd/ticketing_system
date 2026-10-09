import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import App from './App'

// Counts the queue reads the page makes. A failure note reads the queue before it is shown, so a
// test that expects a failure, or none, waits for that read first (see readsSettled). failNext
// makes the next read throw.
const queueReads = vi.hoisted(() => ({ started: 0, finished: 0, failNext: false }))

vi.mock('./occurrences', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./occurrences')>()
  return {
    ...actual,
    async openOccurrenceStore(...args: Parameters<typeof actual.openOccurrenceStore>) {
      const store = await actual.openOccurrenceStore(...args)
      return {
        ...store,
        async queued() {
          queueReads.started += 1
          try {
            if (queueReads.failNext) {
              queueReads.failNext = false
              throw new Error('queue read failed')
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
const offlineNote = /Sync failed: No connection\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./
const unreadableNote = /Sync failed: The server answered but the reply could not be read\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./

beforeEach(() => {
  // A fresh IndexedDB per test: a queued row left by one test would change the next one's count.
  vi.stubGlobal('indexedDB', new IDBFactory())
  queueReads.failNext = false
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

type ReconcileBody = { occurrences: { occurrence_id: string }[] }
type Reconcile = (body: ReconcileBody) => Promise<Response>

// Every fetch the page makes: the revocation feed always answers, a scan is always offline
// (that is how a row gets queued), and the reconcile answer is the test's.
function stubFetch(reconcile: Reconcile) {
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const u = String(url)
    if (u.includes('voided-tickets')) return Promise.resolve(feedResponse())
    if (u.endsWith('/reconciliations')) return reconcile(JSON.parse(init!.body as string))
    return Promise.reject(new TypeError('network down'))
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// A reconcile request the test answers by hand. It waits in `held` until the test calls answer or
// fail on it, so the order of the answers is the test's choice, not the timer's.
type HeldReconcile = { body: ReconcileBody; answer: (response: Response) => void; fail: (error: Error) => void }

function holdReconciles(): HeldReconcile[] {
  const held: HeldReconcile[] = []
  stubFetch((body) => new Promise<Response>((answer, fail) => { held.push({ body, answer, fail }) }))
  return held
}

// A valid reconcile answer that records every occurrence the page sent.
function recordedResponse(body: ReconcileBody) {
  return new Response(JSON.stringify({
    results: body.occurrences.map((o) => ({ occurrence_id: o.occurrence_id, result: 'recorded' })),
  }), { status: 200 })
}

// Lets the page finish a request that is already answered. Only queued work remains after it.
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
}

// Lets the page finish the queue reads that its latest step started, then renders what they changed.
// A failure note reads the queue before it is shown, so a test that expects no note waits here.
async function readsSettled() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
  await waitFor(() => expect(queueReads.finished).toBe(queueReads.started))
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
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
    const note = await screen.findByText(/^Sync failed: No connection\./)
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

  it('a slow failure does not replace a later success', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    const held = holdReconciles()
    // Attempt A (the button) hangs. Attempt B (the online edge) starts while A is still out.
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(2))
    held[1].answer(recordedResponse(held[1].body))
    expect(await screen.findByText('Synced 1 offline scan(s).')).toBeDefined()
    // A fails last, after a success landed while it was out. Its failure must not show.
    // The page reads the queue before it shows a failure, so let that read finish first.
    held[0].fail(new TypeError('network down'))
    await readsSettled()
    expect(screen.getByText('Synced 1 offline scan(s).')).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
  })

  it('a later failure does not stand over an earlier success that has since landed', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    const held = holdReconciles()
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(2))
    // X is still queued, so B's failure shows while A is still out.
    held[1].fail(new TypeError('network down'))
    expect(await screen.findByText(offlineNote)).toBeDefined()
    // A lands last with a success, which replaces B's failure.
    held[0].answer(recordedResponse(held[0].body))
    expect(await screen.findByText('Synced 1 offline scan(s).')).toBeDefined()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
  })

  it('a stalled later attempt does not hide an earlier failure', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    const held = holdReconciles()
    // Attempt A (the button) hangs. Attempt B (the online edge) starts and is never answered.
    // The fetch has no deadline, so B can stall for good.
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(2))
    // A fails while B is still out. X is still queued, so A's failure must show.
    held[0].fail(new TypeError('network down'))
    expect(await screen.findByText(offlineNote)).toBeDefined()
  })

  it('a failure shows the rows still queued after a success covered only part of its batch', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan('ticket-x')
    const held = holdReconciles()
    // Attempt A (the button) sends X and hangs.
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    // Y is queued while A is out. Attempt B (the online edge) sends X and Y, and hangs too.
    await queueOneScan('ticket-y')
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(held).toHaveLength(2))
    // A succeeds for X only, so Y still awaits sync.
    held[0].answer(recordedResponse(held[0].body))
    expect(await screen.findByText('Synced 1 offline scan(s).')).toBeDefined()
    // B fails. Its note counts what is still queued, which is Y alone, not the two rows B sent.
    held[1].fail(new TypeError('network down'))
    expect(await screen.findByText(/^Sync failed: No connection\. 1 scan is saved on this device/)).toBeDefined()
  })

  it('a late failure does not restore a note over a queue that is already empty', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    const held = holdReconciles()
    // Attempt A (the button) sends X and hangs.
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    // Another tab marks X synced. Attempt B (the online edge) finds the queue empty and clears the button.
    await markStoredRowsSynced()
    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull())
    // A fails last. Nothing awaits sync, so its failure must not show.
    held[0].fail(new TypeError('network down'))
    await readsSettled()
    expect(screen.queryByText(/^Sync failed:/)).toBeNull()
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

  it('a failure whose queue read throws keeps its own count and leaves the storage alert alone', async () => {
    render(<App />)
    stubFetch(() => Promise.reject(new TypeError('not yet')))
    await queueOneScan()
    const held = holdReconciles()
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await waitFor(() => expect(held).toHaveLength(1))
    // The queue read that follows the failure throws.
    queueReads.failNext = true
    held[0].fail(new TypeError('network down'))
    await readsSettled()
    expect(await screen.findByText(offlineNote)).toBeDefined()
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
    await new Promise((resolve) => setTimeout(resolve, 0))
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
