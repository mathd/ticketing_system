import { flushSync } from 'react-dom'
import { useEffect, useRef, useState } from 'react'
import { decodeReconcileResponse, type ReconcileResponse } from './access-contract'
import { createEntryScanner, type EntryResult, type Pairing } from './entry-scan'
import { pageStorage, type StorageReadiness } from './occurrence-readiness'
import { type QueuedEntry } from './occurrence-store'
import { planReconcileBatches } from './reconcile-batch'
import { OccurrenceRecords } from './OccurrenceRecords'
import { CANNOT_REPAIR, presentResult, readableTime, REPAIR_NEEDED, STORAGE_UNAVAILABLE, STORAGE_UNREADY } from './scan-result'

type BarcodeDetectorInstance = { detect(source: HTMLVideoElement): Promise<Array<{ rawValue?: string }>> }
type BarcodeDetectorConstructor = new (options: { formats: string[] }) => BarcodeDetectorInstance
declare global { interface Window { BarcodeDetector?: BarcodeDetectorConstructor } }

const reconcileURL = '/api/access/scans/reconciliations'
const revocationsURL = '/api/access/scans/voided-tickets'
export const reconcileTimeoutMs = 15_000
const deviceTokenKey = 'scanner.device-token'
const scannerTokenHeader = 'X-Scanner-Token'
const QUEUED_CAUSE = { offline: 'No connection', unreadable: 'The server answered but the reply could not be read' } as const
function readDeviceToken(): string {
  try { return localStorage.getItem(deviceTokenKey)?.trim() ?? '' } catch { return '' }
}
function scanHeaders(token: string): HeadersInit {
  return { 'Content-Type': 'application/json', [scannerTokenHeader]: token }
}
async function tokenFingerprint(token: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token))
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}
type SyncNote =
  | { kind: 'failure'; id: string; text: string }
  | { kind: 'success'; text: string }
  | { kind: 'pairing'; text: string; failure?: { id: string; text: string } }
  | null
function withoutStaleFailure(previous: SyncNote): SyncNote {
  if (previous?.kind === 'failure') return null
  if (previous?.kind === 'pairing' && previous.failure) return { kind: 'pairing', text: previous.text }
  return previous
}

function App() {
  const storage = useRef(pageStorage).current
  const [readiness, setReadiness] = useState<StorageReadiness>(storage.state)
  const [payload, setPayload] = useState('')
  const [outcome, setOutcome] = useState<EntryResult | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [queuedCount, setQueuedCount] = useState(0)
  const [syncNote, setSyncNote] = useState<SyncNote>(null)
  const [reconcileWarnings, setReconcileWarnings] = useState<string[]>([])
  const [cameraMessage, setCameraMessage] = useState('')
  const [cameraActive, setCameraActive] = useState(false)
  const [deviceToken, setDeviceToken] = useState(readDeviceToken)
  const [pairingInput, setPairingInput] = useState('')
  const [pairingBusy, setPairingBusy] = useState(false)
  const [storageFailure, setStorageFailure] = useState<string | null>(null)
  const [hasRevocationPull, setHasRevocationPull] = useState(false)
  const [recordsRefresh, setRecordsRefresh] = useState(0)
  const video = useRef<HTMLVideoElement>(null)
  const stream = useRef<MediaStream | null>(null)
  const frame = useRef<number | null>(null)
  const mounted = useRef(true)
  const lastSuccessfulSyncAt = useRef<string | null>(null)
  const syncFailureId = useRef(0)
  const syncInFlight = useRef(false)
  const syncRequested = useRef(false)
  const deviceTokenRef = useRef(deviceToken)
  const pairingGeneration = useRef(0)
  const pairingBusyRef = useRef(false)
  const pullInFlight = useRef<{ pairing: Pairing; promise: Promise<void> } | null>(null)
  const generation = useRef(0)
  const pairing = (): Pairing => ({ token: deviceTokenRef.current, generation: pairingGeneration.current })
  const isPairing = (captured: Pairing) => captured.token === deviceTokenRef.current && captured.generation === pairingGeneration.current
  const diagnostic = () => { if (mounted.current) setRecordsRefresh((value) => value + 1) }
  const scanner = useRef<ReturnType<typeof createEntryScanner> | null>(null)
  if (!scanner.current) scanner.current = createEntryScanner({ storage, pairing,
    publish: (result) => { if (mounted.current) flushSync(() => setOutcome(result)) },
    busy: (value) => { if (mounted.current) setSubmitting(value) }, diagnostic,
    refusalQueued: () => { if (mounted.current) void syncQueued() },
    authRefused: (captured) => clearPairing('This device is not paired. Enter its pairing token before admitting anyone.', captured, true),
  })

  const stopCamera = () => {
    generation.current += 1
    if (frame.current !== null) cancelAnimationFrame(frame.current)
    frame.current = null
    stream.current?.getTracks().forEach((track) => track.stop())
    stream.current = null
    setCameraActive(false)
  }
  const getStore = () => {
    const store = storage.readyStore()
    if (!store) throw new Error('scanner storage is not ready')
    return store
  }
  const reportStorageFailure = () => { if (mounted.current) setStorageFailure(STORAGE_UNAVAILABLE) }
  const reportStorageReady = () => { if (mounted.current && storage.readyStore()) setStorageFailure(null) }
  const refreshQueued = async () => {
    try {
      const store = getStore()
      await storage.coordinator.repairAll()
      await store.recoverDeadOwners()
      const queue = await store.queued()
      if (mounted.current) {
        setQueuedCount(queue.entries.length)
        reportStorageReady()
        diagnostic()
      }
    } catch { reportStorageFailure() }
  }

  useEffect(() => {
    mounted.current = true
    const update = (state: StorageReadiness) => {
      setReadiness(state)
      diagnostic()
      if (state.status !== 'ready') stopCamera()
      else {
        reportStorageReady()
        void refreshQueued()
        if (deviceTokenRef.current) void pullRevocations()
      }
    }
    const unsubscribe = storage.subscribe(update)
    update(storage.state())
    storage.start()
    const timer = window.setInterval(() => { if (deviceTokenRef.current) void pullRevocations() }, 15 * 60 * 1000)
    const onOnline = () => {
      if (deviceTokenRef.current) void pullRevocations()
      void syncQueued()
      void refreshQueued()
    }
    const onPairingChange = (event: StorageEvent) => {
      if (event.key !== deviceTokenKey || (event.newValue?.trim() ?? '') === deviceTokenRef.current) return
      scanner.current!.cancel()
      pairingGeneration.current += 1
      deviceTokenRef.current = event.newValue?.trim() ?? ''
      setDeviceToken(deviceTokenRef.current)
      setOutcome(null)
      setHasRevocationPull(false)
      stopCamera()
    }
    window.addEventListener('online', onOnline)
    window.addEventListener('storage', onPairingChange)
    return () => {
      mounted.current = false
      unsubscribe()
      window.clearInterval(timer)
      window.removeEventListener('online', onOnline)
      window.removeEventListener('storage', onPairingChange)
      scanner.current!.cancel()
      stopCamera()
    }
    // The page listeners read mutable refs. Subscribing again would cancel an active scan.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  // Pairing changes start a feed pull. The readiness subscription covers storage opens.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { if (deviceToken && storage.readyStore()) void pullRevocations() }, [deviceToken])

  const pullRevocations = async (): Promise<void> => {
    const captured = pairing()
    if (!captured.token || !storage.readyStore()) return
    const current = pullInFlight.current
    if (current && current.pairing.token === captured.token && current.pairing.generation === captured.generation) return current.promise
    const pull = (async () => {
      try {
        const store = getStore()
        const fingerprint = await tokenFingerprint(captured.token)
        const previous = await store.revocationPull(fingerprint)
        if (mounted.current && isPairing(captured)) setHasRevocationPull(!!previous.completedAt)
        let cursor: string | null = null
        do {
          if (!mounted.current || !isPairing(captured)) return
          const query = cursor === null ? '?limit=100' : `?limit=100&cursor=${encodeURIComponent(cursor)}`
          const response = await fetch(`${revocationsURL}${query}`, { headers: { [scannerTokenHeader]: captured.token } })
          if (response.status === 401) {
            clearPairing('This device is not paired. Enter its pairing token before admitting anyone.', captured)
            return
          }
          if (!response.ok) return
          const body: unknown = await response.json()
          if (!body || typeof body !== 'object' || !('ticket_ids' in body) || !('next_cursor' in body)) return
          const page = body as { ticket_ids?: unknown; next_cursor?: unknown }
          if (!Array.isArray(page.ticket_ids) || page.ticket_ids.some((id) => typeof id !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id))) return
          if (page.next_cursor !== null && (typeof page.next_cursor !== 'string' || !page.next_cursor)) return
          await store.mergeRevoked(page.ticket_ids as string[])
          cursor = page.next_cursor
        } while (cursor !== null)
        await store.completeRevocationPull(fingerprint, new Date().toISOString())
        if (mounted.current && isPairing(captured)) setHasRevocationPull(true)
      } catch { /* A failed pull never advances its completed timestamp. */ }
    })()
    const flight = { pairing: captured, promise: pull }
    pullInFlight.current = flight
    try { await pull } finally { if (pullInFlight.current === flight) pullInFlight.current = null }
  }
  const submit = async (value = payload) => {
    if (!value.trim()) return
    if (!submitting) setOutcome(null)
    await scanner.current!.submit(value)
    if (mounted.current) void refreshQueued()
  }

  const reportSyncFailure = async (cause: 'offline' | 'unreadable') => {
    const id = String(++syncFailureId.current)
    if (!mounted.current) return
    const last = (lastSuccessfulSyncAt.current ? readableTime(lastSuccessfulSyncAt.current) : undefined) ?? 'none'
    const failureText = (count: number) => `Sync failed: ${QUEUED_CAUSE[cause]}. ${count} scan${count === 1 ? ' is' : 's are'} saved on this device and will be sent on the next sync. Last successful sync this session: ${last}.`
    const failure = { id, text: `Sync failed: ${QUEUED_CAUSE[cause]}. Checking the scans saved on this device…` }
    setSyncNote((previous) => previous?.kind === 'pairing' ? { ...previous, failure } : { kind: 'failure', ...failure })
    const correctFailure = (previous: SyncNote, text?: string): SyncNote => {
      const current = previous?.kind === 'pairing' ? previous.failure : previous?.kind === 'failure' ? previous : null
      if (current?.id !== id) return previous
      if (!text) return withoutStaleFailure(previous)
      const corrected = { id, text }
      return previous?.kind === 'pairing' ? { ...previous, failure: corrected } : { kind: 'failure', ...corrected }
    }
    try {
      const queue = (await getStore().queued()).entries
      if (!mounted.current) return
      setQueuedCount(queue.length)
      setSyncNote((previous) => correctFailure(previous, queue.length ? failureText(queue.length) : undefined))
    } catch {
      // Leave the storage alert alone.
      if (!mounted.current) return
      const text = `Sync failed: ${QUEUED_CAUSE[cause]}. Any scans saved on this device will be sent on the next sync. Last successful sync this session: ${last}.`
      setSyncNote((previous) => correctFailure(previous, text))
    }
  }

  const syncQueued = async () => {
    if (!mounted.current) return
    if (syncInFlight.current) {
      syncRequested.current = true
      return
    }
    syncInFlight.current = true
    try {
      do {
        syncRequested.current = false
        await syncBatch()
      } while (syncRequested.current && mounted.current)
    } finally {
      syncInFlight.current = false
    }
  }


  const syncBatch = async () => {
    let queue: QueuedEntry[]
    try {
      const store = getStore()
      await storage.coordinator.repairAll()
      await store.recoverDeadOwners()
      queue = (await store.queued()).entries
      reportStorageReady()
      diagnostic()
    } catch { reportStorageFailure(); return }
    if (!queue.length) {
      if (mounted.current) { setQueuedCount(0); setSyncNote(withoutStaleFailure) }
      return
    }
    const plan = planReconcileBatches(queue)
    const warnings = plan.unsendable.map((row) => `Pre-upgrade record ${String(row.key)} could not sync. It remains queued and listed in the export.`)
    const captured = pairing()
    if (!captured.token) return
    const publishWarnings = () => {
      if (mounted.current && isPairing(captured)) setReconcileWarnings([...warnings])
    }
    publishWarnings()
    let recorded = 0
    let conflicts = 0
    for (const batch of plan.batches) {
      if (!mounted.current || !isPairing(captured)) return
      const controller = new AbortController()
      const timer = window.setTimeout(() => controller.abort(), reconcileTimeoutMs)
      let data: ReconcileResponse
      try {
        let response: Response
        try {
          response = await fetch(reconcileURL, { method: 'POST', headers: scanHeaders(captured.token),
            body: JSON.stringify({ occurrences: batch.occurrences }), signal: controller.signal })
        } catch { void reportSyncFailure('offline'); return }
        if (response.status === 401) {
          clearPairing('This device is not paired. Enter its pairing token to sync the queued scans.', captured)
          return
        }
        if (response.status === 422 && batch.isolated) {
          warnings.push(`Pre-upgrade record ${String(batch.keys[0])} could not sync. It remains queued and listed in the export.`)
          publishWarnings()
          continue
        }
        if (!response.ok) { void reportSyncFailure('unreadable'); return }
        try {
          data = decodeReconcileResponse(await response.json(), new Set(batch.keys.map(String)))
        } catch { void reportSyncFailure('unreadable'); return }
      } finally { window.clearTimeout(timer) }
      try {
        const applied = await getStore().applyReconcileResults(batch.keys, data.results)
        if (applied.status !== 'applied') { reportStorageFailure(); return }
      } catch { reportStorageFailure(); return }
      for (const result of data.results) {
        if (result.result === 'recorded' || result.result === 'synced') recorded += 1
        else if (result.result === 'conflict') conflicts += 1
        else warnings.push(`Reconciliation rejected for ${result.occurrence_id}. This result is not recorded.`)
      }
      publishWarnings()
    }
    if (mounted.current && isPairing(captured)) {
      setReconcileWarnings(warnings)
      lastSuccessfulSyncAt.current = new Date().toISOString()
      setSyncNote({ kind: 'success', text: `Synced ${recorded} offline scan(s).${conflicts ? ` ${conflicts} conflict${conflicts === 1 ? '' : 's'} flagged for the operator.` : ''}` })
    }
    await refreshQueued()
  }

  const startCamera = async () => {
    if (!storage.readyStore()) { setOutcome({ kind: 'storage-unready' }); return }
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


  const clearPairing = (reason: string, rejected: Pairing, retainResult = false) => {
    if (!isPairing(rejected)) return
    scanner.current!.cancel()
    pairingGeneration.current += 1
    try { if (localStorage.getItem(deviceTokenKey) === rejected.token) localStorage.removeItem(deviceTokenKey) } catch { /* In-memory invalidation still applies. */ }
    deviceTokenRef.current = ''
    setDeviceToken('')
    setHasRevocationPull(false)
    if (!retainResult) setOutcome(null)
    stopCamera()
    setSyncNote((previous) => {
      if (previous?.kind === 'failure') return { kind: 'pairing', text: reason, failure: { id: previous.id, text: previous.text } }
      if (previous?.kind === 'pairing' && previous.failure) return { kind: 'pairing', text: reason, failure: previous.failure }
      return { kind: 'pairing', text: reason }
    })
  }
  const pairDevice = async (event: React.FormEvent) => {
    event.preventDefault()
    const token = pairingInput.trim()
    if (!token || pairingBusyRef.current || !storage.readyStore()) return
    const captured = pairing()
    pairingBusyRef.current = true
    setPairingBusy(true)
    try {
      const fingerprint = await tokenFingerprint(token)
      const pull = await getStore().revocationPull(fingerprint)
      if (!mounted.current || !isPairing(captured)) return
      scanner.current!.cancel()
      pairingGeneration.current += 1
      deviceTokenRef.current = token
      try { localStorage.setItem(deviceTokenKey, token) } catch { /* Pair for this page session. */ }
      setDeviceToken(token)
      setHasRevocationPull(!!pull.completedAt)
      setPairingInput('')
      setOutcome(null)
      stopCamera()
      setSyncNote((previous) => {
        if (previous?.kind === 'failure') return previous
        if (previous?.kind === 'pairing' && previous.failure) return { kind: 'failure', ...previous.failure }
        return null
      })
    } catch { reportStorageFailure() } finally {
      pairingBusyRef.current = false
      if (mounted.current) setPairingBusy(false)
    }
  }
  const retryStorage = async () => {
    if (storage.readyStore()) {
      await storage.coordinator.repairAll()
      await refreshQueued()
    } else storage.retry()
    diagnostic()
  }
  const ready = readiness.status === 'ready'
  const warnings = storage.coordinator.repairWarnings()
  const result = outcome ? presentResult(outcome) : null
  return <main className="scanner">
    <h1>Gate scanner</h1>
    {!deviceToken ? <section aria-label="Device pairing">
      <h2>{outcome?.kind === 'auth-refused' ? 'Device pairing' : 'Pair this device'}</h2>
      <p>This scanner is not paired yet. An operator enrols it once with <code>access enrol-scanner</code> and reads out the token it prints.</p>
      <form aria-label="Device pairing" onSubmit={(event) => void pairDevice(event)}>
        <label htmlFor="pairing-token">Pairing token</label>
        <input id="pairing-token" type="password" value={pairingInput} onChange={(event) => setPairingInput(event.target.value)} autoComplete="off" placeholder="Paste the token from access enrol-scanner" />
        <button type="submit" disabled={!ready || pairingBusy || !pairingInput.trim()}>Pair device</button>
      </form>
      {queuedCount > 0 && <p className="queue-note" role="status">{queuedCount} offline scan{queuedCount === 1 ? '' : 's'} are still saved on this device and will sync once it is paired.</p>}
    </section> : <>
      <p>Scan a ticket QR code or paste its credential to validate entry.</p>
      <section aria-label="Ticket scan input">
        <form aria-label="Ticket scan" onSubmit={(event) => { event.preventDefault(); void submit() }}>
          <label htmlFor="qr-payload">Ticket credential</label>
          <textarea id="qr-payload" value={payload} onChange={(event) => setPayload(event.target.value)} placeholder="Paste QR credential" rows={4} />
          <div className="scanner-actions">
            <button type="submit" disabled={!ready || submitting || !payload.trim()}>Check ticket</button>
            <button type="button" onClick={() => void startCamera()} disabled={!ready || cameraActive || submitting}>Use camera</button>
            <button type="button" onClick={() => void pullRevocations()} disabled={!ready}>Refresh revocation list</button>
            {cameraActive && <button type="button" onClick={stopCamera}>Stop camera</button>}
          </div>
        </form>
        <video ref={video} aria-label="Camera preview" muted playsInline hidden={!cameraActive} />
        {cameraMessage && <p className="camera-note" role="status">{cameraMessage}</p>}
        {queuedCount > 0 && <p className="queue-note" role="status">{queuedCount} queued offline scan{queuedCount === 1 ? '' : 's'} awaiting sync.{' '}
          <button type="button" disabled={!ready} onClick={() => void syncQueued()}>Sync queued scans</button></p>}
      </section>
      {!hasRevocationPull && <p className="queue-note" role="status">This device holds no revocation list yet.</p>}
    </>}
    {submitting && !result && <p role="status">Checking...</p>}
    {result && <section aria-label="Scan result" className={`result ${result.tone}`} role={result.tone === 'rejected' ? 'alert' : 'status'}>
      {result.heading && <h2>{result.heading}</h2>}{result.lines.map((line) => <p key={line}>{line}</p>)}
    </section>}
    {syncNote && <p className="sync-note" role="status">{syncNote.text}{syncNote.kind === 'pairing' && syncNote.failure && <><br />{syncNote.failure.text}</>}</p>}
    {reconcileWarnings.map((warning) => <p role="alert" key={warning}>{warning}</p>)}
    {!ready && <p role="alert">{STORAGE_UNREADY}</p>}
    {(storageFailure || readiness.status === 'failed' || readiness.status === 'closed') && <p role="alert">{STORAGE_UNAVAILABLE}</p>}
    {warnings.some((warning) => warning.kind === 'write_needs_repair') && <p role="alert">{REPAIR_NEEDED}</p>}
    {warnings.some((warning) => warning.kind === 'cannot_repair') && <p role="alert">{CANNOT_REPAIR}</p>}
    {(!ready || storageFailure || warnings.length > 0) && <button type="button" onClick={() => void retryStorage()} disabled={readiness.status === 'draining'}>Retry storage</button>}
    <OccurrenceRecords storage={storage} refresh={recordsRefresh} />
  </main>
}
export default App
