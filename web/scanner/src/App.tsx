import { flushSync } from 'react-dom'
import { useEffect, useRef, useState } from 'react'
import { decodeReconcileResponse, type ReconcileResponse } from './access-contract'
import { createEntryScanner, type EntryResult, type Pairing, type ResultContext } from './entry-scan'
import { pageStorage, type StorageReadiness } from './occurrence-readiness'
import type { ScanDirection } from './occurrence-model'
import { type OccurrenceStore, type QueuedEntry } from './occurrence-store'
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
// The waiting line appears after this much initial storage work. It describes the wait only.
const waitingDelayMs = 1_000
const waitingText = 'Waiting for local storage to finish before this sync can continue.'
const pairingErrorText = 'Pairing could not be completed on this device. Submit the pairing token again to retry.'
const writeDiagnosticText = 'The last sync result could not be saved on this device, so that batch was not applied. Scans from that batch that are still queued will be retried on a later sync.'
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

type FailureCause = keyof typeof QUEUED_CAUSE
type FailureCount = { state: 'checking' } | { state: 'unknown' } | { state: 'known'; count: number }
// A carried failure keeps its identity across pairing changes. Only the observation that captured it can correct its count.
type CarriedFailure = { id: number; cause: FailureCause; last: string; count: FailureCount }
type SyncNote =
  | { kind: 'failure'; failure: CarriedFailure }
  | { kind: 'success'; text: string }
  | { kind: 'pairing'; text: string; failure?: CarriedFailure }
  | null
type BatchDiagnostic = 'write' | null
type QueueRead = Awaited<ReturnType<OccurrenceStore['queued']>>
// One mounted page component. Its callbacks publish only while it is live, and never into a newer one.
type Lifecycle = {
  live: boolean
  loop: { running: boolean; requested: boolean }
  controller?: AbortController
  attempt?: { id: number; timer: number }
}

const carriedFailure = (note: SyncNote): CarriedFailure | undefined =>
  note?.kind === 'failure' || note?.kind === 'pairing' ? note.failure : undefined
function failureText({ cause, last, count }: CarriedFailure): string {
  const lead = `Sync failed: ${QUEUED_CAUSE[cause]}.`
  if (count.state === 'checking') return `${lead} Checking the scans saved on this device…`
  if (count.state === 'unknown') return `${lead} Any scans saved on this device will be sent on the next sync. Last successful sync this session: ${last}.`
  return `${lead} ${count.count} scan${count.count === 1 ? ' is' : 's are'} saved on this device and will be sent on the next sync. Last successful sync this session: ${last}.`
}

function App() {
  const storage = useRef(pageStorage).current
  const [readiness, setReadiness] = useState<StorageReadiness>(storage.state)
  const [payload, setPayload] = useState('')
  const [outcome, setOutcome] = useState<{ result: EntryResult; context: ResultContext } | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const submittingRef = useRef(false)
  const [direction, setDirection] = useState<ScanDirection>('entry')
  const directionRef = useRef<ScanDirection>('entry')
  const [storageWarnings, setStorageWarnings] = useState<string[]>([])
  const [queuedCount, setQueuedCount] = useState(0)
  const [syncNote, setSyncNoteState] = useState<SyncNote>(null)
  const noteRef = useRef<SyncNote>(null)
  const [waiting, setWaiting] = useState(false)
  const [batchDiagnostic, setBatchDiagnostic] = useState<BatchDiagnostic>(null)
  const [reconcileWarnings, setReconcileWarnings] = useState<string[]>([])
  const [cameraMessage, setCameraMessage] = useState('')
  const [cameraActive, setCameraActive] = useState(false)
  const [deviceToken, setDeviceToken] = useState(readDeviceToken)
  const [pairingInput, setPairingInput] = useState('')
  const [pairingBusy, setPairingBusy] = useState(false)
  const [pairingError, setPairingError] = useState(false)
  const [storageFailure, setStorageFailure] = useState<string | null>(null)
  const [hasRevocationPull, setHasRevocationPull] = useState(false)
  const [recordsRefresh, setRecordsRefresh] = useState(0)
  const video = useRef<HTMLVideoElement>(null)
  const stream = useRef<MediaStream | null>(null)
  const frame = useRef<number | null>(null)
  const mounted = useRef(true)
  const lifecycle = useRef<Lifecycle>({ live: false, loop: { running: false, requested: false } })
  const lastSuccessfulSyncAt = useRef<string | null>(null)
  const failureSeq = useRef(0)
  const attemptSeq = useRef(0)
  const tickets = useRef({ issued: 0, published: 0 })
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
    publish: (result, context) => { if (mounted.current) flushSync(() => setOutcome({ result, context })) },
    busy: (value) => { submittingRef.current = value; if (mounted.current) setSubmitting(value) }, diagnostic,
    refusalQueued: () => { if (mounted.current) void syncQueued() },
    authRefused: (captured) => clearPairing('This device is not paired. Enter its pairing token before admitting anyone.', captured, true),
  })

  // The only writer of the sync note. The ref answers at once, so a correction can check its failure before it publishes.
  const updateNote = (next: (previous: SyncNote) => SyncNote) => {
    const value = next(noteRef.current)
    noteRef.current = value
    setSyncNoteState(value)
  }
  // Queue observations take tickets in invocation order. A result publishes only if no later one has published.
  const issueTicket = () => ++tickets.current.issued
  const claim = (ticket: number) => {
    if (ticket <= tickets.current.published) return false
    tickets.current.published = ticket
    return true
  }
  // A result publishes only for its own lifecycle and for the store that produced it.
  const applicable = (life: Lifecycle, store: OccurrenceStore | null) => !!store && life.live && storage.readyStore() === store
  const correctFailure = (id: number | undefined, count: FailureCount | null) => updateNote((previous) => {
    const failure = carriedFailure(previous)
    if (id === undefined || failure?.id !== id) return previous
    if (count === null) return previous?.kind === 'pairing' ? { kind: 'pairing', text: previous.text } : null
    const corrected = { ...failure, count }
    return previous?.kind === 'pairing' ? { ...previous, failure: corrected } : { kind: 'failure', failure: corrected }
  })
  // A committed nonempty pairing drops the pairing instruction. The failure keeps its identity and reads unknown until its refresh.
  const carryAfterPairing = () => updateNote((previous) => {
    const failure = carriedFailure(previous)
    return failure ? { kind: 'failure', failure: { ...failure, count: { state: 'unknown' } } } : null
  })
  // A checking count cannot outlive its storage. The live transition settles it, since a correction held on the old store publishes nothing.
  const settleCheckingFailure = () => {
    const failure = carriedFailure(noteRef.current)
    if (failure?.count.state === 'checking') correctFailure(failure.id, { state: 'unknown' })
  }

  const stopCamera = () => {
    generation.current += 1
    if (frame.current !== null) cancelAnimationFrame(frame.current)
    frame.current = null
    stream.current?.getTracks().forEach((track) => track.stop())
    stream.current = null
    setCameraActive(false)
  }
  const resetDirection = () => {
    stopCamera()
    setCameraMessage('')
    directionRef.current = 'entry'
    setDirection('entry')
  }
  const changeDirection = (next: ScanDirection) => {
    if (submittingRef.current || !storage.readyStore() || !deviceTokenRef.current) return
    stopCamera()
    setCameraMessage('')
    setOutcome(null)
    directionRef.current = next
    setDirection(next)
  }
  const publishStorageWarnings = (warnings: Array<{ key: IDBValidKey; problem: string }>) => {
    if (mounted.current) setStorageWarnings(warnings.map((warning) =>
      `Local record ${String(warning.key)} cannot be read: ${warning.problem}. It stays on this device and will not sync.`))
  }
  const getStore = () => {
    const store = storage.readyStore()
    if (!store) throw new Error('scanner storage is not ready')
    return store
  }
  // The App read-error source. Only an accepted ordinary read clears it. A readiness notification does not.
  const raiseReadError = (failure: number | undefined) => {
    if (mounted.current) setStorageFailure(STORAGE_UNAVAILABLE)
    correctFailure(failure, { state: 'unknown' })
  }
  const publishRead = (read: QueueRead, failure: number | undefined) => {
    publishStorageWarnings(read.warnings)
    setQueuedCount(read.entries.length)
    setStorageFailure(null)
    correctFailure(failure, read.entries.length ? { state: 'known', count: read.entries.length } : null)
    diagnostic()
  }
  // One queue observation: the storage prelude, then one queued() read. The prelude failure uses the failure captured at its start.
  // The read uses the failure captured at its invocation. Returns the entries the read saw, or undefined when the attempt ended.
  const observe = async (life: Lifecycle, store: OccurrenceStore | null): Promise<QueuedEntry[] | undefined> => {
    // An obsolete lifecycle or store starts no repair, recovery or read.
    if (store ? !applicable(life, store) : !life.live) return undefined
    if (!store) {
      const prelude = issueTicket()
      if (life.live && claim(prelude)) raiseReadError(carriedFailure(noteRef.current)?.id)
      return undefined
    }
    const prelude = issueTicket()
    const preludeFailure = carriedFailure(noteRef.current)?.id
    try {
      await storage.coordinator.repairAll()
      // A cleanup or store change during the repair wait stops the prelude here, so no recovery or read starts.
      if (!applicable(life, store)) return undefined
      await store.recoverDeadOwners()
      if (!applicable(life, store)) return undefined
    } catch {
      if (applicable(life, store) && claim(prelude)) raiseReadError(preludeFailure)
      return undefined
    }
    // The ticket is taken where the read is invoked. Nothing awaits between this check and that call.
    const ticket = issueTicket()
    const failure = carriedFailure(noteRef.current)?.id
    let read: QueueRead
    try {
      read = await store.queued()
    } catch {
      if (applicable(life, store) && claim(ticket)) raiseReadError(failure)
      return undefined
    }
    if (!applicable(life, store)) return undefined
    if (claim(ticket)) publishRead(read, failure)
    return read.entries
  }
  const refreshQueued = () => observe(lifecycle.current, storage.readyStore())

  useEffect(() => {
    mounted.current = true
    const life: Lifecycle = { live: true, loop: { running: false, requested: false } }
    lifecycle.current = life
    const update = (state: StorageReadiness) => {
      setReadiness(state)
      diagnostic()
      if (state.status !== 'ready') {
        stopCamera()
        settleCheckingFailure()
      }
      else {
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
    // Focus and a visible transition read the shared queue again. They never start a sync or a revocation pull.
    const onFocus = () => { void refreshQueued() }
    const onVisibility = () => { if (document.visibilityState === 'visible') void refreshQueued() }
    const onPairingChange = (event: StorageEvent) => {
      if (event.key !== deviceTokenKey || (event.newValue?.trim() ?? '') === deviceTokenRef.current) return
      resetDirection()
      scanner.current!.cancel()
      pairingGeneration.current += 1
      deviceTokenRef.current = event.newValue?.trim() ?? ''
      setDeviceToken(deviceTokenRef.current)
      setOutcome(null)
      setHasRevocationPull(false)
      // A replacement commits pairing. A removal leaves the carried failure as it is.
      if (deviceTokenRef.current) {
        setPairingError(false)
        carryAfterPairing()
      }
      void refreshQueued()
    }
    window.addEventListener('online', onOnline)
    window.addEventListener('focus', onFocus)
    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('storage', onPairingChange)
    return () => {
      mounted.current = false
      life.live = false
      life.loop.requested = false
      life.controller?.abort()
      if (life.attempt) window.clearTimeout(life.attempt.timer)
      life.attempt = undefined
      setWaiting(false)
      unsubscribe()
      window.clearInterval(timer)
      window.removeEventListener('online', onOnline)
      window.removeEventListener('focus', onFocus)
      document.removeEventListener('visibilitychange', onVisibility)
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
    await scanner.current!.submit(value, directionRef.current)
    if (mounted.current) void refreshQueued()
  }

  // A failed sync creates its failure, then corrects its count with one direct queue read. That read runs no repair or recovery.
  // Its result publishes only while the lifecycle and the store it was read from are both current.
  const reportSyncFailure = async (life: Lifecycle, store: OccurrenceStore, cause: FailureCause) => {
    if (!applicable(life, store)) return
    const id = ++failureSeq.current
    const last = (lastSuccessfulSyncAt.current ? readableTime(lastSuccessfulSyncAt.current) : undefined) ?? 'none'
    const failure: CarriedFailure = { id, cause, last, count: { state: 'checking' } }
    updateNote((previous) => previous?.kind === 'pairing' ? { ...previous, failure } : { kind: 'failure', failure })
    const ticket = issueTicket()
    const stillCurrent = () => applicable(life, store) && carriedFailure(noteRef.current)?.id === id
    try {
      const read = await store.queued()
      if (!stillCurrent() || !claim(ticket)) return
      publishStorageWarnings(read.warnings)
      setQueuedCount(read.entries.length)
      correctFailure(id, read.entries.length ? { state: 'known', count: read.entries.length } : null)
    } catch {
      // The correction never raises or clears the storage alert.
      if (!stillCurrent() || !claim(ticket)) return
      correctFailure(id, { state: 'unknown' })
    }
  }

  // The waiting line belongs to one attempt. Only that attempt's timer or finalizer can change it.
  const startAttempt = (life: Lifecycle) => {
    const id = ++attemptSeq.current
    const timer = window.setTimeout(() => { if (life.live && life.attempt?.id === id) setWaiting(true) }, waitingDelayMs)
    life.attempt = { id, timer }
    return id
  }
  const endAttempt = (life: Lifecycle, id: number) => {
    if (life.attempt?.id !== id) return
    window.clearTimeout(life.attempt.timer)
    life.attempt = undefined
    if (life.live) setWaiting(false)
  }

  // One pass of the serial sync owner. Pairing is read after the storage phase, so a replacement token is used for the batches.
  const syncPass = async (life: Lifecycle) => {
    const store = storage.readyStore()
    const attempt = startAttempt(life)
    let entries: QueuedEntry[] | undefined
    try {
      entries = await observe(life, store)
    } finally {
      endAttempt(life, attempt)
    }
    if (!store || !entries?.length) return
    const plan = planReconcileBatches(entries)
    const warnings = plan.unsendable.map((row) => `Pre-upgrade record ${String(row.key)} could not sync. It remains queued and listed in the export.`)
    const captured = pairing()
    if (!captured.token) return
    let recorded = 0
    let conflicts = 0
    let rejected = 0
    const aggregateText = () => `Synced ${recorded} offline scan(s).${conflicts ? ` ${conflicts} conflict${conflicts === 1 ? '' : 's'} flagged for the operator.` : ''}${rejected ? ` ${rejected} rejected scan${rejected === 1 ? ' was' : 's were'} not recorded.` : ''}`
    const publishWarnings = () => {
      if (life.live && isPairing(captured)) setReconcileWarnings([
        ...(recorded + conflicts + rejected > 0 ? [`Completed batches: ${aggregateText()}`] : []), ...warnings,
      ])
    }
    publishWarnings()
    for (const [index, batch] of plan.batches.entries()) {
      if (!applicable(life, store) || !isPairing(captured)) return
      const controller = new AbortController()
      const timer = window.setTimeout(() => controller.abort(), reconcileTimeoutMs)
      life.controller = controller
      // The first request of a nonempty paired pass replaces the write diagnostic before it is sent, even if the request fails.
      if (index === 0) setBatchDiagnostic(null)
      let data: ReconcileResponse | undefined
      try {
        let response: Response
        try {
          response = await fetch(reconcileURL, { method: 'POST', headers: scanHeaders(captured.token),
            body: JSON.stringify({ occurrences: batch.occurrences }), signal: controller.signal })
        } catch {
          if (applicable(life, store) && isPairing(captured)) void reportSyncFailure(life, store, 'offline')
          return
        }
        if (!applicable(life, store)) return
        if (response.status === 401) {
          if (isPairing(captured)) clearPairing('This device is not paired. Enter its pairing token to sync the queued scans.', captured)
          return
        }
        if (response.status === 422 && batch.isolated) {
          if (!isPairing(captured)) return
          warnings.push(`Pre-upgrade record ${String(batch.keys[0])} could not sync. It remains queued and listed in the export.`)
          publishWarnings()
          continue
        }
        if (!response.ok) {
          if (isPairing(captured)) void reportSyncFailure(life, store, 'unreadable')
          return
        }
        try {
          data = decodeReconcileResponse(await response.json(), new Set(batch.keys.map(String)))
        } catch {
          if (applicable(life, store) && isPairing(captured)) void reportSyncFailure(life, store, 'unreadable')
          return
        }
      } finally {
        window.clearTimeout(timer)
        if (life.controller === controller) life.controller = undefined
      }
      // A decoded response applies even if pairing changed. Only a replaced lifecycle or store stops it.
      if (!data || !applicable(life, store)) return
      let applied: Awaited<ReturnType<OccurrenceStore['applyReconcileResults']>>
      try {
        applied = await store.applyReconcileResults(batch.keys, data.results)
      } catch {
        // A thrown transaction or write applied nothing. Only the current pairing reports it.
        if (applicable(life, store) && isPairing(captured)) setBatchDiagnostic('write')
        return
      }
      // A started application commits atomically. Its outcome publishes only while its lifecycle and store are current.
      if (!applicable(life, store)) return
      if (applied.status === 'invalid') {
        if (isPairing(captured)) void reportSyncFailure(life, store, 'unreadable')
        return
      }
      // A refused batch changes no row and adds no report. The refresh shows the rows the store holds now.
      if (applied.status === 'incompatible') {
        await observe(life, store)
        return
      }
      if (!isPairing(captured)) {
        // The batch applied under a replaced pairing. It reports nothing new, but the device queue still refreshes.
        await observe(life, store)
        return
      }
      for (const result of data.results) {
        if (result.result === 'recorded' || result.result === 'synced') recorded += 1
        else if (result.result === 'conflict') conflicts += 1
        else {
          rejected += 1
          warnings.push(`Reconciliation rejected for ${result.occurrence_id}. This result is not recorded.`)
        }
      }
      publishWarnings()
    }
    if (!applicable(life, store)) return
    if (isPairing(captured)) {
      setReconcileWarnings(warnings)
      lastSuccessfulSyncAt.current = new Date().toISOString()
      updateNote(() => ({ kind: 'success', text: aggregateText() }))
    }
    await observe(life, store)
  }

  // Requests share one loop per lifecycle. A request made during a pass sets one flag, and that flag runs one follow-up.
  const syncQueued = async () => {
    const life = lifecycle.current
    if (!life.live) return
    if (life.loop.running) {
      life.loop.requested = true
      return
    }
    life.loop.running = true
    try {
      do {
        life.loop.requested = false
        await syncPass(life)
      } while (life.loop.requested && life.live)
    } finally {
      life.loop.running = false
    }
  }

  const startCamera = async () => {
    if (!storage.readyStore()) { setOutcome({ result: { kind: 'storage-unready' }, context: Object.freeze({ direction: directionRef.current, requestMayHaveBeenSent: false }) }); return }
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
    resetDirection()
    scanner.current!.cancel()
    pairingGeneration.current += 1
    try { if (localStorage.getItem(deviceTokenKey) === rejected.token) localStorage.removeItem(deviceTokenKey) } catch { /* In-memory invalidation still applies. */ }
    deviceTokenRef.current = ''
    setDeviceToken('')
    setHasRevocationPull(false)
    if (!retainResult) setOutcome(null)
    updateNote((previous) => {
      const failure = carriedFailure(previous)
      return failure ? { kind: 'pairing', text: reason, failure } : { kind: 'pairing', text: reason }
    })
  }
  const pairDevice = async (event: React.FormEvent) => {
    event.preventDefault()
    const token = pairingInput.trim()
    if (!token || pairingBusyRef.current || !storage.readyStore()) return
    const captured = pairing()
    const life = lifecycle.current
    pairingBusyRef.current = true
    setPairingBusy(true)
    try {
      const fingerprint = await tokenFingerprint(token)
      const pull = await getStore().revocationPull(fingerprint)
      if (!life.live || !isPairing(captured)) return
      resetDirection()
      scanner.current!.cancel()
      pairingGeneration.current += 1
      deviceTokenRef.current = token
      try { localStorage.setItem(deviceTokenKey, token) } catch { /* Pair for this page session. */ }
      setDeviceToken(token)
      setHasRevocationPull(!!pull.completedAt)
      setPairingInput('')
      setOutcome(null)
      setPairingError(false)
      carryAfterPairing()
      void refreshQueued()
    } catch {
      // A failed fingerprint or metadata read ends this pairing attempt only. It is not a storage outage.
      if (life.live && isPairing(captured)) setPairingError(true)
    } finally {
      pairingBusyRef.current = false
      if (life.live) setPairingBusy(false)
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
  const result = outcome ? presentResult(outcome.result, outcome.context) : null
  return <main className="scanner">
    <h1>Gate scanner</h1>
    <fieldset className="scan-direction" disabled={!deviceToken || !ready || submitting}>
      <legend>Scan direction</legend>
      <label><input type="radio" name="direction" value="entry" checked={direction === 'entry'} onChange={() => changeDirection('entry')} /> Entry</label>
      <label><input type="radio" name="direction" value="exit" checked={direction === 'exit'} onChange={() => changeDirection('exit')} /> Exit</label>
    </fieldset>
    {!deviceToken ? <section aria-label="Device pairing">
      <h2>{outcome?.result.kind === 'auth-refused' ? 'Device pairing' : 'Pair this device'}</h2>
      <p>This scanner is not paired yet. An operator enrols it once with <code>access enrol-scanner</code> and reads out the token it prints.</p>
      <form aria-label="Device pairing" onSubmit={(event) => void pairDevice(event)}>
        <label htmlFor="pairing-token">Pairing token</label>
        <input id="pairing-token" type="password" value={pairingInput} onChange={(event) => setPairingInput(event.target.value)} autoComplete="off" placeholder="Paste the token from access enrol-scanner" />
        <button type="submit" disabled={!ready || pairingBusy || !pairingInput.trim()}>Pair device</button>
      </form>
      {pairingError && <p role="alert">{pairingErrorText}</p>}
      {queuedCount > 0 && <p className="queue-note" role="status">{queuedCount} offline scan{queuedCount === 1 ? '' : 's'} are still saved on this device and will sync once it is paired.</p>}
    </section> : <>
      <p>Scan a ticket QR code or paste its credential {direction === 'entry' ? 'to validate entry.' : 'to record Exit.'}</p>
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
    {syncNote && <p className="sync-note" role="status">{syncNote.kind === 'failure' ? failureText(syncNote.failure) : syncNote.text}{syncNote.kind === 'pairing' && syncNote.failure && <><br />{failureText(syncNote.failure)}</>}</p>}
    {waiting && <p role="status">{waitingText}</p>}
    {storageWarnings.map((warning) => <p role="alert" key={warning}>{warning}</p>)}
    {reconcileWarnings.map((warning) => <p role="alert" key={warning}>{warning}</p>)}
    {batchDiagnostic && <p role="alert">{writeDiagnosticText}</p>}
    {!ready && <p role="alert">{STORAGE_UNREADY}</p>}
    {(storageFailure || readiness.status === 'failed' || readiness.status === 'closed') && <p role="alert">{STORAGE_UNAVAILABLE}</p>}
    {warnings.some((warning) => warning.kind === 'write_needs_repair') && <p role="alert">{REPAIR_NEEDED}</p>}
    {warnings.some((warning) => warning.kind === 'cannot_repair') && <p role="alert">{CANNOT_REPAIR}</p>}
    {(!ready || storageFailure || warnings.length > 0) && <button type="button" onClick={() => void retryStorage()} disabled={readiness.status === 'draining'}>Retry storage</button>}
    <OccurrenceRecords storage={storage} refresh={recordsRefresh} />
  </main>
}
export default App
