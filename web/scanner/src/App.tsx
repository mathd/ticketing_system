import { useEffect, useRef, useState } from 'react'
import { openOccurrenceStore, type OccurrenceStore } from './occurrences'
import './index.css'

type ScanOutcome =
  | { kind: 'accepted'; scannedAt: string; replay: boolean }
  | { kind: 'rejected'; reason: string; originalScanAt?: string }
  | { kind: 'queued' }
  | { kind: 'duplicate-response' }

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
    try {
      const response = await fetch(scanURL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ qr_payload: record.qrPayload, occurrence_id: record.occurrenceId, occurred_at: record.occurredAt }),
      })
      const result: { decision?: string; reason?: string; scanned_at?: string; original_scan_at?: string; replay?: boolean } = await response.json()
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
      } else {
        await store.markSynced(record.occurrenceId, result.reason ?? 'scan_failed')
        setOutcome({ kind: 'rejected', reason: result.reason ?? 'scan_failed', originalScanAt: result.original_scan_at })
      }
    } catch {
      // Offline: the occurrence stays durably queued and reconciles on sync.
      await store.markQueued(record.occurrenceId)
      setOutcome({ kind: 'queued' })
      await refreshQueued()
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
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          occurrences: queue.map((r) => ({ qr_payload: r.qrPayload, occurrence_id: r.occurrenceId, occurred_at: r.occurredAt })),
        }),
      })
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
      {outcome?.kind === 'queued' && <section className="result queued" role="status"><h2>Queued offline</h2><p>No connection — the scan is saved on this device and will sync when back online. Admit per venue offline policy.</p></section>}
      {outcome?.kind === 'duplicate-response' && <section className="result rejected" role="alert"><h2>Already processed</h2><p>This scan was already handled on this device — no second entry.</p></section>}
    </main>
  )
}

export default App
