import type { components } from './access-api-types.gen'

export type ScanRequest = components['schemas']['ScanRequest']
export type ReconcileRequest = components['schemas']['ReconcileRequest']
export type ScanResponse =
  | components['schemas']['ScanAccepted']
  | components['schemas']['ScanRejected']
export type ReconcileResponse = components['schemas']['ReconcileResponse']
type ReconcileResult = components['schemas']['ReconcileResult']
type AccessError = components['schemas']['Error']

const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})[tT](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[zZ]|[+-](\d{2}):(\d{2}))$/

function object(value: unknown, name: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${name} is not an object`)
  }
  return value as Record<string, unknown>
}

function string(value: unknown, name: string): string {
  if (typeof value !== 'string' || value === '') throw new Error(`${name} is missing`)
  return value
}

function dateTime(value: unknown, name: string): string {
  const text = string(value, name)
  const match = RFC3339.exec(text)
  if (!match) throw new Error(`${name} is not an RFC 3339 date-time`)

  const [, yearText, monthText, dayText, hourText, minuteText, secondText, offsetHourText, offsetMinuteText] = match
  const year = Number(yearText)
  const month = Number(monthText)
  const day = Number(dayText)
  const hour = Number(hourText)
  const minute = Number(minuteText)
  const second = Number(secondText)
  const offsetHour = offsetHourText === undefined ? 0 : Number(offsetHourText)
  const offsetMinute = offsetMinuteText === undefined ? 0 : Number(offsetMinuteText)
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  if (
    month < 1 || month > 12 ||
    day < 1 || day > days[month - 1] ||
    hour > 23 || minute > 59 || second > 59 ||
    offsetHour > 23 || offsetMinute > 59
  ) {
    throw new Error(`${name} is not an RFC 3339 date-time`)
  }
  return text
}

function optionalBoolean(value: unknown, name: string): boolean | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'boolean') throw new Error(`${name} is not a boolean`)
  return value
}

function optionalDateTime(value: unknown, name: string): string | undefined {
  return value === undefined ? undefined : dateTime(value, name)
}

export function decodeAccessError(value: unknown): AccessError {
  const body = object(value, 'access error')
  return { error: string(body.error, 'error') }
}

export function decodeScanResponse(status: number, value: unknown): ScanResponse {
  const body = object(value, 'scan response')
  if (status === 200) {
    if (body.decision !== 'accepted') throw new Error('scan response decision is not accepted')
    const replay = optionalBoolean(body.replay, 'replay')
    return {
      decision: 'accepted',
      scanned_at: dateTime(body.scanned_at, 'scanned_at'),
      ...(replay === undefined ? {} : { replay }),
    }
  }
  if (status === 409 || status === 422) {
    if (body.decision !== 'rejected') throw new Error('scan response decision is not rejected')
    const originalScanAt = optionalDateTime(body.original_scan_at, 'original_scan_at')
    return {
      decision: 'rejected',
      reason: string(body.reason, 'reason'),
      ...(originalScanAt === undefined ? {} : { original_scan_at: originalScanAt }),
    }
  }
  throw new Error(`status ${status} has no scan response contract`)
}

export function decodeReconcileResponse(
  value: unknown,
  expectedOccurrenceIds: ReadonlySet<string>,
): ReconcileResponse {
  const body = object(value, 'reconciliation response')
  if (!Array.isArray(body.results)) throw new Error('reconciliation response is missing results')

  const seen = new Set<string>()
  const results: ReconcileResult[] = body.results.map((raw) => {
    const entry = object(raw, 'reconciliation result')
    const occurrenceId = string(entry.occurrence_id, 'occurrence_id')
    if (!expectedOccurrenceIds.has(occurrenceId)) {
      throw new Error('reconciliation result names an occurrence that was not submitted')
    }
    if (seen.has(occurrenceId)) throw new Error('reconciliation response repeats an occurrence')
    seen.add(occurrenceId)

    const result = entry.result
    if (result !== 'recorded' && result !== 'conflict' && result !== 'synced' && result !== 'rejected') {
      throw new Error('reconciliation result is outside the contract')
    }
    const occurredAt = optionalDateTime(entry.occurred_at, 'occurred_at')
    const skewFlagged = optionalBoolean(entry.skew_flagged, 'skew_flagged')
    return {
      occurrence_id: occurrenceId,
      result,
      ...(occurredAt === undefined ? {} : { occurred_at: occurredAt }),
      ...(skewFlagged === undefined ? {} : { skew_flagged: skewFlagged }),
    }
  })
  if (seen.size !== expectedOccurrenceIds.size) {
    throw new Error('reconciliation response omits a submitted occurrence')
  }
  return { results }
}
