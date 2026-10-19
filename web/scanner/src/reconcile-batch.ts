// Reconcile request batches (ADR-085 §7.4, owner D31; TKT-558 D12, D17).
//
// New rows send their captured type: a normal row sends event_type entry, a
// refusal sends local_decision only. A pre-upgrade QUEUED row (M02) syncs as
// main did, with its literal stored values. Its direction follows the legacy
// rules below, never a nullish or truthy fallback. A row whose wire fields are
// not all strings would make Access refuse the whole request, so it goes alone
// in its own request; a refusal of that request keeps it queued and leaves the
// other requests unaffected. A value that JSON cannot carry exactly is not sent.

import type { QueuedEntry } from './occurrence-store'

/** The one nonempty invalid direction for a legacy row whose own direction is not a nonempty string. */
export const LEGACY_INVALID_DIRECTION = 'invalid_legacy_direction'
/** The ReconcileRequest maxItems bound in services/access/api/openapi.yaml. */
export const RECONCILE_BATCH_LIMIT = 100

export type WireRow = Record<string, unknown>
export type ReconcileBatch = {
  /** The stored keys of the rows in this request, in request order. */
  keys: IDBValidKey[]
  occurrences: WireRow[]
  /** True when the request holds one row that failed the ordinary format checks. */
  isolated: boolean
}
export type UnsendableRow = { key: IDBValidKey; reason: string }
export type ReconcilePlan = { batches: ReconcileBatch[]; unsendable: UnsendableRow[] }

const own = (value: object, key: string) => Object.prototype.hasOwnProperty.call(value, key)

/** True when JSON carries the value exactly: plain data of strings, finite numbers, booleans and null. */
function jsonExact(value: unknown, ancestors = new Set<object>()): boolean {
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return true
  if (typeof value === 'number') return Number.isFinite(value) && !Object.is(value, -0)
  if (typeof value === 'object') {
    if (ancestors.has(value)) return false
    ancestors.add(value)
    try {
      if (Array.isArray(value)) {
        for (let index = 0; index < value.length; index += 1) {
          if (!own(value, String(index)) || !jsonExact(value[index], ancestors)) return false
        }
        return true
      }
      const prototype = Object.getPrototypeOf(value)
      if (prototype !== Object.prototype && prototype !== null) return false
      return Object.values(value as Record<string, unknown>).every((field) => field !== undefined && jsonExact(field, ancestors))
    } finally {
      ancestors.delete(value)
    }
  }
  return false
}

/** The wire value of a legacy normal row's direction, or undefined to omit event_type. */
export function legacyEventType(raw: Record<string, unknown>): string | undefined {
  if (!own(raw, 'direction')) return undefined
  const direction = raw.direction
  if (typeof direction === 'string' && direction !== '') return direction
  return LEGACY_INVALID_DIRECTION
}

type Planned = { key: IDBValidKey; row: WireRow; isolate: boolean } | { key: IDBValidKey; unsendable: string }

function planLegacy(key: IDBValidKey, raw: Record<string, unknown>): Planned {
  const row: WireRow = {}
  for (const [field, wire] of [['qrPayload', 'qr_payload'], ['occurrenceId', 'occurrence_id'], ['occurredAt', 'occurred_at']] as const) {
    if (!own(raw, field) || raw[field] === undefined) return { key, unsendable: `${field} has no value` }
    if (!jsonExact(raw[field])) return { key, unsendable: `${field} cannot be sent exactly` }
    row[wire] = raw[field]
  }
  // Main sent local_decision for any truthy stored value and never sent a type with it.
  if (raw.localDecision) {
    if (!jsonExact(raw.localDecision)) return { key, unsendable: 'localDecision cannot be sent exactly' }
    row.local_decision = raw.localDecision
  } else {
    const eventType = legacyEventType(raw)
    if (eventType !== undefined) row.event_type = eventType
  }
  const isolate = Object.values(row).some((value) => typeof value !== 'string')
  return { key, row, isolate }
}

function planCurrent(entry: Extract<QueuedEntry, { format: 1 }>): Planned {
  const { record } = entry
  const row: WireRow = {
    qr_payload: record.qrPayload,
    occurrence_id: record.occurrenceId,
    occurred_at: record.occurredAt,
  }
  if (record.localDecision === 'revocation_refused') row.local_decision = record.localDecision
  else row.event_type = record.direction
  return { key: entry.key, row, isolate: false }
}

export function planReconcileBatches(entries: readonly QueuedEntry[], limit = RECONCILE_BATCH_LIMIT): ReconcilePlan {
  const plan: ReconcilePlan = { batches: [], unsendable: [] }
  let shared: ReconcileBatch | undefined
  for (const entry of entries) {
    const planned = entry.format === 1 ? planCurrent(entry) : planLegacy(entry.key, entry.raw)
    if ('unsendable' in planned) {
      plan.unsendable.push({ key: planned.key, reason: planned.unsendable })
      continue
    }
    if (planned.isolate) {
      plan.batches.push({ keys: [planned.key], occurrences: [planned.row], isolated: true })
      continue
    }
    if (!shared || shared.occurrences.length >= limit) {
      shared = { keys: [], occurrences: [], isolated: false }
      plan.batches.push(shared)
    }
    shared.keys.push(planned.key)
    shared.occurrences.push(planned.row)
  }
  return plan
}
