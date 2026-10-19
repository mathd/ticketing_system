import { decodeScanResponse, type ScanRequest } from './access-contract'
import type { CapturedOccurrence } from './occurrence-coordinator'
import type { OfflineStatus, TerminalOutcome } from './occurrence-model'
import type { StorageController } from './occurrence-readiness'
import { decodeTicketID } from './revocation'
import { captureScanClock, checkScanClock, productionScanClock, productionScanScheduler,
  type ScanClock, type ScanReading, type ScanScheduler } from './scan-clock'
import { sendScan } from './scan-transport'

export type Pairing = Readonly<{ token: string; generation: number }>
export type EntryResult =
  | { kind: 'accepted'; scannedAt: string; replay: boolean }
  | { kind: 'rejected'; reason: string; originalScanAt?: string }
  | { kind: 'queued'; status?: OfflineStatus }
  | { kind: 'cancelled'; requestMayHaveBeenSent: boolean }
  | { kind: 'duplicate' | 'mark-failed' | 'auth-refused' | 'denial-unreadable' | 'server-error'
      | 'status-unconfirmed' | 'body-unconfirmed' | 'local-failure' | 'storage-unready'
      | 'mint-unconfirmed' | 'revocation-refused' | 'stopped-before-send' | 'sent-unconfirmed'
      | 'accepted-too-late' | 'revocation-audit-not-saved' }

export type EntryScannerOptions = {
  storage: StorageController
  pairing(): Pairing
  publish(result: EntryResult): void
  busy(value: boolean): void
  diagnostic(): void
  authRefused(pairing: Pairing): void
  refusalQueued?(): void
  clock?: ScanClock
  scheduler?: ScanScheduler
}

type Stop = 'cancelled' | 'deadline' | 'clock' | 'failure'
type Execution = {
  pairing: Pairing
  captured: CapturedOccurrence
  start: ScanReading
  registered: boolean
  sent: boolean
  stopped: boolean
  published: boolean
  listed: boolean
  accepted: boolean
  status?: number
  abort: AbortController
  interrupt?: () => void
}
const stoppedWait = new Error('scan stopped')

/** A scan keeps its identity, clocks and pairing across asynchronous continuations. */
export function createEntryScanner(options: EntryScannerOptions) {
  const { storage } = options
  const coordinator = storage.coordinator
  const clock = options.clock ?? productionScanClock
  const scheduler = options.scheduler ?? productionScanScheduler
  let active: Execution | undefined
  const current = (execution: Execution) => {
    const pairing = options.pairing()
    return execution.pairing.generation === pairing.generation && execution.pairing.token === pairing.token
  }
  const release = (execution: Execution) => {
    if (active === execution) { active = undefined; options.busy(false) }
  }
  const publish = (execution: Execution, result: EntryResult) => {
    if (execution.published) return
    execution.published = true
    // Release only this submission. A prior scan's persistence cannot release a newer one.
    release(execution)
    if (current(execution)) options.publish(result)
  }
  const persist = (execution: Execution) => {
    void coordinator.persist(execution.captured.occurrenceId).then(options.diagnostic, options.diagnostic)
  }
  const terminal = (execution: Execution, outcome: TerminalOutcome, result: EntryResult) => {
    if (execution.registered) coordinator.latch({ kind: 'terminal', occurrenceId: execution.captured.occurrenceId,
      outcome, decidedAt: new Date().toISOString() })
    publish(execution, result)
    if (execution.registered) persist(execution)
  }
  const stop = (execution: Execution, reason: Stop) => {
    if (execution.stopped || execution.published) return
    execution.stopped = true
    const checkpoint = execution.sent ? 'decision' as const : 'send' as const
    const outcome: TerminalOutcome = reason === 'cancelled'
      ? { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: execution.sent }
      : { family: 'terminal', cause: 'stopped', checkpoint, reason, requestMayHaveBeenSent: execution.sent }
    if (reason === 'cancelled') terminal(execution, outcome, { kind: 'cancelled', requestMayHaveBeenSent: execution.sent })
    else if (execution.listed) terminal(execution, outcome, { kind: 'revocation-audit-not-saved' })
    else if (execution.status === 409 || execution.status === 422) {
      // Its unknown denial was latched before starting the body read.
      publish(execution, { kind: 'denial-unreadable' }); persist(execution)
    } else if (execution.accepted) terminal(execution,
      reason === 'failure' ? { family: 'terminal', cause: 'mark_failure' } : outcome,
      { kind: reason === 'failure' ? 'mark-failed' : 'accepted-too-late' })
    else if (execution.status === 200) terminal(execution,
      { family: 'terminal', cause: 'unconfirmed_accepted_body' }, { kind: 'body-unconfirmed' })
    else if (reason === 'failure') terminal(execution,
      { family: 'terminal', cause: 'local_failure', requestMayHaveBeenSent: execution.sent }, { kind: 'local-failure' })
    else terminal(execution, outcome, { kind: execution.sent ? 'sent-unconfirmed' : 'stopped-before-send' })
    // The decision and instruction precede cleanup. Abort is never offline authority.
    execution.abort.abort()
    execution.interrupt?.()
  }
  const checkpoint = (execution: Execution, limit: 3000 | 30000) => {
    if (execution.stopped || execution.published) return undefined
    if (!current(execution)) { stop(execution, 'cancelled'); return undefined }
    try {
      const check = checkScanClock(execution.start, captureScanClock(clock), limit)
      if (check.ok) return check.remaining
      stop(execution, check.reason)
    } catch { stop(execution, 'failure') }
    return undefined
  }
  const wait = <T>(execution: Execution, pending: Promise<T>, limit: 3000 | 30000): Promise<T> => new Promise((resolve, reject) => {
    let timer: unknown
    let finished = false
    const finish = (settle: () => void) => {
      if (finished) return
      finished = true
      scheduler.clear(timer)
      execution.interrupt = undefined
      settle()
    }
    execution.interrupt = () => finish(() => reject(stoppedWait))
    const arm = () => {
      if (finished) return
      const remaining = checkpoint(execution, limit)
      if (remaining === undefined) { finish(() => reject(stoppedWait)); return }
      timer = scheduler.set(arm, remaining)
    }
    // Observe both settlements even after interruption. The store separately reports
    // transaction completion to the coordinator; this promise cannot invent it.
    pending.then((value) => finish(() => resolve(value)), (error: unknown) => finish(() => reject(error)))
    arm()
  })
  const queue = (execution: Execution, status?: OfflineStatus) => {
    if (checkpoint(execution, execution.listed ? 3000 : 30000) === undefined) return
    coordinator.latch({ kind: 'queue', occurrenceId: execution.captured.occurrenceId,
      outcome: execution.listed ? { family: 'queue', cause: 'local_refusal' }
        : status === undefined ? { family: 'queue', cause: 'transport_failure' }
          : { family: 'queue', cause: 'server_unavailable', status }, decidedAt: new Date().toISOString() })
    publish(execution, execution.listed ? { kind: 'revocation-refused' }
      : status === undefined ? { kind: 'queued' } : { kind: 'queued', status })
    persist(execution)
    if (execution.listed && current(execution)) options.refusalQueued?.()
  }
  return {
    cancel() { if (active) stop(active, 'cancelled') },
    async submit(value: string) {
      if (active || !value.trim()) return
      const pairing = Object.freeze({ ...options.pairing() })
      if (!pairing.token) return
      const store = storage.readyStore()
      if (!store) { options.publish({ kind: 'storage-unready' }); return }
      let start: ScanReading
      try { start = captureScanClock(clock) } catch { options.publish({ kind: 'local-failure' }); return }
      const occurredAt = new Date().toISOString()
      const execution: Execution = { pairing, start, registered: false, sent: false, stopped: false, published: false,
        listed: false, accepted: false, captured: { occurrenceId: crypto.randomUUID(), qrPayload: value.trim(),
          occurredAt, createdAt: occurredAt, type: 'normal' }, abort: new AbortController() }
      active = execution; options.busy(true)
      const id = execution.captured.occurrenceId
      try {
        if (checkpoint(execution, 3000) === undefined) return
        const ticketId = decodeTicketID(execution.captured.qrPayload)
        execution.listed = !!ticketId && await wait(execution, store.isRevoked(ticketId), 3000)
        if (execution.stopped) return
        if (checkpoint(execution, 3000) === undefined) return
        execution.captured = Object.freeze({ ...execution.captured, type: execution.listed ? 'refusal' : 'normal' })
        coordinator.register(execution.captured); execution.registered = true
        try {
          await wait(execution, execution.listed ? store.mintRefusal(id) : store.mintNormal(id), 3000)
        } catch (error) {
          if (execution.stopped) return
          if (execution.listed) stop(execution, 'failure')
          else terminal(execution, { family: 'terminal', cause: 'mint_unconfirmed' }, { kind: 'mint-unconfirmed' })
          if (error !== stoppedWait) options.diagnostic()
          return
        }
        if (execution.stopped) return
        if (execution.listed) { queue(execution); return }
        const body: ScanRequest = { qr_payload: execution.captured.qrPayload, occurrence_id: id,
          occurred_at: execution.captured.occurredAt, direction: 'entry' }
        const request = new Request(new URL('/api/access/scans', window.location.href), {
          method: 'POST', redirect: 'manual', signal: execution.abort.signal,
          headers: { 'Content-Type': 'application/json', 'X-Scanner-Token': pairing.token }, body: JSON.stringify(body),
        })
        if (checkpoint(execution, 3000) === undefined) return
        execution.sent = true
        const transport = await wait(execution, sendScan(request), 30000)
        if (execution.stopped) return
        if (!current(execution) || transport.kind === 'cancelled') { stop(execution, 'cancelled'); return }
        if (transport.kind === 'no-response') { queue(execution); return }
        const response = transport.response
        const status = response.status
        execution.status = status
        if (status === 401) {
          terminal(execution, { family: 'terminal', cause: 'auth_refused' }, { kind: 'auth-refused' })
          if (current(execution)) options.authRefused(pairing)
          return
        }
        if (status === 409 || status === 422) {
          coordinator.latch({ kind: 'terminal', occurrenceId: id, decidedAt: new Date().toISOString(),
            outcome: { family: 'terminal', cause: 'denied', status, latchId: crypto.randomUUID(), reason: null } })
          persist(execution)
          if (checkpoint(execution, 30000) === undefined) return
          try {
            const result = decodeScanResponse(status, await wait(execution, response.json(), 30000))
            if (execution.stopped || checkpoint(execution, 30000) === undefined) return
            if (result.decision !== 'rejected') throw new Error('not a denial')
            coordinator.captureDenialReason(id, result.reason)
            publish(execution, { kind: 'rejected', reason: result.reason,
              ...(result.original_scan_at === undefined ? {} : { originalScanAt: result.original_scan_at }) })
            persist(execution)
          } catch { if (!execution.stopped) { publish(execution, { kind: 'denial-unreadable' }); persist(execution) } }
          return
        }
        if (status === 502 || status === 503 || status === 504) { queue(execution, status); return }
        if (status !== 200) {
          const serverError = status === 500 || status === 501 || (status >= 505 && status <= 599)
          terminal(execution, { family: 'terminal', cause: serverError ? 'unconfirmed_server_error' : 'unconfirmed_status', status },
            { kind: serverError ? 'server-error' : 'status-unconfirmed' })
          return
        }
        if (checkpoint(execution, 30000) === undefined) return
        let result
        try { result = decodeScanResponse(status, await wait(execution, response.json(), 30000)) } catch {
          if (!execution.stopped) terminal(execution, { family: 'terminal', cause: 'unconfirmed_accepted_body' }, { kind: 'body-unconfirmed' })
          return
        }
        if (execution.stopped) return
        if (result.decision !== 'accepted') throw new Error('not accepted')
        execution.accepted = true
        if (checkpoint(execution, 30000) === undefined) return
        if (!coordinator.beginMarking(id)) return
        const mark = await wait(execution, (storage.readyStore() ?? store).reserveActuation(id), 30000)
        if (execution.stopped || checkpoint(execution, 30000) === undefined) return
        if (mark.reserved) publish(execution, { kind: 'accepted', scannedAt: result.scanned_at, replay: result.replay === true })
        else if (mark.why === 'own_existing_actuation') publish(execution, { kind: result.replay === true ? 'duplicate' : 'mark-failed' })
        else terminal(execution, { family: 'terminal', cause: 'mark_failure' }, { kind: 'mark-failed' })
      } catch {
        if (!execution.stopped && !execution.published) stop(execution, 'failure')
        options.diagnostic()
      } finally { release(execution) }
    },
  }
}
