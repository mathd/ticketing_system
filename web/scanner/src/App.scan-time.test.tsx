import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
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
  afterActuate: undefined as undefined | (() => void),
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
        async actuate(occurrenceId) {
          const opened = await store.actuate(occurrenceId)
          hooks.afterActuate?.()
          return opened
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

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void }
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => { resolve = res })
  return { promise, resolve }
}

function advance(monotonic: number, wall = monotonic) {
  clock.monotonic += monotonic
  clock.wall += wall
}

const acceptedResponse = () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-16T20:00:01Z' }), { status: 200 })

function stubScans(scan: () => Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn((url: string) => String(url).includes('voided-tickets')
    ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
    : scan()))
}

function scanRequests() {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))
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
  hooks.afterActuate = undefined
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
