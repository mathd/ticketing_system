import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import App from './App'

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

type Reconcile = (body: { occurrences: { occurrence_id: string }[] }) => Promise<Response>

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

const syncStatus = () => screen.getByText(/^Sync failed:|^Synced /)

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

  it('keeps a standing failure note when a later sync finds the queue empty', async () => {
    await failSyncWith(() => Promise.reject(new TypeError('network down')), offlineNote)
    // Mark the queued row synced behind the app's back, then trigger a sync: the queue read
    // is empty, nothing is sent, and nothing has succeeded — the note must not be cleared.
    const fetchMock = stubFetch(() => Promise.reject(new TypeError('must not be called')))
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
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    // An empty-queue sync makes no request, so there is no response to wait for: let its
    // IndexedDB read and the continuation after it run (fake-indexeddb completes in
    // microseconds), then check. Mutation-checked: clearing the note on the empty return
    // turns this red.
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/reconciliations'))).toBe(false)
    expect(syncStatus().textContent).toMatch(offlineNote)
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
  })
})
