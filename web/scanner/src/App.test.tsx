import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'

// Every scan and reconciliation needs an enrolled device. The
// suites below are about SCAN behaviour, so they pair once here — otherwise each
// of them would be re-asserting the pairing screen and its own subject not at
// all. The pairing screen has its own tests, in App.test.tsx.
beforeEach(() => {
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

function pasteCredential(value = 'signed-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
}

function routedFetch(scan: () => Promise<Response>) {
  return vi.fn((url: string, _init?: RequestInit) => String(url).includes('voided-tickets')
    ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
    : scan())
}

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void; reject: (reason?: unknown) => void }
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

// A fake MediaStream whose single track exposes a stop spy.
function fakeStream() {
  const stop = vi.fn()
  return { stop, stream: { getTracks: () => [{ stop }] } as unknown as MediaStream }
}

// Stub a BarcodeDetector that never finds a code, so the detect loop stays pending harmlessly.
function stubIdleDetector() {
  vi.stubGlobal('BarcodeDetector', class {
    detect = vi.fn().mockResolvedValue([])
  })
}

function stubCamera(getUserMedia: () => Promise<MediaStream>) {
  vi.stubGlobal('navigator', {
    locks: navigator.locks,
    mediaDevices: { getUserMedia },
  })
}

describe('App', () => {
  it('renders the scanner and accepts a pasted credential', async () => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 }))))
    render(<App />)
    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
  })

  it('shows the original scan time for a duplicate', async () => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(new Response(JSON.stringify({ decision: 'rejected', reason: 'already_redeemed', original_scan_at: '2026-07-13T12:00:00Z' }), { status: 409 }))))
    render(<App />)
    pasteCredential()
    expect(await screen.findByText(/Already redeemed at/)).toBeDefined()
  })

  it('rejects an invalid credential', async () => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(new Response(JSON.stringify({ decision: 'rejected', reason: 'invalid_credential' }), { status: 422 }))))
    render(<App />)
    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Rejected' })).toBeDefined()
  })

  it('keeps the paste path available when camera detection is unavailable', async () => {
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(screen.getByText(/Paste the credential instead/)).toBeDefined())
    expect(screen.getByLabelText('Ticket credential')).toBeDefined()
  })

  it('stops a failed camera loop and restores the paste path', async () => {
    const stop = vi.fn()
    const detect = vi.fn().mockRejectedValue(new Error('detector failed'))
    const getUserMedia = vi.fn().mockResolvedValue({ getTracks: () => [{ stop }] })
    vi.stubGlobal('BarcodeDetector', class {
      detect = detect
    })
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 }))))

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))

    expect(await screen.findByText(/Camera QR detection stopped unexpectedly/)).toBeDefined()
    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(screen.getByRole('button', { name: 'Use camera' }).hasAttribute('disabled')).toBe(false)

    pasteCredential('fallback-ticket')
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
  })

  it('stops a stream that resolves after the scanner unmounts and never attaches it', async () => {
    const { stop, stream } = fakeStream()
    const pending = deferred<MediaStream>()
    const getUserMedia = vi.fn().mockReturnValue(pending.promise)
    stubIdleDetector()
    stubCamera(getUserMedia)
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    // jsdom's HTMLMediaElement has no srcObject accessor; install a spyable one for this test.
    const srcSetter = vi.fn()
    Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', {
      configurable: true,
      get: () => null,
      set: srcSetter,
    })

    const view = render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledOnce())

    // Unmount while getUserMedia is still pending, then let it resolve.
    view.unmount()
    await act(async () => {
      pending.resolve(stream)
      await pending.promise
    })

    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(srcSetter).not.toHaveBeenCalledWith(stream)
    expect(play).not.toHaveBeenCalled()
    delete (HTMLMediaElement.prototype as { srcObject?: unknown }).srcObject
  })

  it('does not orphan a stream when camera startup is re-entered before the first resolves', async () => {
    const first = fakeStream()
    const second = fakeStream()
    const pending1 = deferred<MediaStream>()
    const pending2 = deferred<MediaStream>()
    const getUserMedia = vi.fn()
      .mockReturnValueOnce(pending1.promise)
      .mockReturnValueOnce(pending2.promise)
    stubIdleDetector()
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    const useCamera = screen.getByRole('button', { name: 'Use camera' })
    // Two rapid starts before either getUserMedia settles.
    fireEvent.click(useCamera)
    fireEvent.click(useCamera)
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledTimes(2))

    await act(async () => {
      pending1.resolve(first.stream)
      pending2.resolve(second.stream)
      await Promise.resolve()
    })

    // The first (stale) start must stop its stream exactly once; the second survives.
    await waitFor(() => expect(first.stop).toHaveBeenCalledOnce())
    expect(second.stop).not.toHaveBeenCalled()
  })

  it('keeps the active stream when a superseded startup finishes late', async () => {
    const first = fakeStream()
    const second = fakeStream()
    const gum1 = deferred<MediaStream>()
    const play1 = deferred<void>()
    const getUserMedia = vi.fn()
      .mockReturnValueOnce(gum1.promise)
      .mockResolvedValueOnce(second.stream)
    stubIdleDetector()
    stubCamera(getUserMedia)
    // First start's play() hangs (pending) so it stays mid-flight; later starts resolve immediately.
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play')
    play.mockReturnValueOnce(play1.promise).mockResolvedValue()

    render(<App />)
    const useCamera = screen.getByRole('button', { name: 'Use camera' })

    // First start reaches its pending play().
    fireEvent.click(useCamera)
    await act(async () => {
      gum1.resolve(first.stream)
      await Promise.resolve()
    })

    // Re-enter: second start supersedes the first and reaches active state.
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledTimes(2))
    await act(async () => {
      play1.resolve() // first start's play finally resolves — but it is now stale
      await Promise.resolve()
    })

    // The stale first start stopped its own stream; the active second stream is untouched.
    await waitFor(() => expect(first.stop).toHaveBeenCalled())
    expect(second.stop).not.toHaveBeenCalled()
  })

  it('ignores a stale getUserMedia rejection without disturbing the active message', async () => {
    const active = fakeStream()
    const rejecting = deferred<MediaStream>()
    const getUserMedia = vi.fn()
      .mockReturnValueOnce(rejecting.promise)
      .mockResolvedValueOnce(active.stream)
    stubIdleDetector()
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    const useCamera = screen.getByRole('button', { name: 'Use camera' })
    fireEvent.click(useCamera)
    // Supersede with a second start that succeeds.
    fireEvent.click(useCamera)
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.getByText(/Point the camera/)).toBeDefined())

    await act(async () => {
      rejecting.reject(new Error('permission denied (stale)'))
      await Promise.resolve()
    })

    // Stale rejection must not replace the active generation's guidance with the failure message.
    expect(screen.getByText(/Point the camera/)).toBeDefined()
    expect(screen.queryByText(/Camera access was unavailable/)).toBeNull()
    expect(active.stop).not.toHaveBeenCalled()
  })

  it('stops the previous stream when the camera is restarted while already active', async () => {
    const first = fakeStream()
    const second = fakeStream()
    const getUserMedia = vi.fn()
      .mockResolvedValueOnce(first.stream)
      .mockResolvedValueOnce(second.stream)
    stubIdleDetector()
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    // First start fully completes and the camera is active.
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(screen.getByText(/Point the camera/)).toBeDefined())
    expect(first.stop).not.toHaveBeenCalled()

    // Restart via the always-present "Use camera" (Stop is a separate button); the app re-enables the
    // camera button between generations, so drive a fresh start directly.
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Stop camera' }))
    })
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.getByText(/Point the camera/)).toBeDefined())

    // The first stream must have been stopped; the second is live and untouched.
    expect(first.stop).toHaveBeenCalled()
    expect(second.stop).not.toHaveBeenCalled()
  })

  it('stops the acquired stream when video playback fails', async () => {
    const { stop, stream } = fakeStream()
    const getUserMedia = vi.fn().mockResolvedValue(stream)
    stubIdleDetector()
    stubCamera(getUserMedia)
    // play() rejecting exercises the post-attach failure path: the acquired stream must be released.
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockRejectedValue(new Error('play failed'))

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))

    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(await screen.findByText(/Camera access was unavailable/)).toBeDefined()
  })

  it('stops the acquired stream if BarcodeDetector construction throws', async () => {
    const { stop, stream } = fakeStream()
    const getUserMedia = vi.fn().mockResolvedValue(stream)
    vi.stubGlobal('BarcodeDetector', class {
      constructor() {
        throw new Error('detector construction failed')
      }
    })
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))

    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(await screen.findByText(/Camera access was unavailable/)).toBeDefined()
  })
})

// Exit mode (TKT-399). The direction is chosen on the rendered control and sent
// with the scan. The copy for an exit never tells staff to stop a person who is
// leaving: an exit is recorded or it is not recorded.
function chooseDirection(name: 'Entry' | 'Exit') {
  fireEvent.click(screen.getByRole('radio', { name }))
}

function scanBodies(fetchMock: { mock: { calls: unknown[][] } }) {
  return fetchMock.mock.calls
    .filter(([url]) => String(url).endsWith('/scans'))
    .map(([, init]) => JSON.parse((init as RequestInit).body as string))
}

const acceptedResponse = () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-16T19:00:00Z' }), { status: 200 })

// A revocation feed that answers 401 to the first device token once `revoke` is
// called, and serves every other request. Scans are accepted.
function revocableFeed() {
  let revoked = false
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (!String(url).includes('voided-tickets')) return Promise.resolve(acceptedResponse())
    const token = (init!.headers as Record<string, string>)['X-Scanner-Token']
    return Promise.resolve(revoked && token === 'paired-device-token'
      ? new Response(JSON.stringify({ error: 'scanner device is not enrolled' }), { status: 401 })
      : new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
  })
  vi.stubGlobal('fetch', fetchMock)
  return {
    fetchMock,
    revoke: () => { revoked = true },
    // The pull on load must finish first, or a refresh joins it instead of asking again.
    firstPullDone: () => waitFor(() => expect(screen.queryAllByText(/holds no revocation list yet/)).toHaveLength(0)),
  }
}

// The device loses its pairing on a revocation refresh, then the operator pairs it again.
async function repairAfterFeed401(feed: ReturnType<typeof revocableFeed>) {
  feed.revoke()
  fireEvent.click(screen.getByRole('button', { name: 'Refresh revocation list' }))
  fireEvent.change(await screen.findByLabelText('Pairing token'), { target: { value: 'new-device-token' } })
  fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
  expect(await screen.findByLabelText('Ticket credential')).toBeDefined()
}

// Every occurrence row, read straight from IndexedDB, not through the app.
async function storedOccurrences(): Promise<Array<Record<string, unknown>>> {
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

describe('scan direction', () => {
  it('submits a live exit from the scan control', async () => {
    const fetchMock = routedFetch(() => Promise.resolve(acceptedResponse()))
    vi.stubGlobal('fetch', fetchMock)
    render(<App />)

    expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).checked).toBe(false)
    expect(screen.getByText('Entry mode')).toBeDefined()
    pasteCredential('entry-ticket')
    expect(await screen.findByText(/^Entry recorded at /)).toBeDefined()

    chooseDirection('Exit')
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).checked).toBe(true)
    expect(screen.getByText('Exit mode')).toBeDefined()
    pasteCredential('exit-ticket')
    expect(await screen.findByText(/^Exit recorded at /)).toBeDefined()
    expect(screen.getByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(screen.queryByText(/Entry recorded/)).toBeNull()

    expect(scanBodies(fetchMock).map((body) => [body.qr_payload, body.direction])).toEqual([
      ['entry-ticket', 'entry'],
      ['exit-ticket', 'exit'],
    ])
  })

  // Each row is the exact copy an operator reads. Entry copy is unchanged (D12),
  // so an Entry row is here to catch exit wording that leaks into entry mode.
  it.each([
    ['an accepted exit', 'Exit', 200, { decision: 'accepted', scanned_at: '2026-10-16T19:00:00Z' }, 'Accepted',
      [`Exit recorded at ${new Date('2026-10-16T19:00:00Z').toLocaleString()}.`]],
    ['a replayed exit', 'Exit', 200, { decision: 'accepted', scanned_at: '2026-10-16T19:00:00Z', replay: true }, 'Accepted',
      [`Exit recorded at ${new Date('2026-10-16T19:00:00Z').toLocaleString()}.`, 'This scan was previously recorded — result replayed, exit counted once.']],
    ['an exit with no open entry', 'Exit', 409, { decision: 'rejected', reason: 'not_inside' }, 'Exit not recorded',
      ['This pass is not recorded as inside.', 'Reason: not_inside.', 'The holder may leave.']],
    ['an exit on a single-entry ticket', 'Exit', 409, { decision: 'rejected', reason: 'exit_not_applicable' }, 'Exit not recorded',
      ['Exit is not available for this ticket.', 'Reason: exit_not_applicable.', 'The holder may leave.']],
    ['an exit of a refunded ticket', 'Exit', 409, { decision: 'rejected', reason: 'refunded' }, 'Exit not recorded',
      ['This ticket was refunded.', 'Reason: refunded.', 'The holder may leave.', 'This ticket is no longer valid for entry.']],
    ['an exit of an exchanged ticket', 'Exit', 409, { decision: 'rejected', reason: 'exchanged' }, 'Exit not recorded',
      ['This ticket was exchanged.', 'Reason: exchanged.', 'The holder may leave.', 'This ticket is no longer valid for entry.']],
    ['an exit whose record cannot be verified', 'Exit', 409, { decision: 'rejected', reason: 'exit_unverified' }, 'Exit not recorded',
      ['The ticket record could not be verified, so the exit was not counted.', 'Reason: exit_unverified.', 'The holder may leave.']],
    ['an exit of a quarantined ticket', 'Exit', 409, { decision: 'rejected', reason: 'integrity_quarantined' }, 'Exit not recorded',
      ['This ticket is held for an integrity check.', 'Reason: integrity_quarantined.', 'The holder may leave.']],
    ['an exit with a reason this scanner does not know', 'Exit', 422, { decision: 'rejected', reason: 'invalid_credential' }, 'Exit not recorded',
      ['Access did not record this exit.', 'Reason: invalid_credential.', 'The holder may leave.']],
    ['an accepted entry', 'Entry', 200, { decision: 'accepted', scanned_at: '2026-10-16T19:00:00Z' }, 'Accepted',
      [`Entry recorded at ${new Date('2026-10-16T19:00:00Z').toLocaleString()}.`]],
    ['a rejected entry', 'Entry', 422, { decision: 'rejected', reason: 'invalid_credential' }, 'Rejected',
      ['Credential is invalid or cannot be redeemed.']],
  ] as const)('displays %s with its direction', async (_name, direction, status, body, heading, lines) => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(new Response(JSON.stringify(body), { status }))))
    render(<App />)
    chooseDirection(direction)
    pasteCredential()

    expect(await screen.findByRole('heading', { name: heading })).toBeDefined()
    const result = screen.getByRole('heading', { name: heading }).closest('section')!
    expect(Array.from(result.querySelectorAll('p'), (line) => line.textContent)).toEqual(lines)
    if (direction === 'Exit') {
      expect(screen.queryByRole('heading', { name: 'Rejected' })).toBeNull()
      expect(screen.queryByText(/Credential is invalid/)).toBeNull()
    } else {
      expect(screen.queryByText(/The holder may leave/)).toBeNull()
    }
  })

  it('resets mode to entry after reload', async () => {
    const fetchMock = routedFetch(() => Promise.resolve(acceptedResponse()))
    vi.stubGlobal('fetch', fetchMock)
    const view = render(<App />)
    chooseDirection('Exit')
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).checked).toBe(true)

    view.unmount()
    cleanup()
    render(<App />)
    expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).checked).toBe(false)
    pasteCredential('after-reload')
    expect(await screen.findByText(/^Entry recorded at /)).toBeDefined()
    expect(scanBodies(fetchMock).map((body) => body.direction)).toEqual(['entry'])
  })

  it('resets mode to entry after the device is paired again', async () => {
    let scans = 0
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(scans++ === 0
      ? new Response(JSON.stringify({ error: 'scanner device is not enrolled' }), { status: 401 })
      : acceptedResponse())))
    render(<App />)
    chooseDirection('Exit')
    pasteCredential()

    fireEvent.change(await screen.findByLabelText('Pairing token'), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
    expect(await screen.findByLabelText('Ticket credential')).toBeDefined()
    expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).checked).toBe(false)
    expect(screen.getByText('Entry mode')).toBeDefined()
  })

  it('clears an exit result when the device is paired again', async () => {
    const feed = revocableFeed()
    render(<App />)
    await feed.firstPullDone()
    chooseDirection('Exit')
    pasteCredential('exit-ticket')
    expect(await screen.findByText(/^Exit recorded at /)).toBeDefined()

    await repairAfterFeed401(feed)
    expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(screen.queryByText(/Exit recorded/)).toBeNull()
  })

  // A detection that started in Exit mode before the device lost its pairing must not
  // submit after the operator pairs again: the mode is Entry then, so the exit would
  // be minted and synced as an entry.
  it('stops a camera generation when the device loses its pairing', async () => {
    const { stop, stream } = fakeStream()
    const staleDetection = deferred<Array<{ rawValue?: string }>>()
    const detect = vi.fn().mockReturnValueOnce(staleDetection.promise)
    vi.stubGlobal('BarcodeDetector', class {
      detect = detect
    })
    stubCamera(vi.fn().mockResolvedValue(stream))
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    const feed = revocableFeed()

    render(<App />)
    await feed.firstPullDone()
    chooseDirection('Exit')
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(detect).toHaveBeenCalledOnce())

    await repairAfterFeed401(feed)
    expect(stop).toHaveBeenCalledOnce()
    expect(screen.queryByRole('button', { name: 'Stop camera' })).toBeNull()

    // The detect loop awaited this promise before the test did, so its continuation
    // has run when this await returns. A live generation would have started its scan.
    await act(async () => {
      staleDetection.resolve([{ rawValue: 'stale-exit-ticket' }])
      await staleDetection.promise
    })
    // A scan through the screen is the barrier. A scan that started from the stale
    // detection holds the button until it has finished, so it is sent first.
    await screen.findByRole('button', { name: 'Check ticket' })
    pasteCredential('after-repair-ticket')
    expect(await screen.findByText(/^Entry recorded at /)).toBeDefined()
    expect(scanBodies(feed.fetchMock).map((body) => [body.qr_payload, body.direction])).toEqual([
      ['after-repair-ticket', 'entry'],
    ])
    expect((await storedOccurrences()).filter((record) => record.qrPayload === 'stale-exit-ticket')).toEqual([])
  })

  it('clears the previous result when direction changes', async () => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(acceptedResponse())))
    render(<App />)
    pasteCredential()
    expect(await screen.findByText(/^Entry recorded at /)).toBeDefined()

    chooseDirection('Exit')
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(screen.queryByText(/Entry recorded/)).toBeNull()
  })

  it('does not let the direction change while a scan is in flight', async () => {
    const reply = deferred<Response>()
    vi.stubGlobal('fetch', routedFetch(() => reply.promise))
    render(<App />)
    pasteCredential()
    await waitFor(() => expect(screen.getByRole('button', { name: 'Checking…' })).toBeDefined())

    expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).disabled).toBe(true)
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).disabled).toBe(true)

    reply.resolve(acceptedResponse())
    expect(await screen.findByText(/^Entry recorded at /)).toBeDefined()
    expect((screen.getByRole('radio', { name: 'Exit' }) as HTMLInputElement).disabled).toBe(false)
  })

  // A detection started under Entry must not submit after the operator chose Exit:
  // the person in front of the camera would be counted in the wrong direction.
  it('stops a camera generation when direction changes', async () => {
    const first = fakeStream()
    const second = fakeStream()
    const staleDetection = deferred<Array<{ rawValue?: string }>>()
    const detect = vi.fn()
      .mockReturnValueOnce(staleDetection.promise)
      .mockResolvedValue([{ rawValue: 'camera-exit-ticket' }])
    vi.stubGlobal('BarcodeDetector', class {
      detect = detect
    })
    const getUserMedia = vi.fn()
      .mockResolvedValueOnce(first.stream)
      .mockResolvedValueOnce(second.stream)
    stubCamera(getUserMedia)
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    const fetchMock = routedFetch(() => Promise.resolve(acceptedResponse()))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(detect).toHaveBeenCalledOnce())

    chooseDirection('Exit')
    expect(first.stop).toHaveBeenCalledOnce()
    expect(screen.queryByRole('button', { name: 'Stop camera' })).toBeNull()

    // The detect loop awaited this promise before the test did, so its continuation
    // has run when this await returns. A live generation would have put the code in
    // the credential field before it started the scan.
    await act(async () => {
      staleDetection.resolve([{ rawValue: 'stale-entry-ticket' }])
      await staleDetection.promise
    })
    expect((screen.getByLabelText('Ticket credential') as HTMLTextAreaElement).value).toBe('')
    expect(screen.getByRole('button', { name: 'Check ticket' })).toBeDefined()
    expect(detect).toHaveBeenCalledOnce()

    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    expect(await screen.findByText(/^Exit recorded at /)).toBeDefined()
    expect(scanBodies(fetchMock).map((body) => [body.qr_payload, body.direction])).toEqual([
      ['camera-exit-ticket', 'exit'],
    ])
    expect(second.stop).toHaveBeenCalledOnce()
  })

  it('cancels a pending camera start when direction changes', async () => {
    const { stop, stream } = fakeStream()
    const pending = deferred<MediaStream>()
    stubIdleDetector()
    stubCamera(vi.fn().mockReturnValue(pending.promise))
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    chooseDirection('Exit')
    await act(async () => {
      pending.resolve(stream)
      await pending.promise
    })

    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(play).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: 'Stop camera' })).toBeNull()
  })
})

// The scan routes require an enrolled device, so the app has to keep its
// credential and present a clear unpaired state.
describe('device pairing', () => {
  it('asks to be paired instead of scanning, and never sends a scan unpaired', async () => {
    localStorage.clear()
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    expect(await screen.findByRole('heading', { name: 'Pair this device' })).toBeDefined()
    // The scan form is not merely disabled — it is not there. A gate operator
    // cannot paste a credential into a scanner that has no credential itself.
    expect(screen.queryByLabelText('Ticket credential')).toBeNull()
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringContaining('/scans'), expect.anything())
  })

  it('pairs, keeps the token across a reload, and sends it on every scan', async () => {
    localStorage.clear()
    const fetchMock = routedFetch(() => Promise.resolve(
      new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-08-10T12:00:00Z' }), { status: 200 }),
    ))
    vi.stubGlobal('fetch', fetchMock)

    const { unmount } = render(<App />)
    fireEvent.change(await screen.findByLabelText('Pairing token'), { target: { value: '  gate-token  ' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))

    // Trimmed on the way in: a token pasted from a terminal carries whitespace,
    // and an untrimmed one is refused in a way that reads as "revoked device".
    await waitFor(() => expect(localStorage.getItem('scanner.device-token')).toBe('gate-token'))

    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    const scanCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/scans'))!
    const headers = (scanCall[1] as RequestInit).headers as Record<string, string>
    expect(headers['X-Scanner-Token']).toBe('gate-token')

    // Survives a reload — a gate device is paired once, not once per page load.
    unmount()
    cleanup()
    render(<App />)
    expect(await screen.findByLabelText('Ticket credential')).toBeDefined()
  })

  // Absent versus broken, at the turnstile (TKT-305).
  //
  // A non-JSON error body — a gateway's 502 page, an access panic — makes
  // response.json() throw, and that throw lands in the same catch a genuine network
  // failure does. The scan was queued either way, correctly; the COPY said "No
  // connection", telling gate staff the network is down when access had answered.
  // At a venue that sends someone to check the wifi while the actual fault is
  // upstream.
  //
  // The QUEUE BEHAVIOUR is deliberately unchanged and is asserted in both cases:
  // ADR-066's fail-closed posture is not what this ticket touches, and a fix that
  // stopped queueing would be far worse than the wrong message.
  it('says the server answered badly, not "No connection", when the reply cannot be read', async () => {
    // A 502 whose body is an HTML error page: reached the server, unreadable reply.
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(
      new Response('<html><body>502 Bad Gateway</body></html>', { status: 502 }),
    )))

    render(<App />)
    pasteCredential()

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/the server answered but the reply could not be read/i)).toBeDefined()
    expect(screen.queryByText(/No connection/i)).toBeNull()
    // Still queued — the behaviour, unchanged.
    expect(await screen.findByText(/offline scan/i)).toBeDefined()
  })

  // A valid accepted response does not prove that this device recorded the
  // actuation. Report the local write failure without blaming the network or server.
  it('blames this device, not the server, when the reply was fine and the local write failed', async () => {
    const reply = deferred<Response>()
    const fetchMock = vi.fn((url: string) => String(url).includes('voided-tickets')
      ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
      : reply.promise)
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    pasteCredential()
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(1))

    // Mint has committed before fetch starts. Failing every later write targets
    // actuation and its fail-closed queue attempt without relying on transaction counts.
    const realTransaction = IDBDatabase.prototype.transaction
    vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementation(function (this: IDBDatabase, ...args: Parameters<typeof realTransaction>) {
      if (args[1] === 'readwrite') {
        throw new DOMException('device storage unavailable', 'InvalidStateError')
      }
      return realTransaction.apply(this, args)
    })

    reply.resolve(
      new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-08-30T20:00:00Z' }), { status: 200 }),
    )

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/could not be recorded on this device/i)).toBeDefined()
    expect(screen.queryByText(/No connection/i)).toBeNull()
    expect(screen.queryByText(/the server answered/i)).toBeNull()
  })

  it('still says "No connection" when the request never reached the server', async () => {
    // fetch itself rejects: this is the case the old copy described correctly, and
    // it must keep saying so — otherwise the fix has merely moved the wrong message.
    vi.stubGlobal('fetch', vi.fn((url: string) => String(url).includes('voided-tickets')
      ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
      : Promise.reject(new TypeError('Failed to fetch'))))

    render(<App />)
    pasteCredential()

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/No connection/i)).toBeDefined()
    expect(screen.queryByText(/the server answered/i)).toBeNull()
    expect(await screen.findByText(/offline scan/i)).toBeDefined()
  })

  it('treats a 401 as "pair the device", not as a rejected ticket, and keeps the scan queued', async () => {
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(
      new Response(JSON.stringify({ error: 'scanner device is not enrolled' }), { status: 401 }),
    )))

    render(<App />)
    pasteCredential()

    // The distinction that matters at a turnstile: the person in front of the
    // operator has a perfectly good ticket, and "Rejected" is the wrong
    // instruction. They land on the one screen they can act on, with the reason.
    expect(await screen.findByRole('heading', { name: 'Pair this device' })).toBeDefined()
    expect(screen.queryByRole('heading', { name: 'Rejected' })).toBeNull()
    expect(await screen.findByText(/the ticket was not checked/i)).toBeDefined()

    // The stale token is gone, so a reload does not retry it forever.
    expect(localStorage.getItem('scanner.device-token')).toBeNull()

    // And the occurrence is not lost. It was never recorded anywhere upstream,
    // so discarding it would silently drop a real entry.
    expect(await screen.findByText(/offline scan/i)).toBeDefined()
  })
})
