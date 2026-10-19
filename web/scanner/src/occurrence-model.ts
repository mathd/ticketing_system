// Local outcome model for scan occurrences (ADR-085 §2.2).
//
// The durable record keeps the five state names of ADR-025 §3. A tagged local
// outcome says why a decided row exists. These tags stay on this device: they
// are never sent to Access. Old or damaged rows bypass TypeScript, so the store
// reads every current-format row through readCurrentRecord.

export const RECORD_FORMAT = 1 as const

export type OccurrenceState = 'PENDING' | 'ACTUATED' | 'QUEUED' | 'SYNCED' | 'RESOLVED'
export const OCCURRENCE_STATES: readonly OccurrenceState[] = ['PENDING', 'ACTUATED', 'QUEUED', 'SYNCED', 'RESOLVED']

export type ScanType = 'normal' | 'refusal'
export type ReconcileResult = 'recorded' | 'synced' | 'conflict' | 'rejected'
export const RECONCILE_RESULTS: readonly ReconcileResult[] = ['recorded', 'synced', 'conflict', 'rejected']
export type OfflineStatus = 502 | 503 | 504
export type DenialStatus = 409 | 422

/** A queue decision makes a row reconcilable. */
export type QueueOutcome =
  | { family: 'queue'; cause: 'transport_failure' }
  | { family: 'queue'; cause: 'server_unavailable'; status: OfflineStatus }
  | { family: 'queue'; cause: 'local_refusal' }
  | { family: 'queue'; cause: 'uncertain_recovery' }

/** A terminal decision never reaches reconciliation. */
export type TerminalOutcome =
  /** R01-R17. A null reason is the unknown reason of R17 or of a denial before its body. */
  | { family: 'terminal'; cause: 'denied'; status: DenialStatus; latchId: string; reason: string | null }
  | { family: 'terminal'; cause: 'auth_refused' }
  | { family: 'terminal'; cause: 'unconfirmed_accepted_body' }
  | { family: 'terminal'; cause: 'unconfirmed_server_error'; status: number }
  | { family: 'terminal'; cause: 'unconfirmed_status'; status: number }
  | { family: 'terminal'; cause: 'local_failure'; requestMayHaveBeenSent: boolean }
  | { family: 'terminal'; cause: 'mark_failure' }
  | { family: 'terminal'; cause: 'cancelled'; requestMayHaveBeenSent: boolean }
  | { family: 'terminal'; cause: 'mint_unconfirmed' }
  | { family: 'terminal'; cause: 'stopped'; checkpoint: 'send' | 'decision'; reason: 'deadline' | 'clock' | 'failure'; requestMayHaveBeenSent: boolean }

/** The own-row actuation reservation. It does not prove that the gate opened. */
export type ActuationOutcome = { family: 'actuation'; cause: 'reserved' }

export type LocalOutcome = QueueOutcome | TerminalOutcome | ActuationOutcome

type RecordBase = {
  occurrenceId: string
  qrPayload: string
  occurredAt: string
  createdAt: string
  state: OccurrenceState
  actuated: boolean
  result?: ReconcileResult
  ownerId: string
  /** Epoch milliseconds. Advisory: an expired lease alone never proves page death. */
  leaseExpiresAt: number
  recordFormat: typeof RECORD_FORMAT
  localOutcome?: LocalOutcome
  /** The device-claimed decision time. Absent on PENDING and on uncertain recovery. */
  decidedAt?: string
}
export type NormalRecord = RecordBase & { direction: 'entry'; localDecision?: never }
export type RefusalRecord = RecordBase & { localDecision: 'revocation_refused'; direction?: never }
export type CurrentRecord = NormalRecord | RefusalRecord

export type RecordRead = { ok: true; record: CurrentRecord } | { ok: false; problem: string }

export function scanType(record: CurrentRecord): ScanType {
  return record.localDecision === 'revocation_refused' ? 'refusal' : 'normal'
}

const own = (value: object, key: string) => Object.prototype.hasOwnProperty.call(value, key)
const nonEmptyString = (value: unknown): value is string => typeof value === 'string' && value !== ''
const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

function queueOutcomeProblem(outcome: Record<string, unknown>, type: ScanType): string | null {
  switch (outcome.cause) {
    case 'transport_failure':
      return type === 'normal' ? null : 'a refusal cannot have a transport queue cause'
    case 'server_unavailable':
      if (type !== 'normal') return 'a refusal cannot have a server queue cause'
      return outcome.status === 502 || outcome.status === 503 || outcome.status === 504
        ? null
        : 'a server queue cause needs status 502, 503 or 504'
    case 'local_refusal':
      return type === 'refusal' ? null : 'a normal scan cannot have a refusal queue cause'
    case 'uncertain_recovery':
      return null
    default:
      return 'unknown queue cause'
  }
}

function terminalOutcomeProblem(outcome: Record<string, unknown>): string | null {
  const status = outcome.status
  switch (outcome.cause) {
    case 'denied':
      if (status !== 409 && status !== 422) return 'a denial needs status 409 or 422'
      if (!nonEmptyString(outcome.latchId)) return 'a denial needs its latch id'
      return outcome.reason === null || nonEmptyString(outcome.reason) ? null : 'a denial reason is null or a nonempty string'
    case 'auth_refused':
    case 'unconfirmed_accepted_body':
    case 'mark_failure':
    case 'mint_unconfirmed':
      return null
    case 'unconfirmed_server_error':
      return typeof status === 'number' && (status === 500 || status === 501 || (status >= 505 && status <= 599))
        ? null
        : 'a server error needs status 500, 501 or 505-599'
    case 'unconfirmed_status':
      return Number.isInteger(status) && !(typeof status === 'number' && status >= 500 && status <= 599)
        && status !== 200 && status !== 401 && status !== 409 && status !== 422
        ? null
        : 'an unconfirmed status cannot be a classified status'
    case 'stopped':
      if (outcome.checkpoint !== 'send' && outcome.checkpoint !== 'decision') return 'unknown stop checkpoint'
      if (!['deadline', 'clock', 'failure'].includes(outcome.reason as string)) return 'unknown stop reason'
      if (outcome.checkpoint === 'send' && outcome.requestMayHaveBeenSent !== false) return 'send stop cannot have sent a request'
      return typeof outcome.requestMayHaveBeenSent === 'boolean' ? null : 'request progress is required'
    case 'local_failure':
    case 'cancelled':
      return typeof outcome.requestMayHaveBeenSent === 'boolean' ? null : 'request progress is required'
    default:
      return 'unknown terminal cause'
  }
}

/** Validates a stored row as a current-format record. A failed read is a local storage warning, never a legacy row. */
export function readCurrentRecord(raw: unknown): RecordRead {
  const fail = (problem: string): RecordRead => ({ ok: false, problem })
  if (!isObject(raw)) return fail('not an object')
  if (raw.recordFormat !== RECORD_FORMAT) return fail('not a current-format record')
  if (!nonEmptyString(raw.occurrenceId)) return fail('occurrence id is missing')
  if (!nonEmptyString(raw.qrPayload)) return fail('credential is missing')
  if (!nonEmptyString(raw.occurredAt) || !nonEmptyString(raw.createdAt)) return fail('claimed times are missing')
  if (!OCCURRENCE_STATES.includes(raw.state as OccurrenceState)) return fail('unknown state')
  if (typeof raw.actuated !== 'boolean') return fail('actuated flag is missing')
  if (!nonEmptyString(raw.ownerId)) return fail('owner is missing')
  if (typeof raw.leaseExpiresAt !== 'number' || !Number.isFinite(raw.leaseExpiresAt)) return fail('lease is missing')

  let type: ScanType
  if (own(raw, 'direction') && !own(raw, 'localDecision') && raw.direction === 'entry') type = 'normal'
  else if (own(raw, 'localDecision') && !own(raw, 'direction') && raw.localDecision === 'revocation_refused') type = 'refusal'
  else return fail('record type is not exactly one of entry or refusal')

  const state = raw.state as OccurrenceState
  if (state === 'PENDING') {
    if (own(raw, 'localOutcome') || own(raw, 'decidedAt') || own(raw, 'result')) return fail('a pending row has no decision')
    if (raw.actuated) return fail('a pending row is not actuated')
    return { ok: true, record: raw as CurrentRecord }
  }

  if (!isObject(raw.localOutcome)) return fail('a decided row needs its outcome')
  const outcome = raw.localOutcome
  const recovered = outcome.family === 'queue' && outcome.cause === 'uncertain_recovery'
  if (recovered ? own(raw, 'decidedAt') : !nonEmptyString(raw.decidedAt)) {
    return fail(recovered ? 'a recovered row has no original decision time' : 'a decided row needs its decision time')
  }
  const result = raw.result
  const hasResult = own(raw, 'result')

  if (state === 'ACTUATED') {
    if (type !== 'normal') return fail('a refusal cannot actuate')
    if (raw.actuated !== true) return fail('an actuated row is marked actuated')
    if (outcome.family !== 'actuation' || outcome.cause !== 'reserved') return fail('an actuated row needs the reservation outcome')
    return hasResult ? fail('an actuated row has no reconcile result') : { ok: true, record: raw as CurrentRecord }
  }
  if (raw.actuated) return fail('only an actuated row is marked actuated')

  if (outcome.family === 'queue') {
    const problem = queueOutcomeProblem(outcome, type)
    if (problem) return fail(problem)
    if (state === 'QUEUED') return hasResult ? fail('a queued row has no reconcile result') : { ok: true, record: raw as CurrentRecord }
    if (state === 'SYNCED') {
      return result === 'recorded' || result === 'synced' ? { ok: true, record: raw as CurrentRecord } : fail('a synced row needs recorded or synced')
    }
    return result === 'conflict' || result === 'rejected' ? { ok: true, record: raw as CurrentRecord } : fail('a reconciled RESOLVED row needs conflict or rejected')
  }

  if (outcome.family === 'terminal') {
    if (state !== 'RESOLVED') return fail('a terminal outcome needs RESOLVED')
    if (type === 'refusal' && !((outcome.cause === 'cancelled' || (outcome.cause === 'stopped' && outcome.checkpoint === 'send')) && outcome.requestMayHaveBeenSent === false)) {
      return fail('a refusal permits only a pre-send terminal stop')
    }
    if (hasResult) return fail('a terminal row has no reconcile result')
    const problem = terminalOutcomeProblem(outcome)
    return problem ? fail(problem) : { ok: true, record: raw as CurrentRecord }
  }
  return fail('the outcome does not match the state')
}

/** Structural equality for outcome tags. The tags are small plain objects. */
export function sameOutcome(a: LocalOutcome, b: LocalOutcome): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)])
  for (const key of keys) {
    if ((a as Record<string, unknown>)[key] !== (b as Record<string, unknown>)[key]) return false
  }
  return true
}
