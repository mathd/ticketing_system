import { useEffect, useRef, useState } from 'react'
import {
  decodeAccessError,
  decodeReconcileResponse,
  decodeScanResponse,
  type ReconcileResponse,
  type ReconcileRequest,
  type ScanRequest,
} from './access-contract'
import { createOccurrenceOwner, replaceOccurrenceOwner } from './occurrence-owner'
import { openOccurrenceStore, type OccurrenceRecord, type OccurrenceStore } from './occurrences'

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
const storageBeforeScanMessage = 'This device cannot save scans right now. No ticket was checked. Try again after restoring browser storage.'
const storageAfterResponseMessage = 'This device could not save the server result. Do not rescan until browser storage is restored.'

// The enrolled device's credential.
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
const pageOccurrenceOwner = createOccurrenceOwner()

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
  const [storageFailure, setStorageFailure] = useState<string | null>(null)
  const video = useRef<HTMLVideoElement>(null)
  const stream = useRef<MediaStream | null>(null)
  const frame = useRef<number | null>(null)
  const mounted = useRef(true)
  const storePromise = useRef<Promise<OccurrenceStore> | null>(null)
  const occurrenceOwner = useRef(pageOccurrenceOwner)
  const reopenAfter = useRef<Promise<void>>(Promise.resolve())
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

  const getStore = (): Promise<OccurrenceStore> => {
    if (storePromise.current) return storePromise.current
    const owner = occurrenceOwner.current
    const attempt = reopenAfter.current.then(() => openOccurrenceStore('gate-occurrences', owner))
    storePromise.current = attempt
    return attempt
  }

  const reportStorageFailure = (message = storageBeforeScanMessage) => {
    const failedStore = storePromise.current
    storePromise.current = null
    if (failedStore) {
      const releasedOwner = occurrenceOwner.current
      occurrenceOwner.current = replaceOccurrenceOwner(releasedOwner)
      const priorClose = reopenAfter.current
      const failedClose = failedStore
        .then((opened) => opened.close())
        .catch(() => undefined)
      reopenAfter.current = Promise.all([priorClose, failedClose]).then(() => undefined)
    }
    if (mounted.current) setStorageFailure(message)
  }

  const reportStorageReady = () => {
    if (mounted.current) setStorageFailure(null)
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
      const pendingStore = storePromise.current
      storePromise.current = null
      if (pendingStore) void pendingStore.then((opened) => opened.close()).catch(() => undefined)
      stopCamera()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const refreshQueued = async () => {
    try {
      const store = await getStore()
      const queue = await store.queued()
      if (mounted.current) {
        setQueuedCount(queue.length)
        setStorageFailure(null)
      }
    } catch {
      reportStorageFailure()
    }
  }

  const submit = async (value = payload) => {
    if (!value.trim() || submitting) return
    setSubmitting(true)
    setOutcome(null)
    try {
      let store: OccurrenceStore
      let record: OccurrenceRecord
      try {
        store = await getStore()
        // Commit the occurrence before sending the request (ADR-025 §D3).
        record = await store.mint(value.trim(), new Date().toISOString())
        reportStorageReady()
      } catch {
        reportStorageFailure()
        return
      }

      // Track how far the exchange reached so the operator sees the cause the
      // scanner established. Every failure still queues the occurrence.
      let cause: 'offline' | 'unreadable' | 'local' = 'offline'
      try {
        const request: ScanRequest = {
          qr_payload: record.qrPayload,
          occurrence_id: record.occurrenceId,
          occurred_at: record.occurredAt,
        }
        const response = await fetch(scanURL, {
          method: 'POST',
          headers: scanHeaders(deviceToken),
          body: JSON.stringify(request),
        })
        cause = 'unreadable'
        const body: unknown = await response.json()
        if (response.status === 401) {
          decodeAccessError(body)
          cause = 'local'
          // The ticket was not checked. Keep the occurrence and return the device
          // to pairing instead of presenting a ticket rejection.
          await store.markQueued(record.occurrenceId)
          clearPairing('This device is not paired, so the ticket was not checked. Enter its pairing token before admitting anyone.')
          await refreshQueued()
          return
        }
        const result = decodeScanResponse(response.status, body)
        cause = 'local'
        if (result.decision === 'accepted') {
          // A response opens the gate only through this device's pending record.
          const opened = await store.actuate(record.occurrenceId)
          if (opened) {
            setOutcome({ kind: 'accepted', scannedAt: result.scanned_at, replay: result.replay === true })
          } else {
            setOutcome({ kind: 'duplicate-response' })
          }
        } else {
          setOutcome({ kind: 'rejected', reason: result.reason, originalScanAt: result.original_scan_at })
          try {
            await store.markSynced(record.occurrenceId, result.reason)
          } catch {
            // Access has decided this ticket. Keep that decision on screen even
            // when the local terminal-state write fails. Reopening under a new
            // owner will recover the pending row for reconciliation.
            reportStorageFailure(storageAfterResponseMessage)
          }
          return
        }
      } catch {
        // Preserve the operator result even when the queue write also fails. A
        // later store open recovers a record that remains PENDING.
        let queueReadable = true
        try {
          await store.markQueued(record.occurrenceId)
        } catch {
          queueReadable = false
          reportStorageFailure(cause === 'local' ? storageAfterResponseMessage : undefined)
        }
        setOutcome({ kind: 'queued', cause })
        if (queueReadable) await refreshQueued()
      }
    } finally {
      setSubmitting(false)
    }
  }

  const syncQueued = async () => {
    let store: OccurrenceStore
    let queue: OccurrenceRecord[]
    try {
      store = await getStore()
      queue = await store.queued()
      reportStorageReady()
    } catch {
      reportStorageFailure()
      return
    }
    if (!queue.length) return
    let data: ReconcileResponse
    try {
      const request: ReconcileRequest = {
        occurrences: queue.map((record) => ({
          qr_payload: record.qrPayload,
          occurrence_id: record.occurrenceId,
          occurred_at: record.occurredAt,
        })),
      }
      const response = await fetch(reconcileURL, {
        method: 'POST',
        headers: scanHeaders(deviceToken),
        body: JSON.stringify(request),
      })
      if (response.status === 401) {
        // The queue is untouched: an unpaired device must not discard a night of
        // offline scans. Pair and sync again.
        clearPairing('This device is not paired. Enter its pairing token to sync the queued scans.')
        return
      }
      if (!response.ok) return
      data = decodeReconcileResponse(
        await response.json(),
        new Set(queue.map((record) => record.occurrenceId)),
      )
    } catch {
      // The queue is durable. A later reconnect or manual retry sends it again.
      return
    }

    let conflicts = 0
    try {
      for (const entry of data.results) {
        await store.markSynced(entry.occurrence_id, entry.result)
        if (entry.result === 'conflict') conflicts += 1
      }
    } catch {
      reportStorageFailure()
      return
    }
    if (mounted.current) {
      setSyncNote(
        conflicts > 0
          ? `Synced ${data.results.length} offline scan(s) — ${conflicts} conflict${conflicts > 1 ? 's' : ''} flagged for the operator.`
          : `Synced ${data.results.length} offline scan(s).`,
      )
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
          {storageFailure && <p className="sync-note" role="alert">{storageFailure}</p>}
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
        {storageFailure && <p className="sync-note" role="alert">{storageFailure}</p>}
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
