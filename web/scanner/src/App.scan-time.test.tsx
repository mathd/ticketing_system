import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory, IDBObjectStore } from 'fake-indexeddb'
import type { OccurrenceStore, OccurrenceStoreOptions, OwnerLiveness } from './occurrences'

// The rule (TKT-399 D27, ADR-025 §3): no scan may be minted or sent 3 seconds
// or more after it started, and no scan may open the gate 30 seconds or more
// after it started. These tests run the production store over a real (fake) IndexedDB.
// The test holds the clocks, and can hold the owner lock and the row recovery
// that the store runs after the database opens. It can also move the clocks
// right after a store step, to stand for a step that took that long.
const clock = vi.hoisted(() => ({ monotonic: 0, wall: 1_760_000_000_000 }))
const hooks = vi.hoisted(() => ({
  beforeHold: undefined as undefined | (() => Promise<void>),
  beforeRecover: undefined as undefined | (() => Promise<void>),
  afterIsRevoked: undefined as undefined | (() => void),
  afterMint: undefined as undefined | (() => void),
  afterMintRefusal: undefined as undefined | (() => void),
  afterActuate: undefined as undefined | (() => void),
  beforeMarkQueued: undefined as undefined | (() => Promise<void>),
}))

vi.mock('./scan-clock', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./scan-clock')>()
  return { ...actual, readScanClock: () => ({ monotonic: clock.monotonic, wall: clock.wall }) }
})

vi.mock('./occurrences', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./occurrences')>()
  const lockName = (ownerId: string) => `ticketing.scanner.occurrence-owner.${ownerId}`
  // The same browser locks as production, with a point where the test can wait.
  const ownerLiveness: OwnerLiveness = {
    async hold(ownerId) {
      await hooks.beforeHold?.()
      let release!: () => void
      await new Promise<void>((acquired) => {
        void navigator.locks.request(lockName(ownerId), () => {
          acquired()
          return new Promise<void>((resolve) => { release = resolve })
        })
      })
      return () => release()
    },
    async runIfOwnerAbsent(ownerId, task) {
      await hooks.beforeRecover?.()
      return navigator.locks.request(lockName(ownerId), { ifAvailable: true }, async (lock) => {
        if (!lock) return false
        await task()
        return true
      })
    },
  }
  return {
    ...actual,
    openOccurrenceStore: async (dbName: string, options: OccurrenceStoreOptions = {}): Promise<OccurrenceStore> => {
      const store = await actual.openOccurrenceStore(dbName, { ...options, ownerLiveness })
      return {
        ...store,
        async isRevoked(ticketID) {
          const listed = await store.isRevoked(ticketID)
          hooks.afterIsRevoked?.()
          return listed
        },
        async mint(qrPayload, occurredAt, direction) {
          const record = await store.mint(qrPayload, occurredAt, direction)
          hooks.afterMint?.()
          return record
        },
        async mintRefusal(qrPayload, occurredAt) {
          const record = await store.mintRefusal(qrPayload, occurredAt)
          hooks.afterMintRefusal?.()
          return record
        },
        async actuate(occurrenceId) {
          const opened = await store.actuate(occurrenceId)
          hooks.afterActuate?.()
          return opened
        },
        async markQueued(occurrenceId) {
          await hooks.beforeMarkQueued?.()
          return store.markQueued(occurrenceId)
        },
      }
    },
  }
})

import App from './App'
import { openOccurrenceStore } from './occurrences'

const DB = 'gate-occurrences'
const scanStoppedText = 'The scanner could not confirm that this scan finished within 3 seconds, so it stopped the scan. No ticket was checked. If another scanner tab is open on this device, close it, then try again.'
const answeredLateText = (direction: 'entry' | 'exit') =>
  `Access accepted this ticket and recorded the ${direction}, but the scanner could not confirm that the scan was still within 30 seconds, so it did not open the gate.`
const unconfirmedLateText = (direction: 'entry' | 'exit') =>
  `The scanner sent this scan, but it could not confirm an answer from Access within 30 seconds, so it did not open the gate. Access may have recorded the ${direction}.`
const offlineAdmitText = /Admit per venue offline policy/

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void; reject: (cause: unknown) => void }
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (cause: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function advance(monotonic: number, wall = monotonic) {
  clock.monotonic += monotonic
  clock.wall += wall
}

const acceptedResponse = () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-16T20:00:01Z' }), { status: 200 })

// Access records every reconciled row, so a test sees each one that was sent.
function stubScans(scan: () => Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    if (String(url).includes('voided-tickets')) {
      return Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
    }
    if (String(url).endsWith('/reconciliations')) {
      const sent = JSON.parse(String(init?.body)) as { occurrences: Array<{ occurrence_id: string }> }
      const results = sent.occurrences.map((row) => ({ occurrence_id: row.occurrence_id, result: 'recorded' }))
      return Promise.resolve(new Response(JSON.stringify({ results }), { status: 200 }))
    }
    return scan()
  }))
}

function scanRequests() {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))
}

function reconcileRequests() {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/reconciliations'))
}

// Every occurrence id that any reconciliation request sent to Access.
function reconciledIds(): string[] {
  return reconcileRequests().flatMap(([, init]) =>
    (JSON.parse(String((init as RequestInit).body)) as { occurrences: Array<{ occurrence_id: string }> })
      .occurrences.map((row) => row.occurrence_id))
}

// A row that another page queued earlier. A sync must send it, so a test that
// asserts what a sync left out also sees that the sync ran.
async function seedQueuedRow(): Promise<string> {
  const seeder = await openOccurrenceStore(DB, { ownerId: 'seeder' })
  const row = await seeder.mint('seeded-ticket', '2026-10-16T19:00:00Z', 'entry')
  await seeder.markQueued(row.occurrenceId)
  seeder.close()
  return row.occurrenceId
}

// Runs one sync, as the online event does, and waits for its request.
async function syncOnce() {
  const before = reconcileRequests().length
  act(() => { window.dispatchEvent(new Event('online')) })
  await waitFor(() => expect(reconcileRequests().length).toBeGreaterThan(before))
}

// The IndexedDB write that closes a row fails once, as a full or broken disk
// does. Only the write of that terminal state fails, so the test can arm it
// before the scan and every other write still succeeds.
const terminalWrite = { failures: 0 }
function failNextTerminalWrite(failedState = 'RESOLVED') {
  terminalWrite.failures = 1
  const put = IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value: unknown, key?: IDBValidKey) {
    const state = (value as { state?: unknown } | null)?.state
    if (this.name === 'occurrences' && state === failedState && terminalWrite.failures > 0) {
      terminalWrite.failures -= 1
      throw new DOMException('the disk is full', 'QuotaExceededError')
    }
    return put.call(this, value, key)
  })
}

function scan(value = 'signed-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
}

async function expectIdleButton() {
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
}

// Every occurrence row, read straight from IndexedDB, not through the app.
async function storedRows(): Promise<Array<Record<string, unknown>>> {
  const db = await new Promise<IDBDatabase>((resolve, reject) => {
    const open = indexedDB.open(DB)
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

async function rowsFor(qrPayload: string) {
  return (await storedRows()).filter((row) => row.qrPayload === qrPayload)
}

// A credential whose claims name this ticket id, so the device can find it on
// its revocation list.
function credentialFor(ticketID: string) {
  const claims = btoa(JSON.stringify({ tid: ticketID })).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `header.${claims}.signature`
}

// Holds the owner lock that the store takes after the database opens, starts a
// scan while it waits, moves the clocks, and lets it go.
async function scanWhileOwnerLockWaits(move: () => void, value = 'signed-ticket') {
  const ownerLock = deferred<void>()
  hooks.beforeHold = () => ownerLock.promise
  render(<App />)
  scan(value)
  expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined()
  move()
  ownerLock.resolve()
}

beforeEach(() => {
  vi.stubGlobal('indexedDB', new IDBFactory())
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
  stubScans(() => Promise.resolve(acceptedResponse()))
})

afterEach(() => {
  cleanup()
  hooks.beforeHold = undefined
  hooks.beforeRecover = undefined
  hooks.afterIsRevoked = undefined
  hooks.afterMint = undefined
  hooks.afterMintRefusal = undefined
  hooks.afterActuate = undefined
  hooks.beforeMarkQueued = undefined
  terminalWrite.failures = 0
  clock.monotonic = 0
  clock.wall = 1_760_000_000_000
  localStorage.clear()
  sessionStorage.clear()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the 3 second limit to mint and send a scan (TKT-399 D27)', () => {
  it('refuses a scan whose store waited for its owner lock until 3000 ms had passed, and mints, sends and opens nothing', async () => {
    await scanWhileOwnerLockWaits(() => advance(3_000))

    expect((await screen.findByRole('heading', { name: 'Scan stopped' })).parentElement?.textContent).toBe(`Scan stopped${scanStoppedText}`)
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(scanRequests()).toHaveLength(0)
    expect(await storedRows()).toEqual([])

    // The store itself is fine. The next scan starts its own 3 seconds.
    scan()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
  })

  it('refuses a scan whose store waited for row recovery until 3000 ms had passed', async () => {
    // A row left PENDING by a page that has gone. The store opened by the app
    // checks that page's owner lock while it recovers rows.
    const crashedPage = await openOccurrenceStore(DB, { ownerId: 'crashed-page' })
    const crashed = await crashedPage.mint('crashed-ticket', '2026-10-16T19:59:00Z', 'entry')
    crashedPage.close()
    const recovery = deferred<void>()
    hooks.beforeRecover = () => recovery.promise

    render(<App />)
    scan()
    expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined()
    advance(3_000)
    recovery.resolve()

    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    await expectIdleButton()
    expect(scanRequests()).toHaveLength(0)
    expect(await rowsFor('signed-ticket')).toEqual([])
    expect((await storedRows()).map((row) => row.occurrenceId)).toEqual([crashed.occurrenceId])
  })

  // Nothing was sent, so the row is closed and never reconciled: a reconciliation
  // would record an entry the operator was told did not happen.
  it('does not send a scan whose mint took until 3000 ms', async () => {
    hooks.afterMint = () => advance(3_000)
    render(<App />)
    scan()

    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    await expectIdleButton()
    expect(scanRequests()).toHaveLength(0)
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_not_sent',
    })])
    expect(screen.queryByText(/queued offline scan/)).toBeNull()
  })

  it('sends a scan whose mint took until 2999 ms', async () => {
    hooks.afterMint = () => advance(2_999)
    render(<App />)
    scan()

    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(scanRequests()).toHaveLength(1)
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED', actuated: true })])
  })

  it('in Exit mode, says that the exit was not recorded and that the holder may leave', async () => {
    hooks.afterMint = () => advance(3_000)
    render(<App />)
    fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
    scan()

    const heading = await screen.findByRole('heading', { name: 'Exit not recorded' })
    expect(heading.parentElement?.textContent).toBe(`Exit not recorded${scanStoppedText}The holder may leave.`)
    expect(heading.parentElement?.getAttribute('role')).toBe('status')
    expect(scanRequests()).toHaveLength(0)
  })

  it('does not record a local refusal for a revoked ticket after 3000 ms', async () => {
    const ticketID = '0b5d6a8e-1c2f-4e3a-9b7d-2f6c8e1a4b3c'
    const seeder = await openOccurrenceStore(DB, { ownerId: 'seeder' })
    await seeder.mergeRevoked([ticketID])
    seeder.close()
    hooks.afterIsRevoked = () => advance(3_000)

    render(<App />)
    scan(credentialFor(ticketID))

    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Not valid for entry' })).toBeNull()
    expect(await storedRows()).toEqual([])
    expect(scanRequests()).toHaveLength(0)

    // In time, the same scan records its refusal.
    hooks.afterIsRevoked = undefined
    scan(credentialFor(ticketID))
    expect(await screen.findByRole('heading', { name: 'Not valid for entry' })).toBeDefined()
    expect(await storedRows()).toEqual([expect.objectContaining({ localDecision: 'revocation_refused' })])
  })

  // The literal limit from the requirement, on each clock.
  it.each([
    { monotonic: 2_999, wall: 2_999, refused: false },
    { monotonic: 3_000, wall: 3_000, refused: true },
    { monotonic: 3_000, wall: 2_999, refused: true },
    { monotonic: 2_999, wall: 3_000, refused: true },
  ])('a wait of $monotonic ms monotonic and $wall ms wall: refused=$refused', async ({ monotonic, wall, refused }) => {
    await scanWhileOwnerLockWaits(() => advance(monotonic, wall))
    if (refused) {
      expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
      await expectIdleButton()
      expect(scanRequests()).toHaveLength(0)
      expect(await storedRows()).toEqual([])
    } else {
      expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
      expect(scanRequests()).toHaveLength(1)
      expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED' })])
    }
  })

  // Some browsers stop performance.now() while the device sleeps. Here it stopped
  // for 500 ms of the wait, so the two clocks still agree within 1000 ms, and
  // only the wall clock shows that 3000 ms passed.
  it('refuses a scan when the monotonic clock stopped for part of a 3000 ms wait', async () => {
    await scanWhileOwnerLockWaits(() => advance(2_500, 3_000))
    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    await expectIdleButton()
    expect(scanRequests()).toHaveLength(0)
    expect(await storedRows()).toEqual([])
  })

  it.each([
    { name: 'forward by 1001 ms', monotonic: 100, wall: 1_101, refused: true },
    { name: 'back by 1001 ms', monotonic: 100, wall: -901, refused: true },
    { name: 'forward by 1000 ms', monotonic: 100, wall: 1_100, refused: false },
    { name: 'back by 1000 ms', monotonic: 100, wall: -900, refused: false },
  ])('a wall clock that jumped $name during the scan: refused=$refused', async ({ monotonic, wall, refused }) => {
    await scanWhileOwnerLockWaits(() => advance(monotonic, wall))
    if (refused) {
      expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
      expect(scanRequests()).toHaveLength(0)
    } else {
      expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
      expect(scanRequests()).toHaveLength(1)
    }
  })

  it.each([
    { name: 'NaN', value: Number.NaN },
    { name: 'Infinity', value: Number.POSITIVE_INFINITY },
  ])('refuses a scan when the wall clock gives $name', async ({ value }) => {
    await scanWhileOwnerLockWaits(() => { clock.wall = value })
    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    await expectIdleButton()
    expect(scanRequests()).toHaveLength(0)
    expect(await storedRows()).toEqual([])
  })
})

// The holder is still at the gate while the answer travels, so a scan sent in
// time may open the gate until 30 seconds after it started (owner decision,
// 2026-10-16). A shorter limit here left the gate shut for a slow answer that
// Access had already recorded, and the next scan then read "already used".
describe('the 30 second limit to open the gate (TKT-399 D27)', () => {
  // Sends the scan in time, then moves the clocks before the answer arrives.
  async function answerAfter(monotonic: number, wall = monotonic) {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(monotonic, wall)
    reply.resolve(acceptedResponse())
  }

  async function expectGateOpened() {
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Gate not opened' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED', actuated: true })])
  }

  async function expectGateNotOpenedBeforeMark() {
    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_accepted',
    })])
  }

  it.each([3_000, 15_000, 29_999])('opens the gate when the answer arrives after %i ms', async (elapsed) => {
    await answerAfter(elapsed)
    await expectGateOpened()
  })

  // The answer comes late. Access has recorded the scan by then, so the screen
  // says so, and the gate does not open. The row is closed and never sent again.
  it('does not open the gate when the answer arrives after 30 000 ms', async () => {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))

    advance(30_000)
    reply.resolve(acceptedResponse())

    const heading = await screen.findByRole('heading', { name: 'Gate not opened' })
    expect(heading.parentElement?.textContent).toBe(`Gate not opened${answeredLateText('entry')}A new scan of this ticket may be refused as already used.`)
    expect(heading.parentElement?.getAttribute('role')).toBe('alert')
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_accepted', direction: 'entry',
    })])
    expect(screen.queryByText(/queued offline scan/)).toBeNull()
  })

  it('in Exit mode, says that Access recorded the exit and that the holder may leave', async () => {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))

    advance(30_000)
    reply.resolve(acceptedResponse())

    const heading = await screen.findByRole('heading', { name: 'Gate not opened' })
    expect(heading.parentElement?.textContent).toBe(`Gate not opened${answeredLateText('exit')}The holder may leave.`)
    expect(heading.parentElement?.getAttribute('role')).toBe('status')
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_accepted', direction: 'exit',
    })])
  })

  // The gate opens when the screen shows the result. The row is marked first
  // (ADR-025 §3), so a late mark leaves it ACTUATED with the gate not opened:
  // the same stranded admission as a crash between marking and opening.
  it('does not open the gate when marking the row took until 30 000 ms', async () => {
    hooks.afterActuate = () => advance(30_000)
    render(<App />)
    scan()

    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED', actuated: true })])
  })

  it('opens the gate when marking the row took until 29 999 ms', async () => {
    hooks.afterActuate = () => advance(29_999)
    render(<App />)
    scan()
    await expectGateOpened()
  })

  // The 30 seconds count from the scan start, not from the answer.
  it('does not open the gate when the answer came after 15 000 ms and marking the row took 15 000 ms more', async () => {
    hooks.afterActuate = () => advance(15_000)
    await answerAfter(15_000)

    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    await expectIdleButton()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED', actuated: true })])
  })

  // The literal limit from the requirement, on each clock.
  it.each([
    { monotonic: 30_000, wall: 29_999 },
    { monotonic: 29_999, wall: 30_000 },
  ])('does not open the gate for an answer after $monotonic ms monotonic and $wall ms wall', async ({ monotonic, wall }) => {
    await answerAfter(monotonic, wall)
    await expectGateNotOpenedBeforeMark()
  })

  // Every other arm of the check is the same as before the send, so each one
  // still refuses well below 30 000 ms.
  it.each([
    { name: 'the wall clock jumped forward by 1001 ms', monotonic: 15_000, wall: 16_001, opened: false },
    { name: 'the wall clock jumped back by 1001 ms', monotonic: 15_000, wall: 13_999, opened: false },
    { name: 'the wall clock jumped forward by 1000 ms', monotonic: 15_000, wall: 16_000, opened: true },
    { name: 'the wall clock jumped back by 1000 ms', monotonic: 15_000, wall: 14_000, opened: true },
    { name: 'the monotonic clock stopped for the whole wait', monotonic: 0, wall: 15_000, opened: false },
    { name: 'the monotonic clock stopped for 500 ms of a 30 000 ms wait', monotonic: 29_500, wall: 30_000, opened: false },
    { name: 'the monotonic clock went back', monotonic: -1, wall: -1, opened: false },
  ])('an answer after $name: gate opened=$opened', async ({ monotonic, wall, opened }) => {
    await answerAfter(monotonic, wall)
    if (opened) await expectGateOpened()
    else await expectGateNotOpenedBeforeMark()
  })

  it.each([
    { name: 'NaN', value: Number.NaN },
    { name: 'Infinity', value: Number.POSITIVE_INFINITY },
  ])('does not open the gate when the wall clock gives $name before the answer', async ({ value }) => {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    clock.wall = value
    reply.resolve(acceptedResponse())
    await expectGateNotOpenedBeforeMark()
  })
})

describe('the scan limits on the camera path (TKT-399 D27)', () => {
  function stubCameraWith(detection: Promise<Array<{ rawValue?: string }>>) {
    vi.stubGlobal('BarcodeDetector', class {
      detect = vi.fn().mockReturnValueOnce(detection).mockResolvedValue([])
    })
    vi.stubGlobal('navigator', {
      locks: navigator.locks,
      mediaDevices: { getUserMedia: vi.fn().mockResolvedValue({ getTracks: () => [{ stop: vi.fn() }] }) },
    })
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
  }

  // A camera scan starts when the code is detected, not when the camera started.
  it('accepts a code detected long after the camera started', async () => {
    const detection = deferred<Array<{ rawValue?: string }>>()
    stubCameraWith(detection.promise)
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    expect(await screen.findByText('Point the camera at a ticket QR code.')).toBeDefined()

    advance(10_000)
    await act(async () => {
      detection.resolve([{ rawValue: 'camera-ticket' }])
      await detection.promise
    })

    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(await rowsFor('camera-ticket')).toEqual([expect.objectContaining({ state: 'ACTUATED' })])
  })

  it('does not open the gate when the answer to a camera scan arrives 30 000 ms after the detection', async () => {
    const detection = deferred<Array<{ rawValue?: string }>>()
    stubCameraWith(detection.promise)
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    expect(await screen.findByText('Point the camera at a ticket QR code.')).toBeDefined()

    advance(10_000)
    await act(async () => {
      detection.resolve([{ rawValue: 'camera-ticket' }])
      await detection.promise
    })
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(30_000)
    reply.resolve(acceptedResponse())

    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(await rowsFor('camera-ticket')).toEqual([expect.objectContaining({ state: 'RESOLVED', actuated: false })])
  })
})

// The owner's decision D30 (2026-10-17): a scan that the time limit stopped is
// never synced to Access, and the scanner never tells staff to admit after the
// limit. Each test has a row from another page in the queue, so its sync does
// send a request, and the test sees what that request left out.
describe('a scan that the time limit stopped is never synced (TKT-399 D30)', () => {
  it('never reconciles a scan stopped after its mint, when the write that closes its row fails', async () => {
    const seeded = await seedQueuedRow()
    hooks.afterMint = () => {
      advance(3_000)
      failNextTerminalWrite()
    }
    render(<App />)
    scan()

    expect(await screen.findByRole('heading', { name: 'Scan stopped' })).toBeDefined()
    expect(await screen.findByText(/cannot save scans right now/)).toBeDefined()
    await expectIdleButton()
    expect(scanRequests()).toHaveLength(0)

    await syncOnce()
    expect(reconciledIds()).toEqual([seeded])
    // The sync closed the row on its next try.
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_not_sent',
    })])
  })

  it('never reconciles a scan that Access accepted too late, when the write that closes its row fails', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(30_000)
    failNextTerminalWrite()
    reply.resolve(acceptedResponse())

    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    expect(await screen.findByText(/could not save the server result/)).toBeDefined()
    await expectIdleButton()

    await syncOnce()
    expect(reconciledIds()).toEqual([seeded])
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_accepted',
    })])
  })

  // The failure came at the limit, so the scanner may not tell staff to admit.
  // Access may have the scan from the live request; nothing else sends it.
  it('shows no offline admission, and closes the row, when the scan fails 30 000 ms after it started', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(30_000)
    reply.reject(new TypeError('Failed to fetch'))

    const heading = await screen.findByRole('heading', { name: 'Gate not opened' })
    expect(heading.parentElement?.textContent).toBe(`Gate not opened${unconfirmedLateText('entry')}A new scan of this ticket may be refused as already used.`)
    expect(heading.parentElement?.getAttribute('role')).toBe('alert')
    await expectIdleButton()
    expect(screen.queryByText(offlineAdmitText)).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Queued offline' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_unconfirmed',
    })])

    await syncOnce()
    expect(reconciledIds()).toEqual([seeded])
  })

  it('never reconciles a scan that failed 30 000 ms after it started, when the write that closes its row fails', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(30_000)
    failNextTerminalWrite()
    reply.reject(new TypeError('Failed to fetch'))

    expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
    expect((await screen.findByText(/could not save the result of this scan/)).textContent)
      .toBe('This device could not save the result of this scan. Try again after restoring browser storage.')
    await expectIdleButton()
    expect(screen.queryByText(offlineAdmitText)).toBeNull()

    await syncOnce()
    expect(reconciledIds()).toEqual([seeded])
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_unconfirmed',
    })])
  })

  it('shows the offline result, and queues the row, when the scan fails 29 999 ms after it started', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(29_999)
    reply.reject(new TypeError('Failed to fetch'))

    expect(await screen.findByText(offlineAdmitText)).toBeDefined()
    await expectIdleButton()
    const [row] = await rowsFor('signed-ticket')
    expect(row).toEqual(expect.objectContaining({ state: 'QUEUED', actuated: false }))

    await syncOnce()
    expect(reconciledIds().sort()).toEqual([seeded, row.occurrenceId].sort())
  })

  // The result is decided at a checked moment and shown before the queue write.
  // The write only records it, so its time cannot make the admission late.
  it('shows the offline result before the queue write, so a queue write that ends at 30 000 ms cannot decide it late', async () => {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    const queueWrite = deferred<void>()
    hooks.beforeMarkQueued = () => queueWrite.promise
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(29_999)
    reply.reject(new TypeError('Failed to fetch'))

    // The queue write still waits.
    expect(await screen.findByText(offlineAdmitText)).toBeDefined()
    expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined()
    advance(1)
    queueWrite.resolve()

    await expectIdleButton()
    expect(screen.getByText(offlineAdmitText)).toBeDefined()
    expect(screen.queryByRole('heading', { name: 'Gate not opened' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({ state: 'QUEUED' })])
  })

  // An exit is never refused (D7), so the holder may leave. The scan is still
  // stopped like an entry and never synced: D30 does not depend on the direction.
  it('in Exit mode, closes a scan that fails 30 000 ms after it started, and says that the holder may leave', async () => {
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(30_000)
    reply.reject(new TypeError('Failed to fetch'))

    const heading = await screen.findByRole('heading', { name: 'Gate not opened' })
    expect(heading.parentElement?.textContent).toBe(`Gate not opened${unconfirmedLateText('exit')}The holder may leave.`)
    expect(heading.parentElement?.getAttribute('role')).toBe('status')
    expect(screen.queryByRole('heading', { name: 'Queued offline' })).toBeNull()
    expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
      state: 'RESOLVED', actuated: false, result: 'late_unconfirmed', direction: 'exit',
    })])
  })

  // A6-1 (decision D35): another storage failure replaces the store while a scan
  // waits for its answer. The page lives, so neither this page nor another tab
  // may queue the row of that scan before it ends. The scan then ends late, and
  // no sync sends it.
  it('never reconciles a scan whose store was replaced while it waited, and that then ended late', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    const [inFlight] = await rowsFor('signed-ticket')
    expect(inFlight).toEqual(expect.objectContaining({ state: 'PENDING' }))

    // The next listing of the queue fails, so the sync replaces the store.
    const getAll = IDBObjectStore.prototype.getAll
    let listingFailures = 1
    vi.spyOn(IDBObjectStore.prototype, 'getAll').mockImplementation(function (this: IDBObjectStore, ...args: Parameters<IDBObjectStore['getAll']>) {
      if (this.name === 'occurrences' && listingFailures > 0) {
        listingFailures -= 1
        throw new DOMException('the disk is broken', 'UnknownError')
      }
      return getAll.apply(this, args)
    })
    act(() => { window.dispatchEvent(new Event('online')) })
    expect(await screen.findByText(/cannot save scans right now/)).toBeDefined()
    expect(listingFailures).toBe(0)

    // Another tab opens after the lease of the row expired.
    const otherTab = await openOccurrenceStore(DB, { ownerId: 'other-tab', now: () => Date.now() + 120_000 })
    try {
      expect((await otherTab.queued()).map((row) => row.occurrenceId)).toEqual([seeded])
      // This page syncs from its next store, which recovers the rows of the owner it replaced.
      await syncOnce()
      expect(reconciledIds()).toEqual([seeded])

      // The scan ends late. Its store is closed, so the close write fails.
      advance(30_000)
      reply.reject(new TypeError('Failed to fetch'))
      expect(await screen.findByRole('heading', { name: 'Gate not opened' })).toBeDefined()
      await expectIdleButton()
      expect(screen.queryByText(offlineAdmitText)).toBeNull()
      expect((await otherTab.queued()).map((row) => row.occurrenceId)).toEqual([])
      expect(reconciledIds()).not.toContain(inFlight.occurrenceId)

      // The next listing of this page closes the row, and nothing sends it.
      act(() => { window.dispatchEvent(new Event('online')) })
      await waitFor(async () => expect(await rowsFor('signed-ticket')).toEqual([expect.objectContaining({
        state: 'RESOLVED', actuated: false, result: 'late_unconfirmed',
      })]))
      expect(reconciledIds()).toEqual([seeded])
      expect((await otherTab.queued()).map((row) => row.occurrenceId)).toEqual([])
    } finally {
      otherTab.close()
    }
  })

  // D35: an outcome decided in time stands when its write fails. The page lets
  // the row go, so its next store recovers the row and a sync sends it, while
  // the page lives.
  it('reconciles a scan decided offline in time whose queue write failed, from the next store of this page', async () => {
    const seeded = await seedQueuedRow()
    const reply = deferred<Response>()
    stubScans(() => reply.promise)
    render(<App />)
    scan()
    await waitFor(() => expect(scanRequests()).toHaveLength(1))
    advance(29_999)
    failNextTerminalWrite('QUEUED')
    reply.reject(new TypeError('Failed to fetch'))

    expect(await screen.findByText(offlineAdmitText)).toBeDefined()
    expect(await screen.findByText(/cannot save scans right now/)).toBeDefined()
    await expectIdleButton()
    expect(terminalWrite.failures).toBe(0)
    const [row] = await rowsFor('signed-ticket')
    expect(row).toEqual(expect.objectContaining({ state: 'PENDING', actuated: false }))

    await syncOnce()
    expect(reconciledIds().sort()).toEqual([seeded, row.occurrenceId].sort())
  })

  it('reconciles a rejected scan whose terminal write failed, from the next store of this page', async () => {
    const seeded = await seedQueuedRow()
    stubScans(() => Promise.resolve(new Response(JSON.stringify({
      decision: 'rejected', reason: 'already_redeemed', original_scan_at: '2026-10-16T19:59:00Z',
    }), { status: 409 })))
    failNextTerminalWrite()
    render(<App />)
    scan()

    expect(await screen.findByRole('heading', { name: 'Rejected' })).toBeDefined()
    expect(await screen.findByText(/could not save the server result/)).toBeDefined()
    await expectIdleButton()
    expect(terminalWrite.failures).toBe(0)
    const [row] = await rowsFor('signed-ticket')
    expect(row).toEqual(expect.objectContaining({ state: 'PENDING', actuated: false }))

    await syncOnce()
    expect(reconciledIds().sort()).toEqual([seeded, row.occurrenceId].sort())
  })

  it('reconciles a local refusal decided in time whose queue write failed, from the next store of this page', async () => {
    const seeded = await seedQueuedRow()
    const ticketID = '0b5d6a8e-1c2f-4e3a-9b7d-2f6c8e1a4b3c'
    const seeder = await openOccurrenceStore(DB, { ownerId: 'revocation-seeder' })
    await seeder.mergeRevoked([ticketID])
    seeder.close()
    failNextTerminalWrite('QUEUED')

    render(<App />)
    scan(credentialFor(ticketID))

    expect(await screen.findByRole('heading', { name: 'Not valid for entry' })).toBeDefined()
    expect(await screen.findByText(/could not save the refusal/)).toBeDefined()
    await expectIdleButton()
    expect(terminalWrite.failures).toBe(0)
    const [refusal] = (await storedRows()).filter((row) => row.localDecision === 'revocation_refused')
    expect(refusal).toEqual(expect.objectContaining({ state: 'PENDING' }))

    await syncOnce()
    expect(reconciledIds().sort()).toEqual([seeded, refusal.occurrenceId as string].sort())
  })

  // A revoked ticket must never read as admissible, so the screen still refuses.
  it('never syncs a local refusal whose mint took until 3000 ms, and still says not valid for entry', async () => {
    const seeded = await seedQueuedRow()
    const ticketID = '0b5d6a8e-1c2f-4e3a-9b7d-2f6c8e1a4b3c'
    const seeder = await openOccurrenceStore(DB, { ownerId: 'revocation-seeder' })
    await seeder.mergeRevoked([ticketID])
    seeder.close()
    hooks.afterMintRefusal = () => advance(3_000)

    render(<App />)
    scan(credentialFor(ticketID))

    const heading = await screen.findByRole('heading', { name: 'Not valid for entry' })
    expect(heading.parentElement?.getAttribute('role')).toBe('alert')
    await expectIdleButton()
    const refusals = (await storedRows()).filter((row) => row.localDecision === 'revocation_refused')
    expect(refusals).toEqual([expect.objectContaining({ state: 'RESOLVED', result: 'late_not_sent' })])

    await syncOnce()
    expect(reconciledIds()).toEqual([seeded])
  })
})
