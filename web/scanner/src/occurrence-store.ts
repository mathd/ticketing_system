// Durable occurrence store, database version 3 (ADR-085 §2, §7; ADR-025 §3).
//
// Every write is one guarded IndexedDB transaction. A write result follows the
// transaction's complete or abort event, never request success. The store
// borrows the page coordinator: it reads the latched decision and the scan
// phase from it and reports mint and mark settlement to it. The store never
// owns the page owner lock and never releases it.
//
// Version 3 is a storage barrier. The version-2 reader cannot open this
// database, so it never sees a new-format row. The upgrade stamps every
// existing row with a legacy class from its stored state only. It does not
// change the row.

import type { DecisionWriter, FinalDecision, PageCoordinator, TerminalDecision, TransitionResult } from './occurrence-coordinator'
import { encodeHeldSnapshot } from './occurrence-export'
import {
  RECONCILE_RESULTS,
  RECORD_FORMAT,
  readCurrentRecord,
  sameOutcome,
  scanType,
  type CurrentRecord,
  type ReconcileResult,
  type ScanType,
} from './occurrence-model'

export const OCCURRENCE_DB = 'gate-occurrences'
export const OCCURRENCE_DB_VERSION = 3
export const PENDING_LEASE_MS = 30_000
export const LEASE_RENEWAL_MS = 10_000

export const STORE = {
  occurrences: 'occurrences',
  revocations: 'revocations',
  revocationMeta: 'revocation_meta',
  legacyClasses: 'legacy_classes',
  recoveryNotices: 'recovery_notices',
  heldDisposals: 'held_disposals',
  exportReceipts: 'export_receipts',
} as const
export const REQUIRED_STORES: readonly string[] = Object.values(STORE)

/**
 * M01: pre-upgrade PENDING, held. M02: pre-upgrade QUEUED, syncs as before.
 * other: pre-upgrade ACTUATED, SYNCED or RESOLVED, kept in its state.
 * unclassifiable: a valid key with an unknown state, held.
 */
export type LegacyClass = 'M01' | 'M02' | 'other' | 'unclassifiable'
export type LegacyClassRecord = { occurrenceId: IDBValidKey; legacyClass: LegacyClass; sourceVersion: 1 | 2 }
export const HELD_CLASSES: readonly LegacyClass[] = ['M01', 'unclassifiable']

export type RecoveryNotice = {
  occurrenceId: string
  cause: 'uncertain_recovery'
  recoveredAt: string
  acknowledgedAt?: string
}
export type NoticeView = {
  occurrenceId: string
  occurredAt: string | undefined
  type: ScanType | undefined
  cause: 'uncertain_recovery'
  syncState: string
}

export type HeldDisposal = {
  occurrenceId: IDBValidKey
  disposition: 'operator_acknowledged_without_sync'
  acknowledgedAt: string
  receiptId: string
}
export type ExportReceipt = {
  receiptId: string
  exportedAt: string
  /** The exact held rows the export contained, in its field-presence encoding. */
  held: Array<{ occurrenceId: IDBValidKey; snapshot: string }>
}

export type QueuedEntry =
  | { format: 1; key: string; record: CurrentRecord }
  | { format: 0; key: IDBValidKey; legacyClass: 'M02'; raw: Record<string, unknown> }
export type StorageWarning = { key: IDBValidKey; problem: string }

export type DisposalSnapshot = {
  held: Array<{ key: IDBValidKey; legacyClass: LegacyClass; raw: unknown; disposal: HeldDisposal | undefined }>
  legacyQueued: Array<{ key: IDBValidKey; raw: unknown }>
}

export type RecoveryReport = {
  recovered: string[]
  liveOwner: string[]
  notEligible: string[]
  failed: Array<{ occurrenceId: string; error: unknown }>
}

export type ActuationResult =
  | { reserved: true }
  | { reserved: false; why: 'own_existing_actuation' | 'missing' | 'foreign' | 'refusal' | 'ineligible_state' | 'not_marking' | 'invalid_record' }

export type ReconcileApplyResult =
  | { status: 'applied'; retired: number; alreadyApplied: number }
  | { status: 'invalid'; why: string }
  | { status: 'incompatible'; key: IDBValidKey }

export type AcknowledgeResult =
  | { status: 'acknowledged'; count: number }
  | { status: 'refused'; why: string }

export type StoreMode = 'initializing' | 'ready' | 'draining' | 'closed'

/** A write that did not commit. phase says where it failed. */
export class StorageWriteError extends Error {
  readonly operation: string
  readonly phase: 'create' | 'body' | 'abort' | 'inactive'
  constructor(operation: string, phase: 'create' | 'body' | 'abort' | 'inactive', cause: unknown) {
    super(`${operation}: transaction ${phase === 'create' ? 'could not start' : 'did not commit'}`, { cause })
    this.name = 'StorageWriteError'
    this.operation = operation
    this.phase = phase
  }
}

const STRICT: IDBTransactionOptions = { durability: 'strict' }
const DEFAULT: IDBTransactionOptions = { durability: 'default' }

export function requestDone<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

type TransactionHooks = { onComplete?: () => void; onAbort?: () => void }

/**
 * Runs body inside one transaction and resolves with its value only after the
 * complete event. The observers are attached when the transaction is created.
 * A body failure aborts the transaction, so a half-written body never commits.
 */
function runTransaction<T>(
  db: IDBDatabase,
  operation: string,
  storeNames: string | string[],
  mode: IDBTransactionMode,
  options: IDBTransactionOptions | undefined,
  body: (tx: IDBTransaction) => Promise<T> | T,
  hooks: TransactionHooks = {},
): Promise<T> {
  let tx: IDBTransaction
  try {
    tx = options ? db.transaction(storeNames, mode, options) : db.transaction(storeNames, mode)
  } catch (cause) {
    hooks.onAbort?.()
    return Promise.reject(new StorageWriteError(operation, 'create', cause))
  }
  return new Promise<T>((resolve, reject) => {
    let outcome: { ok: true; value: T } | { ok: false; error: unknown } | undefined
    tx.oncomplete = () => {
      hooks.onComplete?.()
      if (outcome?.ok) resolve(outcome.value)
      else reject(new StorageWriteError(operation, outcome ? 'body' : 'inactive', outcome && !outcome.ok ? outcome.error : undefined))
    }
    tx.onabort = () => {
      hooks.onAbort?.()
      reject(new StorageWriteError(operation, outcome && !outcome.ok ? 'body' : 'abort', outcome && !outcome.ok ? outcome.error : tx.error))
    }
    let pending: Promise<T> | T
    try {
      pending = body(tx)
    } catch (error) {
      outcome = { ok: false, error }
      try { tx.abort() } catch { /* the transaction already finished */ }
      return
    }
    Promise.resolve(pending).then(
      (value) => { outcome = { ok: true, value } },
      (error) => {
        outcome = { ok: false, error }
        try { tx.abort() } catch { /* the transaction already finished */ }
      },
    )
  })
}

/** Aborts the transaction and ends the body with a refusal value. Nothing commits. */
class Refusal<T> {
  readonly value: T
  constructor(value: T) { this.value = value }
}

async function refusing<T>(operation: () => Promise<T>): Promise<T> {
  try {
    return await operation()
  } catch (error) {
    if (error instanceof StorageWriteError && error.phase === 'body' && error.cause instanceof Refusal) {
      return (error.cause as Refusal<T>).value
    }
    throw error
  }
}

/** A stable identity for an IndexedDB key, for maps inside one transaction. */
export const keyIdentity = (key: IDBValidKey) => JSON.stringify(key)

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

export function classifyLegacyState(value: unknown): LegacyClass {
  const state = isObject(value) ? value.state : undefined
  if (state === 'PENDING') return 'M01'
  if (state === 'QUEUED') return 'M02'
  if (state === 'ACTUATED' || state === 'SYNCED' || state === 'RESOLVED') return 'other'
  return 'unclassifiable'
}

/**
 * The version-3 upgrade. Creates every missing store and stamps each existing
 * occurrence with its legacy class, from its stored state only. A real storage
 * failure aborts the whole upgrade and leaves the previous version unchanged.
 */
function upgrade(request: IDBOpenDBRequest, oldVersion: number) {
  const db = request.result
  const tx = request.transaction
  if (!tx) throw new Error('upgrade without a version-change transaction')
  if (!db.objectStoreNames.contains(STORE.occurrences)) db.createObjectStore(STORE.occurrences, { keyPath: 'occurrenceId' })
  if (!db.objectStoreNames.contains(STORE.revocations)) db.createObjectStore(STORE.revocations, { keyPath: 'ticketID' })
  if (!db.objectStoreNames.contains(STORE.revocationMeta)) db.createObjectStore(STORE.revocationMeta, { keyPath: 'key' })
  if (!db.objectStoreNames.contains(STORE.legacyClasses)) db.createObjectStore(STORE.legacyClasses, { keyPath: 'occurrenceId' })
  if (!db.objectStoreNames.contains(STORE.recoveryNotices)) db.createObjectStore(STORE.recoveryNotices, { keyPath: 'occurrenceId' })
  if (!db.objectStoreNames.contains(STORE.heldDisposals)) db.createObjectStore(STORE.heldDisposals, { keyPath: 'occurrenceId' })
  if (!db.objectStoreNames.contains(STORE.exportReceipts)) db.createObjectStore(STORE.exportReceipts, { keyPath: 'receiptId' })
  if (oldVersion !== 1 && oldVersion !== 2) return

  const classes = tx.objectStore(STORE.legacyClasses)
  const cursorRequest = tx.objectStore(STORE.occurrences).openCursor()
  cursorRequest.onsuccess = () => {
    const cursor = cursorRequest.result
    if (!cursor) return
    const record: LegacyClassRecord = {
      occurrenceId: cursor.primaryKey,
      legacyClass: classifyLegacyState(cursor.value),
      sourceVersion: oldVersion,
    }
    classes.put(record)
    cursor.continue()
  }
}

/** Starts the version-3 open. The caller owns the request: it can abandon it and close a late connection. */
export function requestOccurrenceDatabase(factory: IDBFactory, dbName: string): IDBOpenDBRequest {
  const request = factory.open(dbName, OCCURRENCE_DB_VERSION)
  request.onupgradeneeded = (event) => upgrade(request, event.oldVersion)
  return request
}

/** A version-3 database without every required store is incompatible: TKT-399's three-store schema, for example. */
export function missingStores(db: IDBDatabase): string[] {
  return REQUIRED_STORES.filter((name) => !db.objectStoreNames.contains(name))
}

export interface OccurrenceStore extends DecisionWriter {
  readonly db: IDBDatabase
  mode(): StoreMode
  setMode(mode: StoreMode): void

  mintNormal(occurrenceId: string): Promise<CurrentRecord>
  mintRefusal(occurrenceId: string): Promise<CurrentRecord>
  reserveActuation(occurrenceId: string): Promise<ActuationResult>
  persistDecision(decision: FinalDecision): Promise<TransitionResult>
  enrichDenial(decision: TerminalDecision, reason: string): Promise<TransitionResult>
  applyReconcileResults(submitted: readonly IDBValidKey[], results: ReadonlyArray<{ occurrence_id: string; result: string }>): Promise<ReconcileApplyResult>
  renewOwnedPending(): Promise<number>
  recoverDeadOwners(): Promise<RecoveryReport>
  queued(): Promise<{ entries: QueuedEntry[]; warnings: StorageWarning[] }>

  readNotices(): Promise<NoticeView[]>
  acknowledgeNotice(occurrenceId: string): Promise<'acknowledged' | 'alreadySame' | 'missing'>

  readDisposalSnapshot(): Promise<DisposalSnapshot>
  commitExportReceipt(receipt: ExportReceipt): Promise<void>
  acknowledgeHeld(receiptId: string, keys: readonly IDBValidKey[]): Promise<AcknowledgeResult>

  isRevoked(ticketID: string): Promise<boolean>
  mergeRevoked(ticketIDs: string[]): Promise<void>
  completeRevocationPull(fingerprint: string, completedAt: string): Promise<void>
  revocationPull(fingerprint: string): Promise<{ completedAt?: string }>

  /** Raw read of one stored occurrence, for diagnostics and tests. */
  get(occurrenceId: IDBValidKey): Promise<unknown>
}

export type OccurrenceStoreOptions = {
  db: IDBDatabase
  coordinator: PageCoordinator
  generation: number
  now?: () => number
  leaseMs?: number
}

type RevocationMeta = { key: `pull:${string}`; completedAt: string }

export function createOccurrenceStore(options: OccurrenceStoreOptions): OccurrenceStore {
  const { db, coordinator, generation } = options
  const now = options.now ?? Date.now
  const leaseMs = options.leaseMs ?? PENDING_LEASE_MS
  let mode: StoreMode = 'initializing'
  const iso = () => new Date(now()).toISOString()

  const requireMode = (operation: string, allowed: readonly StoreMode[]) => {
    if (!allowed.includes(mode)) throw new StorageWriteError(operation, 'create', new Error(`store is ${mode}`))
  }

  const mint = async (occurrenceId: string, type: ScanType): Promise<CurrentRecord> => {
    const operation = type === 'normal' ? 'mintNormal' : 'mintRefusal'
    const view = coordinator.view(occurrenceId)
    if (!view) throw new Error(`occurrence ${occurrenceId} is not registered on this page`)
    if (view.captured.type !== type) throw new Error(`occurrence ${occurrenceId} is not a ${type} scan`)
    const observer = coordinator.mintStarted(occurrenceId)
    try {
      requireMode(operation, ['ready'])
    } catch (error) {
      observer.aborted()
      throw error
    }
    const { captured } = view
    const base = {
      occurrenceId: captured.occurrenceId,
      qrPayload: captured.qrPayload,
      occurredAt: captured.occurredAt,
      createdAt: captured.createdAt,
      state: 'PENDING' as const,
      actuated: false,
      ownerId: coordinator.ownerId,
      leaseExpiresAt: now() + leaseMs,
      recordFormat: RECORD_FORMAT,
    }
    const record: CurrentRecord = type === 'normal'
      ? { ...base, direction: 'entry' }
      : { ...base, localDecision: 'revocation_refused' }
    return runTransaction(db, operation, STORE.occurrences, 'readwrite', STRICT, (tx) => {
      // add, not put: a duplicate id aborts instead of overwriting another row.
      tx.objectStore(STORE.occurrences).add(record)
      return record
    }, { onComplete: observer.committed, onAbort: observer.aborted })
  }

  const store: OccurrenceStore = {
    db,
    generation,
    mode: () => mode,
    setMode(next) {
      mode = next
    },

    mintNormal: (occurrenceId) => mint(occurrenceId, 'normal'),
    mintRefusal: (occurrenceId) => mint(occurrenceId, 'refusal'),

    async reserveActuation(occurrenceId) {
      const view = coordinator.view(occurrenceId)
      if (!view || view.mark !== 'none' || !coordinator.canReserve(occurrenceId)) return { reserved: false, why: 'not_marking' }
      requireMode('reserveActuation', ['initializing', 'ready', 'draining'])
      const decidedAt = iso()
      const observer = coordinator.markStarted(occurrenceId, decidedAt)
      let result: ActuationResult = { reserved: false, why: 'missing' }
      let existingDecidedAt: string | undefined
      try {
        return await runTransaction(db, 'reserveActuation', STORE.occurrences, 'readwrite', STRICT, async (tx) => {
          const os = tx.objectStore(STORE.occurrences)
          const raw = await requestDone(os.get(occurrenceId))
          const read = readCurrentRecord(raw)
          if (raw === undefined) result = { reserved: false, why: 'missing' }
          else if (!read.ok) result = { reserved: false, why: 'invalid_record' }
          else if (read.record.ownerId !== coordinator.ownerId) result = { reserved: false, why: 'foreign' }
          else if (scanType(read.record) !== 'normal') result = { reserved: false, why: 'refusal' }
          else if (read.record.state === 'ACTUATED') {
            result = { reserved: false, why: 'own_existing_actuation' }
            existingDecidedAt = read.record.decidedAt
          }
          else if (read.record.state !== 'PENDING') result = { reserved: false, why: 'ineligible_state' }
          // actuated:false needs no check here: readCurrentRecord refuses an actuated PENDING row.
          else if (!coordinator.canReserve(occurrenceId)) result = { reserved: false, why: 'not_marking' }
          else {
            os.put({
              ...read.record,
              state: 'ACTUATED',
              actuated: true,
              localOutcome: { family: 'actuation', cause: 'reserved' },
              decidedAt,
            })
            result = { reserved: true }
          }
          return result
        }, {
          onComplete: () => {
            if (result.reserved) observer.reserved()
            else if (result.why === 'own_existing_actuation') observer.reserved(existingDecidedAt)
            else observer.notReserved()
          },
          onAbort: observer.aborted,
        })
      } catch (error) {
        observer.aborted()
        throw error
      }
    },

    async persistDecision(decision) {
      // Only the coordinator's latched decision can be written. A caller cannot
      // supply another decision for a latched occurrence (I1).
      if (coordinator.latched(decision.occurrenceId) !== decision) return { status: 'ineligible', why: 'not the latched decision' }
      if (decision.kind === 'actuation') return { status: 'ineligible', why: 'actuation is written by the reservation' }
      requireMode('persistDecision', ['initializing', 'ready', 'draining'])
      const targetState = decision.kind === 'queue' ? 'QUEUED' : 'RESOLVED'
      return runTransaction(db, 'persistDecision', STORE.occurrences, 'readwrite', STRICT, async (tx): Promise<TransitionResult> => {
        const os = tx.objectStore(STORE.occurrences)
        const raw = await requestDone(os.get(decision.occurrenceId))
        if (raw === undefined) return { status: 'missing' }
        const read = readCurrentRecord(raw)
        if (!read.ok) return { status: 'ineligible', why: read.problem }
        const row = read.record
        if (row.ownerId !== coordinator.ownerId) return { status: 'foreign' }
        const type = scanType(row)
        const refusalDecision = decision.kind === 'queue' && decision.outcome.cause === 'local_refusal'
        const refusalCancellation = decision.kind === 'terminal' && (decision.outcome.cause === 'cancelled' || (decision.outcome.cause === 'stopped' && decision.outcome.checkpoint === 'send'))
          && decision.outcome.requestMayHaveBeenSent === false
        if (type === 'refusal' ? !refusalDecision && !refusalCancellation : refusalDecision) return { status: 'ineligible', why: 'decision does not match the scan type' }
        if (row.state === 'PENDING') {
          os.put({ ...row, state: targetState, localOutcome: decision.outcome, decidedAt: decision.decidedAt })
          return { status: 'changed' }
        }
        if (row.state === targetState && row.localOutcome && row.decidedAt === decision.decidedAt) {
          const stored = row.localOutcome
          const same = sameOutcome(stored, decision.outcome)
            // An enriched denial still satisfies its unknown-reason target (TKT-558 D11).
            || (decision.kind === 'terminal' && decision.outcome.cause === 'denied' && decision.outcome.reason === null
              && stored.family === 'terminal' && stored.cause === 'denied' && stored.latchId === decision.outcome.latchId
              && stored.status === decision.outcome.status)
          if (same) return { status: 'alreadySame' }
        }
        return { status: 'ineligible', why: `row is ${row.state}` }
      })
    },

    async enrichDenial(decision, reason) {
      if (coordinator.latched(decision.occurrenceId) !== decision) return { status: 'ineligible', why: 'not the latched decision' }
      const target = decision.outcome
      if (target.cause !== 'denied' || target.reason !== null || !reason) return { status: 'ineligible', why: 'not an unknown-reason denial' }
      requireMode('enrichDenial', ['initializing', 'ready', 'draining'])
      return runTransaction(db, 'enrichDenial', STORE.occurrences, 'readwrite', STRICT, async (tx): Promise<TransitionResult> => {
        const os = tx.objectStore(STORE.occurrences)
        const raw = await requestDone(os.get(decision.occurrenceId))
        if (raw === undefined) return { status: 'missing' }
        const read = readCurrentRecord(raw)
        if (!read.ok) return { status: 'ineligible', why: read.problem }
        const row = read.record
        if (row.ownerId !== coordinator.ownerId) return { status: 'foreign' }
        const stored = row.localOutcome
        if (row.state !== 'RESOLVED' || stored?.family !== 'terminal' || stored.cause !== 'denied'
          || stored.latchId !== target.latchId || stored.status !== target.status || row.decidedAt !== decision.decidedAt) {
          return { status: 'ineligible', why: 'row is not this denial' }
        }
        if (stored.reason === reason) return { status: 'alreadySame' }
        if (stored.reason !== null) return { status: 'ineligible', why: 'reason already recorded' }
        // Only the reason changes: state, identity, type, times and the latch stay.
        os.put({ ...row, localOutcome: { ...stored, reason } })
        return { status: 'changed' }
      })
    },

    async applyReconcileResults(submitted, results) {
      // Validate the whole response before any write (I7, TKT-558 D17).
      const submittedKeys = new Map<string, IDBValidKey>()
      for (const key of submitted) {
        if (typeof key !== 'string') return { status: 'invalid', why: 'a submitted key is not a string id' }
        if (submittedKeys.has(key)) return { status: 'invalid', why: 'a submitted id repeats' }
        submittedKeys.set(key, key)
      }
      const planned = new Map<string, ReconcileResult>()
      for (const entry of results) {
        if (!submittedKeys.has(entry.occurrence_id)) return { status: 'invalid', why: 'a result names an id that was not submitted' }
        if (planned.has(entry.occurrence_id)) return { status: 'invalid', why: 'a result repeats an id' }
        if (!RECONCILE_RESULTS.includes(entry.result as ReconcileResult)) return { status: 'invalid', why: 'a result is outside the contract' }
        planned.set(entry.occurrence_id, entry.result as ReconcileResult)
      }
      if (planned.size !== submittedKeys.size) return { status: 'invalid', why: 'a submitted id has no result' }
      requireMode('applyReconcileResults', ['initializing', 'ready', 'draining'])

      return refusing(() => runTransaction(db, 'applyReconcileResults', [STORE.occurrences, STORE.legacyClasses], 'readwrite', STRICT, async (tx): Promise<ReconcileApplyResult> => {
        const os = tx.objectStore(STORE.occurrences)
        const classes = tx.objectStore(STORE.legacyClasses)
        const writes: Record<string, unknown>[] = []
        let alreadyApplied = 0
        for (const [id, result] of planned) {
          const raw = await requestDone(os.get(id))
          const legacy = await requestDone(classes.get(id) as IDBRequest<LegacyClassRecord | undefined>)
          const retired = result === 'recorded' || result === 'synced' ? 'SYNCED' : 'RESOLVED'
          let eligible = false
          if (legacy) {
            eligible = legacy.legacyClass === 'M02' && isObject(raw) && raw.state === 'QUEUED'
          } else {
            const read = readCurrentRecord(raw)
            eligible = read.ok && read.record.state === 'QUEUED'
          }
          if (eligible) {
            writes.push({ ...(raw as Record<string, unknown>), state: retired, result })
            continue
          }
          if (isObject(raw) && raw.state === retired && raw.result === result) {
            alreadyApplied += 1
            continue
          }
          // A changed row aborts the whole batch: no peer is retired without it.
          throw new Refusal<ReconcileApplyResult>({ status: 'incompatible', key: id })
        }
        for (const next of writes) os.put(next)
        return { status: 'applied', retired: writes.length, alreadyApplied }
      }))
    },

    async renewOwnedPending() {
      requireMode('renewOwnedPending', ['ready'])
      // One transaction reads, checks and writes, so a snapshot never lands after a decision.
      return runTransaction(db, 'renewOwnedPending', STORE.occurrences, 'readwrite', DEFAULT, async (tx) => {
        const os = tx.objectStore(STORE.occurrences)
        const rows = await requestDone(os.getAll())
        const leaseExpiresAt = now() + leaseMs
        let renewed = 0
        for (const raw of rows) {
          const read = readCurrentRecord(raw)
          if (read.ok && read.record.state === 'PENDING' && read.record.ownerId === coordinator.ownerId) {
            os.put({ ...read.record, leaseExpiresAt })
            renewed += 1
          }
        }
        return renewed
      })
    },

    async recoverDeadOwners() {
      requireMode('recoverDeadOwners', ['initializing', 'ready'])
      const report: RecoveryReport = { recovered: [], liveOwner: [], notEligible: [], failed: [] }
      const candidates = await runTransaction(db, 'recoverDeadOwners.list', [STORE.occurrences, STORE.legacyClasses], 'readonly', undefined, async (tx) => {
        const rows = await requestDone(tx.objectStore(STORE.occurrences).getAll())
        const legacyKeys = await requestDone(tx.objectStore(STORE.legacyClasses).getAllKeys())
        const legacy = new Set(legacyKeys.map(keyIdentity))
        return rows.flatMap((raw) => {
          const read = readCurrentRecord(raw)
          if (!read.ok || legacy.has(keyIdentity(read.record.occurrenceId))) return []
          const row = read.record
          return row.state === 'PENDING' && row.ownerId !== coordinator.ownerId ? [{ occurrenceId: row.occurrenceId, ownerId: row.ownerId }] : []
        })
      })

      for (const candidate of candidates) {
        try {
          const outcome = await coordinator.lock.runIfOwnerAbsent(candidate.ownerId, () =>
            runTransaction(db, 'recoverDeadOwners', [STORE.occurrences, STORE.recoveryNotices, STORE.legacyClasses], 'readwrite', STRICT, async (tx) => {
              const os = tx.objectStore(STORE.occurrences)
              const raw = await requestDone(os.get(candidate.occurrenceId))
              const legacy = await requestDone(tx.objectStore(STORE.legacyClasses).get(candidate.occurrenceId))
              const read = readCurrentRecord(raw)
              // Re-read every predicate under the dead owner's lock.
              if (legacy !== undefined || !read.ok) return false
              const row = read.record
              if (row.state !== 'PENDING' || row.ownerId !== candidate.ownerId || row.ownerId === coordinator.ownerId) return false
              const reloadPredecessor = coordinator.recoverOwnerId !== undefined && row.ownerId === coordinator.recoverOwnerId
              if (!(reloadPredecessor || row.leaseExpiresAt <= now())) return false
              // The row and its notice commit together, or neither does (I8, TKT-558 D7).
              os.put({ ...row, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } })
              const notice: RecoveryNotice = { occurrenceId: row.occurrenceId, cause: 'uncertain_recovery', recoveredAt: iso() }
              tx.objectStore(STORE.recoveryNotices).put(notice)
              return true
            })
          )
          if (!outcome.ran) report.liveOwner.push(candidate.occurrenceId)
          else if (outcome.value) report.recovered.push(candidate.occurrenceId)
          else report.notEligible.push(candidate.occurrenceId)
        } catch (error) {
          report.failed.push({ occurrenceId: candidate.occurrenceId, error })
        }
      }
      return report
    },

    async queued() {
      return runTransaction(db, 'queued', [STORE.occurrences, STORE.legacyClasses], 'readonly', undefined, async (tx) => {
        const os = tx.objectStore(STORE.occurrences)
        const keys = await requestDone(os.getAllKeys())
        const rows = await requestDone(os.getAll())
        const classes = await requestDone(tx.objectStore(STORE.legacyClasses).getAll() as IDBRequest<LegacyClassRecord[]>)
        const legacy = new Map(classes.map((entry) => [keyIdentity(entry.occurrenceId), entry.legacyClass]))
        const entries: QueuedEntry[] = []
        const warnings: StorageWarning[] = []
        rows.forEach((raw, index) => {
          const key = keys[index]
          const legacyClass = legacy.get(keyIdentity(key))
          if (legacyClass !== undefined) {
            if (legacyClass === 'M02' && isObject(raw) && raw.state === 'QUEUED') entries.push({ format: 0, key, legacyClass, raw })
            return
          }
          const read = readCurrentRecord(raw)
          if (!read.ok) {
            warnings.push({ key, problem: read.problem })
            return
          }
          if (read.record.state === 'QUEUED') entries.push({ format: 1, key: read.record.occurrenceId, record: read.record })
        })
        return { entries, warnings }
      })
    },

    async readNotices() {
      return runTransaction(db, 'readNotices', [STORE.recoveryNotices, STORE.occurrences], 'readonly', undefined, async (tx) => {
        const notices = await requestDone(tx.objectStore(STORE.recoveryNotices).getAll() as IDBRequest<RecoveryNotice[]>)
        const views: NoticeView[] = []
        for (const notice of notices) {
          if (notice.acknowledgedAt !== undefined) continue
          const read = readCurrentRecord(await requestDone(tx.objectStore(STORE.occurrences).get(notice.occurrenceId)))
          views.push({
            occurrenceId: notice.occurrenceId,
            occurredAt: read.ok ? read.record.occurredAt : undefined,
            type: read.ok ? scanType(read.record) : undefined,
            cause: notice.cause,
            syncState: read.ok ? read.record.state : 'unreadable',
          })
        }
        return views
      })
    },

    async acknowledgeNotice(occurrenceId) {
      return runTransaction(db, 'acknowledgeNotice', STORE.recoveryNotices, 'readwrite', STRICT, async (tx) => {
        const os = tx.objectStore(STORE.recoveryNotices)
        const notice = await requestDone(os.get(occurrenceId) as IDBRequest<RecoveryNotice | undefined>)
        if (!notice) return 'missing' as const
        if (notice.acknowledgedAt !== undefined) return 'alreadySame' as const
        os.put({ ...notice, acknowledgedAt: iso() } satisfies RecoveryNotice)
        return 'acknowledged' as const
      })
    },

    async readDisposalSnapshot() {
      return runTransaction(db, 'readDisposalSnapshot', [STORE.occurrences, STORE.legacyClasses, STORE.heldDisposals], 'readonly', undefined, async (tx) => {
        const classes = await requestDone(tx.objectStore(STORE.legacyClasses).getAll() as IDBRequest<LegacyClassRecord[]>)
        const snapshot: DisposalSnapshot = { held: [], legacyQueued: [] }
        for (const entry of classes) {
          if (!HELD_CLASSES.includes(entry.legacyClass) && entry.legacyClass !== 'M02') continue
          const raw = await requestDone(tx.objectStore(STORE.occurrences).get(entry.occurrenceId))
          if (entry.legacyClass === 'M02') {
            snapshot.legacyQueued.push({ key: entry.occurrenceId, raw })
          } else {
            const disposal = await requestDone(tx.objectStore(STORE.heldDisposals).get(entry.occurrenceId) as IDBRequest<HeldDisposal | undefined>)
            snapshot.held.push({ key: entry.occurrenceId, legacyClass: entry.legacyClass, raw, disposal })
          }
        }
        return snapshot
      })
    },

    async commitExportReceipt(receipt) {
      await runTransaction(db, 'commitExportReceipt', STORE.exportReceipts, 'readwrite', STRICT, (tx) => {
        tx.objectStore(STORE.exportReceipts).add(receipt)
      })
    },

    async acknowledgeHeld(receiptId, keys) {
      if (keys.length === 0) return { status: 'refused', why: 'no held record is selected' }
      return refusing(() => runTransaction(db, 'acknowledgeHeld', [STORE.exportReceipts, STORE.occurrences, STORE.legacyClasses, STORE.heldDisposals], 'readwrite', STRICT, async (tx): Promise<AcknowledgeResult> => {
        const refuse = (why: string) => { throw new Refusal<AcknowledgeResult>({ status: 'refused', why }) }
        const receipt = await requestDone(tx.objectStore(STORE.exportReceipts).get(receiptId) as IDBRequest<ExportReceipt | undefined>)
        if (!receipt) refuse('no committed export receipt')
        const covered = new Map(receipt!.held.map((entry) => [keyIdentity(entry.occurrenceId), entry.snapshot]))
        const seen = new Set<string>()
        const acknowledgedAt = iso()
        const writes: HeldDisposal[] = []
        for (const key of keys) {
          const id = keyIdentity(key)
          if (seen.has(id)) refuse('a selected record repeats')
          seen.add(id)
          const legacy = await requestDone(tx.objectStore(STORE.legacyClasses).get(key) as IDBRequest<LegacyClassRecord | undefined>)
          if (!legacy || !HELD_CLASSES.includes(legacy.legacyClass)) refuse('a selected record is not held')
          const snapshot = covered.get(id)
          if (snapshot === undefined) refuse('a selected record is not in the export')
          const raw = await requestDone(tx.objectStore(STORE.occurrences).get(key))
          if (raw === undefined || encodeHeldSnapshot(key, raw) !== snapshot) refuse('a selected record changed after the export')
          writes.push({ occurrenceId: key, disposition: 'operator_acknowledged_without_sync', acknowledgedAt, receiptId })
        }
        for (const disposal of writes) {
          const existing = await requestDone(tx.objectStore(STORE.heldDisposals).get(disposal.occurrenceId) as IDBRequest<HeldDisposal | undefined>)
          if (!existing) tx.objectStore(STORE.heldDisposals).put(disposal)
        }
        return { status: 'acknowledged', count: writes.length }
      }))
    },

    async isRevoked(ticketID) {
      return runTransaction(db, 'isRevoked', STORE.revocations, 'readonly', undefined, async (tx) =>
        (await requestDone(tx.objectStore(STORE.revocations).get(ticketID.toLowerCase()))) !== undefined)
    },
    async mergeRevoked(ticketIDs) {
      await runTransaction(db, 'mergeRevoked', STORE.revocations, 'readwrite', undefined, (tx) => {
        for (const ticketID of ticketIDs) tx.objectStore(STORE.revocations).put({ ticketID: ticketID.toLowerCase() })
      })
    },
    async completeRevocationPull(fingerprint, completedAt) {
      await runTransaction(db, 'completeRevocationPull', STORE.revocationMeta, 'readwrite', undefined, (tx) => {
        tx.objectStore(STORE.revocationMeta).put({ key: `pull:${fingerprint}`, completedAt } satisfies RevocationMeta)
      })
    },
    async revocationPull(fingerprint) {
      return runTransaction(db, 'revocationPull', STORE.revocationMeta, 'readonly', undefined, async (tx) => {
        const meta = await requestDone(tx.objectStore(STORE.revocationMeta).get(`pull:${fingerprint}`) as IDBRequest<RevocationMeta | undefined>)
        return meta?.completedAt === undefined ? {} : { completedAt: meta.completedAt }
      })
    },

    async get(occurrenceId) {
      return runTransaction(db, 'get', STORE.occurrences, 'readonly', undefined, (tx) => requestDone(tx.objectStore(STORE.occurrences).get(occurrenceId)))
    },
  }
  return store
}
