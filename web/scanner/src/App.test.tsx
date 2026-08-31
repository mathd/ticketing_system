import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'

// Every scan and reconciliation needs an enrolled device (ai-review S1). The
// suites below are about SCAN behaviour, so they pair once here — otherwise each
// of them would be re-asserting the pairing screen and its own subject not at
// all. The pairing screen has its own tests, in App.test.tsx.
beforeEach(() => {
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function pasteCredential(value = 'signed-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
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

describe('App', () => {
  it('renders the scanner and accepts a pasted credential', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
  })

  it('shows the original scan time for a duplicate', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'rejected', reason: 'already_redeemed', original_scan_at: '2026-07-13T12:00:00Z' }), { status: 409 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByText(/Already redeemed at/)).toBeDefined()
  })

  it('rejects an invalid credential', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'rejected', reason: 'invalid_credential' }), { status: 422 })))
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 })))

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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
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
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))

    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(await screen.findByText(/Camera access was unavailable/)).toBeDefined()
  })
})

// ai-review S1. The scan routes now require an enrolled device, so the app has to
// have somewhere to keep one and something to do when it does not.
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
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('pairs, keeps the token across a reload, and sends it on every scan', async () => {
    localStorage.clear()
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-08-10T12:00:00Z' }), { status: 200 }),
    )
    vi.stubGlobal('fetch', fetchMock)

    const { unmount } = render(<App />)
    fireEvent.change(await screen.findByLabelText('Pairing token'), { target: { value: '  gate-token  ' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))

    // Trimmed on the way in: a token pasted from a terminal carries whitespace,
    // and an untrimmed one is refused in a way that reads as "revoked device".
    expect(localStorage.getItem('scanner.device-token')).toBe('gate-token')

    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    const headers = fetchMock.mock.calls[0][1].headers as Record<string, string>
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
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response('<html><body>502 Bad Gateway</body></html>', { status: 502 }),
    ))

    render(<App />)
    pasteCredential()

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/the server answered but the reply could not be read/i)).toBeDefined()
    expect(screen.queryByText(/No connection/i)).toBeNull()
    // Still queued — the behaviour, unchanged.
    expect(await screen.findByText(/offline scan/i)).toBeDefined()
  })

  it('still says "No connection" when the request never reached the server', async () => {
    // fetch itself rejects: this is the case the old copy described correctly, and
    // it must keep saying so — otherwise the fix has merely moved the wrong message.
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

    render(<App />)
    pasteCredential()

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/No connection/i)).toBeDefined()
    expect(screen.queryByText(/the server answered/i)).toBeNull()
    expect(await screen.findByText(/offline scan/i)).toBeDefined()
  })

  it('treats a 401 as "pair the device", not as a rejected ticket, and keeps the scan queued', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'scanner device is not enrolled' }), { status: 401 }),
    ))

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
