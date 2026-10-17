import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'

// These tests run the production open path over a real (fake) IndexedDB. Only
// the time limit of each open is in the test's hands: the test fires the timer
// and moves the clock.
const openTimers = vi.hoisted(() => [] as Array<{ fire: () => void }>)
const openClock = vi.hoisted(() => ({ now: 0 }))

vi.mock('./occurrences', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./occurrences')>()
  return {
    ...actual,
    openOccurrenceStore: (dbName: string, options: import('./occurrences').OccurrenceStoreOptions = {}) =>
      actual.openOccurrenceStore(dbName, {
        ...options,
        openTimer: (onTimeout) => {
          let stopped = false
          openTimers.push({ fire: () => { if (!stopped) onTimeout() } })
          return () => { stopped = true }
        },
        openClock: () => openClock.now,
      }),
  }
})

import App from './App'

const DB = 'gate-occurrences'
const blockedText = 'Another scanner tab is open on this device. Close the other scanner tabs, then try again. No ticket was checked.'
const timeoutText = 'Scan storage did not open in time. If another scanner tab is open on this device, close it, then try again. No ticket was checked.'

const connections: IDBDatabase[] = []

function rawOpen(version: number, upgrade?: (db: IDBDatabase) => void): Promise<IDBDatabase> {
  const open = indexedDB.open(DB, version)
  if (upgrade) open.onupgradeneeded = () => upgrade(open.result)
  return new Promise((resolve, reject) => {
    open.onsuccess = () => {
      connections.push(open.result)
      resolve(open.result)
    }
    open.onerror = () => reject(open.error)
  })
}

// A tab on the pre-TKT-399 code: version 2, and no versionchange handler.
function openOlderTab(): Promise<IDBDatabase> {
  return rawOpen(2, (db) => {
    db.createObjectStore('occurrences', { keyPath: 'occurrenceId' })
    db.createObjectStore('revocations', { keyPath: 'ticketID' })
    db.createObjectStore('revocation_meta', { keyPath: 'key' })
  })
}

// Waits for a promise during a fixed number of task turns. A promise that does
// not settle in that time fails the test with its name, instead of hanging it.
async function within<T>(what: string, promise: Promise<T>): Promise<T> {
  let settled = false
  promise.then(() => { settled = true }, () => { settled = true })
  const { setImmediate } = await vi.importActual<{ setImmediate(callback: () => void): void }>('node:timers')
  for (let i = 0; i < 500 && !settled; i++) await new Promise<void>((resolve) => setImmediate(resolve))
  if (!settled) throw new Error(`${what} did not settle`)
  return promise
}

// Another new tab whose version 3 open is reported blocked. It waits first in
// the queue, so this page's open waits behind it and gets no event.
async function openWaitingNewTab(): Promise<void> {
  const open = indexedDB.open(DB, 3)
  open.onsuccess = () => open.result.close()
  await within('the blocked event of the other new tab', new Promise<void>((resolve) => { open.onblocked = () => resolve() }))
}

// Opens after every request queued before it has finished, and reads the rows.
async function rowsOnceQueueDrains(): Promise<Array<Record<string, unknown>>> {
  return within('the open after the queue drains', (async () => {
    const db = await rawOpen(3)
    const request = db.transaction('occurrences', 'readonly').objectStore('occurrences').getAll()
    const rows = await new Promise<Array<Record<string, unknown>>>((resolve, reject) => {
      request.onsuccess = () => resolve(request.result)
      request.onerror = () => reject(request.error)
    })
    db.close()
    return rows
  })())
}

function enterTicket() {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'signed-ticket' } })
}

function scanRequests() {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))
}

async function expectIdleButton() {
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
}

beforeEach(() => {
  vi.stubGlobal('indexedDB', new IDBFactory())
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
  vi.stubGlobal('fetch', vi.fn((url: string) => Promise.resolve(String(url).includes('voided-tickets')
    ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
    : new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-16T20:00:01Z' }), { status: 200 }))))
})

afterEach(() => {
  cleanup()
  for (const db of connections.splice(0)) db.close()
  openTimers.splice(0)
  openClock.now = 0
  localStorage.clear()
  sessionStorage.clear()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('Scanner storage open (TKT-399 version 3 barrier)', () => {
  it('tells the operator to close the other scanner tab when the open is blocked, and checks nothing', async () => {
    const olderTab = await openOlderTab()

    render(<App />)
    expect((await screen.findByRole('alert')).textContent).toBe(blockedText)
    enterTicket()
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    await expectIdleButton()
    expect(screen.getByRole('alert').textContent).toBe(blockedText)
    expect(fetch).not.toHaveBeenCalled()

    // The older tab closes. The open that was blocked completes and closes its
    // connection; it neither checks a ticket nor writes a row.
    olderTab.close()
    expect(await rowsOnceQueueDrains()).toEqual([])
    expect(fetch).not.toHaveBeenCalled()

    // The operator scans again: the open succeeds and the scan goes through.
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('refuses a scan whose open waits behind another tab, and never sends it when the open succeeds later', async () => {
    const olderTab = await openOlderTab()
    await openWaitingNewTab()

    render(<App />)
    await waitFor(() => expect(openTimers).toHaveLength(1))
    enterTicket()
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined()
    expect(screen.queryByRole('alert')).toBeNull()

    // The open runs out of time: the waiting scan fails closed.
    openTimers[0].fire()
    expect((await screen.findByRole('alert')).textContent).toBe(timeoutText)
    await expectIdleButton()
    expect(screen.queryByRole('heading', { level: 2 })).toBeNull()

    // A second click while that request still waits refuses at once.
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    await expectIdleButton()
    expect(screen.getByRole('alert').textContent).toBe(timeoutText)
    expect(fetch).not.toHaveBeenCalled()

    // The older tab closes and the waiting request succeeds. Nothing is minted
    // and nothing is sent, with no new click.
    olderTab.close()
    expect(await rowsOnceQueueDrains()).toEqual([])
    expect(fetch).not.toHaveBeenCalled()
    expect(screen.queryByRole('heading', { level: 2 })).toBeNull()
    expect(screen.getByRole('alert').textContent).toBe(timeoutText)

    // The next click opens storage again and the scan goes through.
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  // A browser may run the timer callback late. The limit is decided by elapsed
  // time, so an open that succeeds 3 seconds after it started is refused even
  // though its timer has not fired. Here the timer never fires.
  it('refuses a scan whose open succeeds after the time limit, before the timer fires', async () => {
    const olderTab = await openOlderTab()
    await openWaitingNewTab()

    render(<App />)
    await waitFor(() => expect(openTimers).toHaveLength(1))
    fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
    enterTicket()
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined()

    openClock.now += 3_000
    olderTab.close()
    expect((await screen.findByRole('alert')).textContent).toBe(timeoutText)
    await expectIdleButton()
    expect(screen.queryByRole('heading', { level: 2 })).toBeNull()
    expect(fetch).not.toHaveBeenCalled()
    // Nothing was minted, so nothing was actuated.
    expect(await rowsOnceQueueDrains()).toEqual([])
    expect(fetch).not.toHaveBeenCalled()

    // The late request is finished. The next click opens storage again.
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
  })

  // The revocation pull swallows its own failure. Its failed open must not stay
  // cached, or the next scan would get that old failure instead of a new open.
  it('opens storage again on the next scan after a background pull failed to open it', async () => {
    const realSetInterval = window.setInterval.bind(window)
    let pullTick: (() => void) | undefined
    vi.spyOn(window, 'setInterval').mockImplementation(((handler: () => void, timeout?: number) => {
      if (timeout === 15 * 60 * 1000) {
        pullTick = handler
        return 0
      }
      return realSetInterval(handler, timeout)
    }) as typeof window.setInterval)
    const olderTab = await openOlderTab()

    render(<App />)
    expect((await screen.findByRole('alert')).textContent).toBe(blockedText)
    expect(pullTick).toBeDefined()
    pullTick?.()
    olderTab.close()
    expect(await rowsOnceQueueDrains()).toEqual([])

    enterTicket()
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
  })
})
