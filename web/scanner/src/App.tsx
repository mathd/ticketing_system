import { useEffect, useRef, useState } from 'react'
import { openOccurrenceStore, type OccurrenceStore } from './occurrences'
import './index.css'

type ScanOutcome =
  | { kind: 'accepted'; scannedAt: string; replay: boolean }
  | { kind: 'rejected'; reason: string; originalScanAt?: string }
  | { kind: 'queued'; cause: 'offline' | 'unreadable' | 'local' }
  | { kind: 'duplicate-response' }

// What the operator is told about a queued scan. Each string says only what the code
// established (TKT-305): the old single message claimed "No connection" for all three,
// including the case where the server answered and failed. The three read differently
// on purpose — one sends staff to the network, one upstream, one to this device — and
// the instruction that follows them ("saved on this device, admit per venue policy") is
// the same in every case, because the queue behaviour is.
const QUEUED_CAUSE = {
  offline: 'No connection',
  unreadable: 'The server answered but the reply could not be read',
  local: 'The scan was sent but could not be recorded on this device',
} as const

type BarcodeDetectorInstance = {
  detect(source: HTMLVideoElement): Promise<Array<{ rawValue?: string }>>
}

type BarcodeDetectorConstructor = new (options: { formats: string[] }) => BarcodeDetectorInstance

declare global {
  interface Window {
    BarcodeDetector?: BarcodeDetectorConstructor
  }
}

const scanURL = '/api/access/scans'
const reconcileURL = '/api/access/scans/reconciliations'

// The enrolled device's credential (ai-review S1).
//
// Held per DEVICE, in this browser's storage, and never compiled into the bundle:
// this app is static and served to every phone that loads /scanner/, so a shared
// token baked in at build time would be published, not secret. An operator enrols
// a gate with `access enrol-scanner` and pairs it once, here.
//
// localStorage rather than the IndexedDB queue: it is read synchronously on every
// request, it must survive a reload, and it is one string. Same origin, same
// device-loss exposure as the queue itself — a lost phone is answered by revoking
// that device, which is the whole reason the credential is per device.
const deviceTokenKey = 'scanner.device-token'
const scannerTokenHeader = 'X-Scanner-Token'

function readDeviceToken(): string {
  try {
    return localStorage.getItem(deviceTokenKey)?.trim() ?? ''
  } catch {
    // Private-mode browsers can throw on access. An unpaired scanner is a
    // legible state; a crashed one is not.
    return ''
  }
}

// scanHeaders is the ONE place the credential is attached, so a new call to the
// access API cannot forget it and discover the omission as a 401 at a live door.
function scanHeaders(token: string): HeadersInit {
  return { 'Content-Type': 'application/json', [scannerTokenHeader]: token }
}

// Opened once per page: the durable occurrence queue (ADR-025 §D3). Every scan
// commits its PENDING record here before the request leaves the device.
const storePromise: Promise<OccurrenceStore> = openOccurrenceStore()

function readableTime(value?: string) {
  return value ? new Date(value).toLocaleString() : undefined
}

function App() {
  const [payload, setPayload] = useState('')
  const [outcome, setOutcome] = useState<ScanOutcome | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [queuedCount, setQueuedCount] = useState(0)
  const [syncNote, setSyncNote] = useState('')
  const [cameraMessage, setCameraMessage] = useState('')
  const [cameraActive, setCameraActive] = useState(false)
  const [deviceToken, setDeviceToken] = useState(readDeviceToken)
  const [pairingInput, setPairingInput] = useState('')
  const video = useRef<HTMLVideoElement>(null)
  const stream = useRef<MediaStream | null>(null)
  const frame = useRef<number | null>(null)
  const mounted = useRef(true)
  // Bumped by every stopCamera() and every startCamera() entry. A start captures its value and
  // treats itself as stale once the counter moves on — so a stream resolving after unmount or a
  // superseded start disposes of its own resource instead of touching the active one.
  const generation = useRef(0)

  const stopCamera = () => {
    generation.current += 1
    if (frame.current !== null) cancelAnimationFrame(frame.current)
    frame.current = null
    stream.current?.getTracks().forEach((track) => track.stop())
    stream.current = null
    setCameraActive(false)
  }

  useEffect(() => {
    mounted.current = true
    void refreshQueued()
    // Reconnect is the natural sync point for the offline queue (ADR-025 §D6).
    const onOnline = () => void syncQueued()
    window.addEventListener('online', onOnline)
    return () => {
      mounted.current = false
      window.removeEventListener('online', onOnline)
      stopCamera()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const refreshQueued = async () => {
    const store = await storePromise
    const queue = await store.queued()
    if (mounted.current) setQueuedCount(queue.length)
  }

  const submit = async (value = payload) => {
    if (!value.trim() || submitting) return
    setSubmitting(true)
    setOutcome(null)
    // Mint and durably commit the occurrence BEFORE the request leaves
    // (ADR-025 §D3): a retry reuses this record; a new scan mints a new one.
    const store = await storePromise
    const record = await store.mint(value.trim(), new Date().toISOString())
    // Why the scan could not complete, for the operator (TKT-305). Three causes, and
    // the UI may only assert one it actually established:
    //
    //   'offline'      the request never arrived — fetch itself rejected
    //   'unreadable'   the server answered and the answer could not be parsed
    //   'local'        the exchange succeeded and something on THIS DEVICE failed
    //
    // The old code collapsed all three into "No connection". A non-JSON error body —
    // a gateway's 502 page, an access panic — makes response.json() throw and lands
    // in the same catch a dead network does, so gate staff were told the network was
    // down while access had answered and failed. At a venue that sends someone to
    // check the wifi while the fault is upstream.
    //
    // 'local' is separated for the same reason 'unreadable' is (ai-review [medium]):
    // the catch also covers store.actuate, store.markQueued and refreshQueued, so an
    // IndexedDB failure after a perfectly good reply would otherwise report a server
    // problem — the identical unsupported claim, one step along.
    //
    // The queue-and-retry behaviour is unchanged in all three cases (ADR-066).
    let cause: 'offline' | 'unreadable' | 'local' = 'offline'
    try {
      const response = await fetch(scanURL, {
        method: 'POST',
        headers: scanHeaders(deviceToken),
        body: JSON.stringify({ qr_payload: record.qrPayload, occurrence_id: record.occurrenceId, occurred_at: record.occurredAt }),
      })
      // The request arrived. Anything that throws from here to the end of the parse
      // is the server's answer being unreadable, not the network being down.
      cause = 'unreadable'
      const result: { decision?: string; reason?: string; scanned_at?: string; original_scan_at?: string; replay?: boolean } = await response.json()
      // Parsed. Anything that throws beyond this point is local to this device.
      cause = 'local'
      if (response.ok && result.decision === 'accepted') {
        // Actuation is keyed on OUR durable pending record, not on the
        // response: mark-actuated-before-open, and a response for an
        // occurrence this device never minted (or already actuated) opens
        // nothing (ADR-025 §D3).
        const opened = await store.actuate(record.occurrenceId)
        if (opened) {
          setOutcome({ kind: 'accepted', scannedAt: result.scanned_at ?? '', replay: result.replay === true })
        } else {
          setOutcome({ kind: 'duplicate-response' })
        }
      } else if (response.status === 401) {
        // Not the ticket's fault, so not a rejection: the person at the turnstile
        // has a perfectly good ticket and "Rejected" is the wrong instruction.
        // Clearing the token drops straight to the pairing screen, which is the
        // one thing the operator can act on, and the note says why they landed
        // there.
        //
        // The occurrence stays QUEUED rather than marked synced — it was never
        // recorded anywhere upstream, and discarding it because this device was
        // unpaired would silently drop a real entry.
        await store.markQueued(record.occurrenceId)
        clearPairing('This device is not paired, so the ticket was not checked. Enter its pairing token before admitting anyone.')
        await refreshQueued()
      } else {
        await store.markSynced(record.occurrenceId, result.reason ?? 'scan_failed')
        setOutcome({ kind: 'rejected', reason: result.reason ?? 'scan_failed', originalScanAt: result.original_scan_at })
      }
    } catch {
      // Queued in every case: the occurrence stays durably recorded and reconciles on
      // sync, which is the fail-closed posture ADR-066 requires and is NOT what this
      // ticket changes. Only the explanation differs — see `cause`.
      //
      // The queue write is itself guarded, because on the 'local' path the thing that
      // just failed IS this device's storage — so markQueued throws too, the exception
      // escapes submitScan, and the operator gets NO screen at all. Found while testing
      // the 'local' message (ai-review [medium]); an unreadable outcome is worse than a
      // wrongly-worded one, since staff are left with a blank result and a person at the
      // turnstile. The occurrence was already minted and durably committed before the
      // request left (ADR-025 §D3), so a failed markQueued does not lose it — the record
      // stays pending and the next sync picks it up.
      try {
        await store.markQueued(record.occurrenceId)
      } catch {
        // Deliberately swallowed: the screen below is the only thing left that can
        // help the operator, and it must render.
      }
      setOutcome({ kind: 'queued', cause })
      // Guarded for the same reason: this reads the queue, so on the 'local' path it
      // throws as well, and an exception escaping here would still deny the operator
      // the screen set on the line above.
      try {
        await refreshQueued()
      } catch {
        // The queued-count badge is a convenience; the outcome screen is not.
      }
    } finally {
      setSubmitting(false)
    }
  }

  const syncQueued = async () => {
    const store = await storePromise
    const queue = await store.queued()
    if (!queue.length) return
    try {
      const response = await fetch(reconcileURL, {
        method: 'POST',
        headers: scanHeaders(deviceToken),
        body: JSON.stringify({
          occurrences: queue.map((r) => ({ qr_payload: r.qrPayload, occurrence_id: r.occurrenceId, occurred_at: r.occurredAt })),
        }),
      })
      if (response.status === 401) {
        // The queue is untouched: an unpaired device must not discard a night of
        // offline scans. Pair and sync again.
        clearPairing('This device is not paired. Enter its pairing token to sync the queued scans.')
        return
      }
      if (!response.ok) return
      const data: { results?: Array<{ occurrence_id: string; result: string }> } = await response.json()
      let conflicts = 0
      for (const entry of data.results ?? []) {
        await store.markSynced(entry.occurrence_id, entry.result)
        if (entry.result === 'conflict') conflicts += 1
      }
      if (mounted.current) {
        setSyncNote(
          conflicts > 0
            ? `Synced ${data.results?.length ?? 0} offline scan(s) — ${conflicts} conflict${conflicts > 1 ? 's' : ''} flagged for the operator.`
            : `Synced ${data.results?.length ?? 0} offline scan(s).`,
        )
      }
    } catch {
      // Still offline — the queue is durable; try again later.
    }
    await refreshQueued()
  }

  const startCamera = async () => {
    if (!window.BarcodeDetector || !navigator.mediaDevices?.getUserMedia) {
      setCameraMessage('Camera QR detection is unavailable in this browser. Paste the credential instead.')
      return
    }
    // Supersede any in-flight start and claim this generation. A start is "stale" once the counter
    // moves past `gen` (another start began, stopCamera ran, or the component unmounted).
    const gen = (generation.current += 1)
    const isStale = () => !mounted.current || generation.current !== gen
    // Release resources owned by *this* start only — never the active stream of a newer generation.
    const disposeOwn = (media: MediaStream) => {
      media.getTracks().forEach((track) => track.stop())
      if (stream.current === media) stream.current = null
      if (video.current && video.current.srcObject === media) video.current.srcObject = null
    }

    let media: MediaStream
    try {
      media = await navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' } })
    } catch {
      if (isStale()) return
      stopCamera()
      setCameraMessage('Camera access was unavailable. Paste the credential instead.')
      return
    }
    if (isStale()) {
      // Unmounted or superseded while acquiring — stop this stream, touch nothing shared.
      media.getTracks().forEach((track) => track.stop())
      return
    }
    if (!video.current) {
      // No preview element to attach to: dispose the stream we just acquired (it is not yet the
      // active stream, so stopCamera would not reach it) and fall back to paste.
      media.getTracks().forEach((track) => track.stop())
      setCameraMessage('Camera preview was unavailable. Paste the credential instead.')
      return
    }

    // This start has won: stop any previously active stream before taking ownership, so a restart
    // over a fully-active camera cannot orphan the old stream. In-flight prior starts dispose
    // themselves via their own generation guard.
    stream.current?.getTracks().forEach((track) => track.stop())
    if (frame.current !== null) cancelAnimationFrame(frame.current)
    frame.current = null

    let detector: BarcodeDetectorInstance
    try {
      stream.current = media
      video.current.srcObject = media
      await video.current.play()
      detector = new window.BarcodeDetector({ formats: ['qr_code'] })
    } catch {
      // srcObject assignment, play(), or detector construction failed — release the acquired stream.
      disposeOwn(media)
      if (!isStale()) {
        setCameraActive(false)
        setCameraMessage('Camera access was unavailable. Paste the credential instead.')
      }
      return
    }
    if (isStale()) {
      disposeOwn(media)
      return
    }

    setCameraActive(true)
    setCameraMessage('Point the camera at a ticket QR code.')
    const detect = async () => {
      if (isStale() || !video.current) return
      let codes: Array<{ rawValue?: string }>
      try {
        codes = await detector.detect(video.current)
      } catch {
        if (isStale()) return
        stopCamera()
        setCameraMessage('Camera QR detection stopped unexpectedly. Paste the credential instead.')
        return
      }
      if (isStale()) return
      const value = codes[0]?.rawValue
      if (value) {
        setPayload(value)
        stopCamera()
        await submit(value)
        return
      }
      frame.current = requestAnimationFrame(() => { void detect() })
    }
    void detect()
  }

  // clearPairing sends the operator to the pairing screen with the reason they
  // are seeing it. One destination for every "this device is not enrolled"
  // answer, because two would eventually disagree about what to tell them.
  const clearPairing = (reason: string) => {
    try {
      localStorage.removeItem(deviceTokenKey)
    } catch {
      // Storage unavailable; the in-memory clear below is what matters.
    }
    setDeviceToken('')
    setSyncNote(reason)
  }

  const pairDevice = (event: React.FormEvent) => {
    event.preventDefault()
    const token = pairingInput.trim()
    if (!token) return
    try {
      localStorage.setItem(deviceTokenKey, token)
    } catch {
      // Storage unavailable: pair for this session rather than refusing to work.
      // The alternative is a gate that cannot open because a browser setting.
    }
    setDeviceToken(token)
    setPairingInput('')
  }

  if (!deviceToken) {
    return (
      <main className="scanner">
        <h1>Gate scanner</h1>
        <section aria-label="Device pairing">
          <h2>Pair this device</h2>
          <p>
            This scanner is not paired yet. An operator enrols it once with{' '}
            <code>access enrol-scanner</code> and reads out the token it prints.
          </p>
          <form onSubmit={pairDevice}>
            <label htmlFor="pairing-token">Pairing token</label>
            <input
              id="pairing-token"
              type="password"
              value={pairingInput}
              onChange={(event) => setPairingInput(event.target.value)}
              autoComplete="off"
              placeholder="Paste the token from access enrol-scanner"
            />
            <div className="scanner-actions">
              <button type="submit" disabled={!pairingInput.trim()}>Pair device</button>
            </div>
          </form>
          {queuedCount > 0 && (
            <p className="queue-note" role="status">
              {`${queuedCount} offline scan${queuedCount > 1 ? 's' : ''} are still saved on this device and will sync once it is paired.`}
            </p>
          )}
          {syncNote && <p className="sync-note" role="status">{syncNote}</p>}
        </section>
      </main>
    )
  }

  return (
    <main className="scanner">
      <h1>Gate scanner</h1>
      <p>Scan a ticket QR code or paste its credential to validate entry.</p>
      <section aria-label="Ticket scan input">
        <label htmlFor="qr-payload">Ticket credential</label>
        <textarea id="qr-payload" value={payload} onChange={(event) => setPayload(event.target.value)} placeholder="Paste QR credential" rows={4} />
        <div className="scanner-actions">
          <button type="button" onClick={() => void submit()} disabled={submitting || !payload.trim()}>{submitting ? 'Checking…' : 'Check ticket'}</button>
          <button type="button" onClick={() => void startCamera()} disabled={cameraActive || submitting}>Use camera</button>
          {cameraActive && <button type="button" onClick={stopCamera}>Stop camera</button>}
        </div>
        <video ref={video} aria-label="Camera preview" muted playsInline hidden={!cameraActive} />
        {cameraMessage && <p className="camera-note" role="status">{cameraMessage}</p>}
        {queuedCount > 0 && (
          <p className="queue-note" role="status">
            {`${queuedCount} queued offline scan${queuedCount > 1 ? 's' : ''} awaiting sync.`}{' '}
            <button type="button" onClick={() => void syncQueued()}>Sync queued scans</button>
          </p>
        )}
        {syncNote && <p className="sync-note" role="status">{syncNote}</p>}
      </section>
      {outcome?.kind === 'accepted' && (
        <section className="result accepted" role="status">
          <h2>Accepted</h2>
          <p>Entry recorded at {readableTime(outcome.scannedAt)}.</p>
          {outcome.replay && <p>This scan was previously recorded — result replayed, entry counted once.</p>}
        </section>
      )}
      {outcome?.kind === 'rejected' && <section className="result rejected" role="alert"><h2>Rejected</h2><p>{outcome.reason === 'already_redeemed' ? `Already redeemed at ${readableTime(outcome.originalScanAt)}.` : 'Credential is invalid or cannot be redeemed.'}</p></section>}
      {outcome?.kind === 'queued' && <section className="result queued" role="status"><h2>Queued offline</h2><p>{QUEUED_CAUSE[outcome.cause]} — the scan is saved on this device and will sync when back online. Admit per venue offline policy.</p></section>}
      {outcome?.kind === 'duplicate-response' && <section className="result rejected" role="alert"><h2>Already processed</h2><p>This scan was already handled on this device — no second entry.</p></section>}
    </main>
  )
}

export default App
