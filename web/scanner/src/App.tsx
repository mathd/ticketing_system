import { useEffect, useRef, useState } from 'react'
import './index.css'

type ScanOutcome =
  | { kind: 'accepted'; scannedAt: string }
  | { kind: 'rejected'; reason: string; originalScanAt?: string }

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

function readableTime(value?: string) {
  return value ? new Date(value).toLocaleString() : undefined
}

function App() {
  const [payload, setPayload] = useState('')
  const [outcome, setOutcome] = useState<ScanOutcome | null>(null)
  const [submitting, setSubmitting] = useState(false)
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
    return () => {
      mounted.current = false
      stopCamera()
    }
  }, [])

  const submit = async (value = payload) => {
    if (!value.trim() || submitting) return
    setSubmitting(true)
    setOutcome(null)
    try {
      const response = await fetch(scanURL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ qr_payload: value.trim() }),
      })
      const result: { decision?: string; reason?: string; scanned_at?: string; original_scan_at?: string } = await response.json()
      if (response.ok && result.decision === 'accepted') {
        setOutcome({ kind: 'accepted', scannedAt: result.scanned_at ?? '' })
      } else {
        setOutcome({ kind: 'rejected', reason: result.reason ?? 'scan_failed', originalScanAt: result.original_scan_at })
      }
    } catch {
      setOutcome({ kind: 'rejected', reason: 'scanner_unavailable' })
    } finally {
      setSubmitting(false)
    }
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
      </section>
      {outcome?.kind === 'accepted' && <section className="result accepted" role="status"><h2>Accepted</h2><p>Entry recorded at {readableTime(outcome.scannedAt)}.</p></section>}
      {outcome?.kind === 'rejected' && <section className="result rejected" role="alert"><h2>Rejected</h2><p>{outcome.reason === 'already_redeemed' ? `Already redeemed at ${readableTime(outcome.originalScanAt)}.` : 'Credential is invalid or cannot be redeemed.'}</p></section>}
    </main>
  )
}

export default App
