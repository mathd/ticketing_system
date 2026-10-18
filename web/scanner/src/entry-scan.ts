import { decodeScanResponse, type ScanRequest } from './access-contract'
import type { CapturedOccurrence } from './occurrence-coordinator'
import type { OfflineStatus, TerminalOutcome } from './occurrence-model'
import type { StorageController } from './occurrence-readiness'
import { decodeTicketID } from './revocation'
import { sendScan } from './scan-transport'

export type Pairing = Readonly<{ token: string; generation: number }>
export type EntryResult =
  | { kind: 'accepted'; scannedAt: string; replay: boolean }
  | { kind: 'rejected'; reason: string; originalScanAt?: string }
  | { kind: 'queued'; status?: OfflineStatus }
  | { kind: 'cancelled'; requestMayHaveBeenSent: boolean }
  | { kind: 'duplicate' | 'mark-failed' | 'auth-refused' | 'denial-unreadable' | 'server-error'
      | 'status-unconfirmed' | 'body-unconfirmed' | 'local-failure' | 'storage-unready'
      | 'mint-unconfirmed' | 'revocation-refused' }

export type EntryScannerOptions = {
  storage: StorageController
  pairing(): Pairing
  publish(result: EntryResult): void
  busy(value: boolean): void
  diagnostic(): void
  authRefused(pairing: Pairing): void
}

type Execution = {
  pairing: Pairing
  captured: CapturedOccurrence
  registered: boolean
  sent: boolean
  cancelled: boolean
  published: boolean
  abort: AbortController
}

/** A scan keeps its identity and pairing across every asynchronous continuation. */
export function createEntryScanner(options: EntryScannerOptions) {
  const { storage } = options
  const coordinator = storage.coordinator
  let active: Execution | undefined
  const current = (execution: Execution) => {
    const pairing = options.pairing()
    return execution.pairing.generation === pairing.generation && execution.pairing.token === pairing.token
  }
  const publish = (execution: Execution, result: EntryResult) => {
    if (execution.published || !current(execution) || (execution.cancelled && result.kind !== 'cancelled')) return
    execution.published = true
    options.publish(result)
  }
  const persist = (execution: Execution) => coordinator.persist(execution.captured.occurrenceId)
    .then(() => { options.diagnostic() })
  const terminal = (execution: Execution, outcome: TerminalOutcome, result: EntryResult) => {
    const final = coordinator.latch({ kind: 'terminal', occurrenceId: execution.captured.occurrenceId,
      outcome, decidedAt: new Date().toISOString() })
    if (final.kind === 'terminal' && final.outcome.cause === outcome.cause) publish(execution, result)
    return persist(execution)
  }
  const cancelExecution = (execution: Execution) => {
    execution.cancelled = true
    execution.abort.abort()
    if (execution.registered) {
      const outcome = { family: 'terminal' as const, cause: 'cancelled' as const, requestMayHaveBeenSent: execution.sent }
      void terminal(execution, outcome, { kind: 'cancelled', requestMayHaveBeenSent: execution.sent })
    }
  }
  return {
    cancel() {
      if (!active) return
      cancelExecution(active)
      active = undefined
      options.busy(false)
    },
    async submit(value: string) {
      if (active || !value.trim()) return
      const pairing = Object.freeze({ ...options.pairing() })
      if (!pairing.token) return
      const store = storage.readyStore()
      if (!store) {
        options.publish({ kind: 'storage-unready' })
        return
      }
      const occurredAt = new Date().toISOString()
      const execution: Execution = { pairing, registered: false, sent: false, cancelled: false, published: false,
        captured: { occurrenceId: crypto.randomUUID(), qrPayload: value.trim(), occurredAt,
          createdAt: occurredAt, type: 'normal' }, abort: new AbortController() }
      active = execution
      options.busy(true)
      const id = execution.captured.occurrenceId
      let listed = false
      let accepted = false
      try {
        try {
          const ticketId = decodeTicketID(execution.captured.qrPayload)
          listed = !!ticketId && await store.isRevoked(ticketId)
          if (!current(execution) || execution.cancelled) return
          execution.captured = Object.freeze({ ...execution.captured, type: listed ? 'refusal' : 'normal' })
          coordinator.register(execution.captured)
          execution.registered = true
          if (listed) await store.mintRefusal(id)
          else await store.mintNormal(id)
        } catch {
          if (listed) {
            if (execution.registered) coordinator.latch({ kind: 'queue', occurrenceId: id,
              outcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: new Date().toISOString() })
            publish(execution, { kind: 'revocation-refused' })
            if (execution.registered) await persist(execution)
          } else if (!execution.cancelled) {
            if (execution.registered) await terminal(execution, { family: 'terminal', cause: 'mint_unconfirmed' }, { kind: 'mint-unconfirmed' })
            else publish(execution, { kind: 'mint-unconfirmed' })
          }
          options.diagnostic()
          return
        }
        if (!current(execution) || execution.cancelled) {
          cancelExecution(execution)
          return
        }
        if (listed) {
          coordinator.latch({ kind: 'queue', occurrenceId: id,
            outcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: new Date().toISOString() })
          publish(execution, { kind: 'revocation-refused' })
          await persist(execution)
          return
        }
        const body: ScanRequest = { qr_payload: execution.captured.qrPayload, occurrence_id: id,
          occurred_at: execution.captured.occurredAt, direction: 'entry' }
        const request = new Request(new URL('/api/access/scans', window.location.href), {
          method: 'POST', redirect: 'manual', signal: execution.abort.signal,
          headers: { 'Content-Type': 'application/json', 'X-Scanner-Token': pairing.token }, body: JSON.stringify(body),
        })
        if (!current(execution) || execution.cancelled) { cancelExecution(execution); return }
        execution.sent = true
        const transport = await sendScan(request)
        if (!current(execution) || execution.cancelled || transport.kind === 'cancelled') { cancelExecution(execution); return }
        if (transport.kind === 'no-response') {
          coordinator.latch({ kind: 'queue', occurrenceId: id, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: new Date().toISOString() })
          publish(execution, { kind: 'queued' })
          await persist(execution)
          return
        }
        const response = transport.response
        const status = response.status
        if (status === 401) {
          const writing = terminal(execution, { family: 'terminal', cause: 'auth_refused' }, { kind: 'auth-refused' })
          if (current(execution)) options.authRefused(pairing)
          await writing
          return
        }
        if (status === 409 || status === 422) {
          coordinator.latch({ kind: 'terminal', occurrenceId: id, decidedAt: new Date().toISOString(),
            outcome: { family: 'terminal', cause: 'denied', status, latchId: crypto.randomUUID(), reason: null } })
          // Status protection starts before reading a possibly stalled body.
          void persist(execution)
          try {
            const result = decodeScanResponse(status, await response.json())
            if (execution.cancelled || !current(execution)) return
            if (result.decision !== 'rejected') throw new Error('not a denial')
            coordinator.captureDenialReason(id, result.reason)
            publish(execution, { kind: 'rejected', reason: result.reason,
              ...(result.original_scan_at === undefined ? {} : { originalScanAt: result.original_scan_at }) })
            void persist(execution)
          } catch {
            publish(execution, { kind: 'denial-unreadable' })
          }
          return
        }
        if (status === 502 || status === 503 || status === 504) {
          coordinator.latch({ kind: 'queue', occurrenceId: id,
            outcome: { family: 'queue', cause: 'server_unavailable', status }, decidedAt: new Date().toISOString() })
          publish(execution, { kind: 'queued', status })
          await persist(execution)
          return
        }
        if (status !== 200) {
          const serverError = status === 500 || status === 501 || (status >= 505 && status <= 599)
          await terminal(execution, { family: 'terminal', cause: serverError ? 'unconfirmed_server_error' : 'unconfirmed_status', status },
            { kind: serverError ? 'server-error' : 'status-unconfirmed' })
          return
        }
        let result
        try { result = decodeScanResponse(status, await response.json()) } catch {
          await terminal(execution, { family: 'terminal', cause: 'unconfirmed_accepted_body' }, { kind: 'body-unconfirmed' })
          return
        }
        if (result.decision !== 'accepted') throw new Error('not accepted')
        accepted = true
        if (execution.cancelled || !current(execution)) { cancelExecution(execution); return }
        if (!coordinator.beginMarking(id)) return
        const mark = await (storage.readyStore() ?? store).reserveActuation(id)
        if (execution.cancelled || !current(execution)) { cancelExecution(execution); return }
        if (mark.reserved) publish(execution, { kind: 'accepted', scannedAt: result.scanned_at, replay: result.replay === true })
        else if (mark.why === 'own_existing_actuation') publish(execution, { kind: result.replay === true ? 'duplicate' : 'mark-failed' })
        else await terminal(execution, { family: 'terminal', cause: 'mark_failure' }, { kind: 'mark-failed' })
      } catch {
        if (execution.cancelled || !current(execution)) { cancelExecution(execution); return }
        // An exception never supplies permission to create an offline fact.
        if (execution.registered && !coordinator.latched(id)) {
          await terminal(execution, accepted ? { family: 'terminal', cause: 'mark_failure' }
            : { family: 'terminal', cause: 'local_failure', requestMayHaveBeenSent: execution.sent },
          { kind: accepted ? 'mark-failed' : 'local-failure' })
        }
        options.diagnostic()
      } finally {
        if (active === execution) {
          active = undefined
          options.busy(false)
        }
      }
    },
  }
}
