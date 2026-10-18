// Storage readiness and connection lifecycle (ADR-085 §7.3, TKT-558 D8).
//
// Storage starts independently of scan input. A scan is accepted only while
// the state is ready: a compatible version-3 connection is open, the page owner
// lock is held, recovery ran, the initial held and notice reads completed and
// every pending repair attempt settled. A repair attempt can fail; readiness
// records the result and keeps the target protected. Before ready, a presented
// credential ends with S01 and nothing is stored for later.
//
// Each open attempt has its own generation. A late success from an abandoned
// generation closes its connection and never publishes ready. A version change
// drains the old connection: in-flight mint and mark transactions settle, every
// pending final write is attempted on that connection, and then it closes. An
// unexpected close fails only its own generation. None of these events replaces
// the page coordinator or releases the page owner lock.

import { pageCoordinator, type PageCoordinator, type PersistReport } from './occurrence-coordinator'
import {
  LEASE_RENEWAL_MS,
  OCCURRENCE_DB,
  PENDING_LEASE_MS,
  createOccurrenceStore,
  missingStores,
  requestOccurrenceDatabase,
  type DisposalSnapshot,
  type NoticeView,
  type OccurrenceStore,
  type RecoveryReport,
  type StorageWarning,
} from './occurrence-store'

export type ReadinessProgress = { opened: boolean; lock: boolean; recovered: boolean; reads: boolean; repairs: boolean }
export type StorageFailureCause = 'lock_unavailable' | 'open_failed' | 'incompatible_schema' | 'unexpected_close' | 'storage_error'

export type StorageReadiness =
  | { status: 'idle' }
  | { status: 'opening'; generation: number; blocked: boolean; progress: ReadinessProgress }
  | { status: 'ready'; generation: number }
  | { status: 'draining'; generation: number }
  | { status: 'closed'; generation: number; cause: 'versionchange' | 'deleted' | 'stopped' }
  | { status: 'failed'; generation: number; cause: StorageFailureCause }

export type InitialReads = {
  recovery: RecoveryReport
  disposal: DisposalSnapshot
  notices: NoticeView[]
  queueWarnings: StorageWarning[]
  repairs: PersistReport[]
}

export interface StorageController {
  readonly coordinator: PageCoordinator
  state(): StorageReadiness
  subscribe(listener: (state: StorageReadiness) => void): () => void
  /** Starts the first generation. Later calls do nothing. */
  start(): void
  /** Retry storage: abandons an unfinished generation and starts a new one. Does nothing while ready or draining. */
  retry(): void
  /** The ready store, or null. A scan presented while this is null ends S01 and stores nothing. */
  readyStore(): OccurrenceStore | null
  initialReads(): InitialReads | null
  /** Ends this controller's connection work, for test teardown. The page owner lock stays held. */
  stop(): void
}

export type StorageControllerOptions = {
  coordinator: PageCoordinator
  dbName?: string
  /** Read at each open, so a test can replace the global factory. */
  factory?: () => IDBFactory
  now?: () => number
  leaseMs?: number
  renewalMs?: number
}

type Connection = {
  generation: number
  request?: IDBOpenDBRequest
  db?: IDBDatabase
  store?: OccurrenceStore
  heartbeat?: ReturnType<typeof setInterval>
  draining: boolean
  closed: boolean
}

class AbandonedOpen extends Error {}

export function createStorageController(options: StorageControllerOptions): StorageController {
  const { coordinator } = options
  const dbName = options.dbName ?? OCCURRENCE_DB
  const factory = options.factory ?? (() => globalThis.indexedDB)
  const now = options.now ?? Date.now
  const leaseMs = options.leaseMs ?? PENDING_LEASE_MS
  const renewalMs = options.renewalMs ?? LEASE_RENEWAL_MS

  let state: StorageReadiness = { status: 'idle' }
  let generation = 0
  let current: Connection | undefined
  let reads: InitialReads | null = null
  const listeners = new Set<(state: StorageReadiness) => void>()

  const publish = (next: StorageReadiness) => {
    state = next
    for (const listener of listeners) listener(next)
  }
  const isCurrent = (connection: Connection) => current === connection
  const active = (connection: Connection) => isCurrent(connection) && !connection.draining && !connection.closed

  const stopHeartbeat = (connection: Connection) => {
    if (connection.heartbeat !== undefined) globalThis.clearInterval(connection.heartbeat)
    connection.heartbeat = undefined
  }

  /** The explicit close path. An explicit close emits no close event, so it changes state here. */
  const closeConnection = (connection: Connection) => {
    if (connection.closed) return
    connection.closed = true
    stopHeartbeat(connection)
    if (connection.store) {
      connection.store.setMode('closed')
      coordinator.detach(connection.store)
    }
    connection.db?.close()
  }

  const fail = (connection: Connection, cause: StorageFailureCause) => {
    closeConnection(connection)
    if (isCurrent(connection)) {
      reads = null
      publish({ status: 'failed', generation: connection.generation, cause })
    }
  }

  const onVersionChange = (connection: Connection, newVersion: number | null) => {
    if (connection.closed || connection.draining) return
    connection.draining = true
    stopHeartbeat(connection)
    if (isCurrent(connection)) {
      reads = null
      publish({ status: 'draining', generation: connection.generation })
    }
    const store = connection.store
    store?.setMode('draining')
    void (async () => {
      // Repair on the still-open connection, then close after every attempt settles (D27).
      if (store) await coordinator.drain(store)
      closeConnection(connection)
      if (isCurrent(connection)) {
        publish({ status: 'closed', generation: connection.generation, cause: newVersion === null ? 'deleted' : 'versionchange' })
      }
    })()
  }

  const onUnexpectedClose = (connection: Connection) => {
    if (connection.closed) return
    connection.closed = true
    stopHeartbeat(connection)
    if (connection.store) {
      connection.store.setMode('closed')
      coordinator.detach(connection.store)
    }
    // A late close of an old generation cannot fail a newer one.
    if (isCurrent(connection)) {
      reads = null
      publish({ status: 'failed', generation: connection.generation, cause: 'unexpected_close' })
    }
  }

  const open = (connection: Connection): Promise<IDBDatabase> =>
    new Promise((resolve, reject) => {
      const request = requestOccurrenceDatabase(factory(), dbName)
      connection.request = request
      request.onblocked = () => {
        // Progress only. Readiness never derives from a blocked event (RD-03).
        if (active(connection) && state.status === 'opening') publish({ ...state, blocked: true })
      }
      request.onsuccess = () => {
        const db = request.result
        if (!active(connection)) {
          // An abandoned generation closes its late connection and publishes nothing (RD-04, RD-05).
          db.close()
          reject(new AbandonedOpen())
          return
        }
        resolve(db)
      }
      request.onerror = () => reject(request.error)
    })

  const run = async (connection: Connection) => {
    const progress: ReadinessProgress = { opened: false, lock: false, recovered: false, reads: false, repairs: false }
    const publishProgress = () => {
      if (active(connection) && state.status === 'opening') publish({ ...state, progress: { ...progress } })
    }
    publish({ status: 'opening', generation: connection.generation, blocked: false, progress: { ...progress } })

    const lock = coordinator.acquireLock().then(() => {
      progress.lock = true
      publishProgress()
    })
    lock.catch(() => undefined)

    let db: IDBDatabase
    try {
      db = await open(connection)
    } catch (error) {
      if (!(error instanceof AbandonedOpen) && active(connection)) fail(connection, 'open_failed')
      return
    }
    connection.db = db
    // Attach the lifecycle handlers before anything can use the connection.
    db.onversionchange = (event) => onVersionChange(connection, event.newVersion)
    db.onclose = () => onUnexpectedClose(connection)
    if (missingStores(db).length > 0) {
      fail(connection, 'incompatible_schema')
      return
    }
    const store = createOccurrenceStore({ db, coordinator, generation: connection.generation, now, leaseMs })
    connection.store = store
    coordinator.attach(store)
    progress.opened = true
    publishProgress()

    let initial: InitialReads
    try {
      const recovery = await store.recoverDeadOwners()
      if (!active(connection)) return
      progress.recovered = true
      publishProgress()
      const disposal = await store.readDisposalSnapshot()
      const notices = await store.readNotices()
      const { warnings } = await store.queued()
      if (!active(connection)) return
      progress.reads = true
      publishProgress()
      // Readiness needs every pending repair attempt observed, not repair success.
      const repairs = await coordinator.repairAll(store)
      if (!active(connection)) return
      progress.repairs = true
      publishProgress()
      initial = { recovery, disposal, notices, queueWarnings: warnings, repairs }
    } catch {
      if (active(connection)) fail(connection, 'storage_error')
      return
    }
    try {
      await lock
    } catch {
      if (active(connection)) fail(connection, 'lock_unavailable')
      return
    }
    if (!active(connection)) return
    reads = initial
    store.setMode('ready')
    connection.heartbeat = globalThis.setInterval(() => {
      // A failed renewal leaves the prior row and the page lock as they were.
      store.renewOwnedPending().catch(() => undefined)
    }, renewalMs)
    publish({ status: 'ready', generation: connection.generation })
  }

  const begin = () => {
    const previous = current
    generation += 1
    const connection: Connection = { generation, draining: false, closed: false }
    current = connection
    reads = null
    // The previous generation is abandoned. An opened but unfinished connection closes now;
    // a pending request closes its connection when it succeeds.
    if (previous && previous.db && !previous.draining) closeConnection(previous)
    void run(connection)
  }

  return {
    coordinator,
    state: () => state,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    start() {
      if (state.status === 'idle') begin()
    },
    retry() {
      if (state.status === 'ready' || state.status === 'draining') return
      begin()
    },
    readyStore() {
      return state.status === 'ready' && current?.store?.mode() === 'ready' ? current.store : null
    },
    initialReads: () => reads,
    stop() {
      const previous = current
      current = undefined
      reads = null
      if (previous) {
        closeConnection(previous)
        publish({ status: 'closed', generation: previous.generation, cause: 'stopped' })
      }
    },
  }
}

/** The one production storage controller. The App starts it; React cleanup never stops it. */
export const pageStorage: StorageController = createStorageController({ coordinator: pageCoordinator })
