// Local export of held and pre-upgrade queued records (ADR-085 §7.4, D25;
// TKT-558 D6, D15).
//
// Held rows keep their full stored fields, credentials included, because the
// operator has no other copy. Pre-upgrade QUEUED (M02) rows keep only their
// id, class, current state and claimed time. Plain JSON cannot tell an own
// undefined field from an absent field, so every stored field carries an
// explicit presence. A value that JSON cannot carry exactly stops the export:
// the export fails visibly and no receipt is written.

import type { DisposalSnapshot, ExportReceipt, HeldDisposal, LegacyClass } from './occurrence-store'

export const EXPORT_SCHEMA_VERSION = 1

export type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue }
export type FieldPresence =
  | { presence: 'absent' }
  | { presence: 'undefined' }
  | { presence: 'value'; value: JsonValue }

/** The fields that a stored occurrence row can have. Each is listed, also when absent. */
export const KNOWN_FIELDS = [
  'occurrenceId',
  'qrPayload',
  'occurredAt',
  'createdAt',
  'state',
  'actuated',
  'result',
  'ownerId',
  'leaseExpiresAt',
  'localDecision',
  'direction',
] as const

export type HeldExportEntry = {
  occurrenceId: JsonValue
  legacyClass: Extract<LegacyClass, 'M01' | 'unclassifiable'>
  holdReason: 'legacy_pending' | 'legacy_unclassifiable'
  format: 0
  type: 'normal' | 'refusal' | 'unknown'
  fields: Record<string, FieldPresence>
  disposal:
    | { status: 'unresolved' }
    | { status: 'acknowledged'; acknowledgedAt: string; disposition: HeldDisposal['disposition']; receiptId: string }
}

export type LegacyQueuedExportEntry = {
  occurrenceId: JsonValue
  legacyClass: 'M02'
  state: FieldPresence
  occurredAt: FieldPresence
}

export type ExportDocument = {
  schemaVersion: typeof EXPORT_SCHEMA_VERSION
  exportId: string
  exportedAt: string
  source: { databaseName: string; databaseVersion: number; legacyFormat: 0; currentFormat: 1 }
  held: HeldExportEntry[]
  legacyQueued: LegacyQueuedExportEntry[]
}

export type PreparedExport = { document: ExportDocument; contents: string; receipt: ExportReceipt }

/** A stored value that the export cannot represent exactly. */
export class UnsupportedExportValue extends Error {
  constructor(path: string) {
    super(`export cannot represent the stored value at ${path} exactly`)
    this.name = 'UnsupportedExportValue'
  }
}

const own = (value: object, key: string) => Object.prototype.hasOwnProperty.call(value, key)

function encodeValue(value: unknown, path: string): JsonValue {
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return value
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || Object.is(value, -0)) throw new UnsupportedExportValue(path)
    return value
  }
  if (Array.isArray(value)) {
    const encoded: JsonValue[] = []
    for (let index = 0; index < value.length; index += 1) {
      if (!own(value, String(index)) || value[index] === undefined) throw new UnsupportedExportValue(`${path}[${index}]`)
      encoded.push(encodeValue(value[index], `${path}[${index}]`))
    }
    return encoded
  }
  if (typeof value === 'object') {
    const prototype = Object.getPrototypeOf(value)
    if (prototype !== Object.prototype && prototype !== null) throw new UnsupportedExportValue(path)
    const encoded: { [key: string]: JsonValue } = {}
    for (const key of Object.keys(value).sort()) {
      const field = (value as Record<string, unknown>)[key]
      if (field === undefined) throw new UnsupportedExportValue(`${path}.${key}`)
      encoded[key] = encodeValue(field, `${path}.${key}`)
    }
    return encoded
  }
  throw new UnsupportedExportValue(path)
}

/** Encodes every own field of a stored row, plus each known field that is absent. */
export function encodeFields(raw: unknown): Record<string, FieldPresence> {
  const fields: Record<string, FieldPresence> = {}
  const record = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : undefined
  const names = new Set<string>([...KNOWN_FIELDS, ...(record ? Object.keys(record) : [])])
  for (const name of [...names].sort()) {
    if (!record || !own(record, name)) fields[name] = { presence: 'absent' }
    else if (record[name] === undefined) fields[name] = { presence: 'undefined' }
    else fields[name] = { presence: 'value', value: encodeValue(record[name], name) }
  }
  return fields
}

function presence(raw: unknown, name: string): FieldPresence {
  if (typeof raw !== 'object' || raw === null || !own(raw, name)) return { presence: 'absent' }
  const value = (raw as Record<string, unknown>)[name]
  return value === undefined ? { presence: 'undefined' } : { presence: 'value', value: encodeValue(value, name) }
}

function encodeKey(key: IDBValidKey): JsonValue {
  return encodeValue(key, 'occurrenceId')
}

/** The exact held row that an export receipt binds, as one stable string. */
export function encodeHeldSnapshot(key: IDBValidKey, raw: unknown): string {
  return JSON.stringify({ occurrenceId: encodeKey(key), fields: encodeFields(raw) })
}

function heldType(raw: unknown, legacyClass: LegacyClass): HeldExportEntry['type'] {
  if (legacyClass === 'unclassifiable' || typeof raw !== 'object' || raw === null) return 'unknown'
  const localDecision = (raw as Record<string, unknown>).localDecision
  if (localDecision === 'revocation_refused') return 'refusal'
  return own(raw, 'localDecision') ? 'unknown' : 'normal'
}

const byKey = <T extends { key: IDBValidKey }>(a: T, b: T) => indexedDB.cmp(a.key, b.key)

/**
 * Builds the export from one disposal snapshot. Throws UnsupportedExportValue
 * when a stored value cannot be represented exactly; then no receipt exists.
 */
export function prepareExport(
  snapshot: DisposalSnapshot,
  identity: { exportId: string; exportedAt: string; databaseName: string; databaseVersion: number },
): PreparedExport {
  const held = [...snapshot.held].sort(byKey)
  const legacyQueued = [...snapshot.legacyQueued].sort(byKey)
  const document: ExportDocument = {
    schemaVersion: EXPORT_SCHEMA_VERSION,
    exportId: identity.exportId,
    exportedAt: identity.exportedAt,
    source: { databaseName: identity.databaseName, databaseVersion: identity.databaseVersion, legacyFormat: 0, currentFormat: 1 },
    held: held.map((entry) => ({
      occurrenceId: encodeKey(entry.key),
      legacyClass: entry.legacyClass === 'M01' ? 'M01' : 'unclassifiable',
      holdReason: entry.legacyClass === 'M01' ? 'legacy_pending' : 'legacy_unclassifiable',
      format: 0,
      type: heldType(entry.raw, entry.legacyClass),
      fields: encodeFields(entry.raw),
      disposal: entry.disposal
        ? {
            status: 'acknowledged',
            acknowledgedAt: entry.disposal.acknowledgedAt,
            disposition: entry.disposal.disposition,
            receiptId: entry.disposal.receiptId,
          }
        : { status: 'unresolved' },
    })),
    legacyQueued: legacyQueued.map((entry) => ({
      occurrenceId: encodeKey(entry.key),
      legacyClass: 'M02',
      state: presence(entry.raw, 'state'),
      occurredAt: presence(entry.raw, 'occurredAt'),
    })),
  }
  const receipt: ExportReceipt = {
    receiptId: identity.exportId,
    exportedAt: identity.exportedAt,
    held: held.map((entry) => ({ occurrenceId: entry.key, snapshot: encodeHeldSnapshot(entry.key, entry.raw) })),
  }
  return { document, contents: `${JSON.stringify(document, null, 2)}\n`, receipt }
}

export type DisposalSummary = {
  heldCount: number
  unresolvedCount: number
  legacyQueuedCount: number
  /** Export stays available for any held row, acknowledged or not, and for any M02 row in any state. */
  exportAvailable: boolean
}

export function summarizeDisposal(snapshot: DisposalSnapshot): DisposalSummary {
  const unresolvedCount = snapshot.held.filter((entry) => !entry.disposal).length
  return {
    heldCount: snapshot.held.length,
    unresolvedCount,
    legacyQueuedCount: snapshot.legacyQueued.length,
    exportAvailable: snapshot.held.length > 0 || snapshot.legacyQueued.length > 0,
  }
}
