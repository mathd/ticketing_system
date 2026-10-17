// Durable occurrence queue + actuation state (ADR-025 §D3/§D6).
//
// Every physical gate decision mints a UUIDv4 occurrence id, committed to
// IndexedDB BEFORE the request is sent; a response — first-time or replay —
// may actuate only while this device holds a pending, never-actuated record
// for that id, and the record is marked actuated BEFORE the accepted screen
// renders (fail-closed). IndexedDB over localStorage because the
// PENDING→ACTUATED transition needs a transactional read-modify-write: a
// racing duplicate response must not observe actuated:false twice.

import type { ScanDirection } from './access-contract'

export type OccurrenceState = 'PENDING' | 'ACTUATED' | 'QUEUED' | 'SYNCED' | 'RESOLVED'

export type OccurrenceRecord = {
  occurrenceId: string
  qrPayload: string
  occurredAt: string
  state: OccurrenceState
  actuated: boolean
  result?: string
  createdAt: string
  /** Page instance that owns an in-flight request. Absent only on legacy rows. */
  ownerId?: string
  /** Epoch milliseconds. The owner renews this while its page remains alive. */
  leaseExpiresAt?: number
  localDecision?: 'revocation_refused'
  /**
   * The direction the operator chose for this scan, committed with the record.
   * Absent only on rows written before TKT-399; those are entries.
   */
  direction?: ScanDirection
}

/** A record that mint has just committed always carries its direction. */
export type MintedOccurrence = OccurrenceRecord & { direction: ScanDirection }

export type RevocationPull = { completedAt?: string }

export interface OccurrenceStore {
  /** Commits the PENDING record; resolves only after the IDB transaction completes. */
  mint(qrPayload: string, occurredAt: string, direction: ScanDirection): Promise<MintedOccurrence>
  mintRefusal(qrPayload: string, occurredAt: string): Promise<OccurrenceRecord>
  isRevoked(ticketID: string): Promise<boolean>
  mergeRevoked(ticketIDs: string[]): Promise<void>
  completeRevocationPull(fingerprint: string, completedAt: string): Promise<void>
  revocationPull(fingerprint: string): Promise<RevocationPull>
  /**
   * Atomic PENDING-and-never-actuated → ACTUATED transition. Resolves true iff
   * THIS call performed it — the one signal that may open the gate.
   */
  actuate(occurrenceId: string): Promise<boolean>
  markQueued(occurrenceId: string): Promise<void>
  /** recorded/synced → SYNCED; any other result → RESOLVED (operator-facing, never a gate action). */
  markSynced(occurrenceId: string, result: string): Promise<void>
  queued(): Promise<OccurrenceRecord[]>
  get(occurrenceId: string): Promise<OccurrenceRecord | undefined>
  close(): void
}

const STORE = 'occurrences'
const REVOCATIONS = 'revocations'
const REVOCATION_META = 'revocation_meta'
type RevocationMeta = { key: `pull:${string}`; completedAt: string }
const DEFAULT_PENDING_LEASE_MS = 30_000

export type OccurrenceStoreOptions = {
  ownerId?: string
  /** Previous owner from this tab, supplied only after an actual page reload. */
  recoverOwnerId?: string
  pendingLeaseMs?: number
  now?: () => number
  ownerLiveness?: OwnerLiveness
  /** Lets a caller that waits for the database open learn early that it ran out of time. */
  openTimer?: OpenTimer
  /** Monotonic milliseconds. Decides if the database open ran out of time. */
  openClock?: () => number
}

/**
 * Keeps a page owner live independently of JavaScript timer scheduling. The
 * callback runs under the same browser-managed lock used by the owner.
 */
export interface OwnerLiveness {
  hold(ownerId: string): Promise<() => void>
  runIfOwnerAbsent(ownerId: string, task: () => Promise<void>): Promise<boolean>
}

const ownerLockName = (ownerId: string) => `ticketing.scanner.occurrence-owner.${ownerId}`

function browserOwnerLiveness(): OwnerLiveness {
  const locks = globalThis.navigator?.locks
  if (!locks) throw new Error('Web Locks are required for durable occurrence ownership')

  return {
    async hold(ownerId) {
      let releaseLock!: () => void
      let acquiredResolve!: () => void
      let acquiredReject!: (cause: unknown) => void
      const acquired = new Promise<void>((resolve, reject) => {
        acquiredResolve = resolve
        acquiredReject = reject
      })
      const released = new Promise<void>((resolve) => {
        releaseLock = resolve
      })
      const request = locks.request(ownerLockName(ownerId), async (lock) => {
        if (!lock) throw new Error('browser did not grant the occurrence-owner lock')
        acquiredResolve()
        await released
      })
      void request.catch(acquiredReject)
      await acquired

      let releasedOnce = false
      return () => {
        if (releasedOnce) return
        releasedOnce = true
        releaseLock()
      }
    },

    runIfOwnerAbsent(ownerId, task) {
      return locks.request(
        ownerLockName(ownerId),
        { mode: 'exclusive', ifAvailable: true },
        async (lock) => {
          if (!lock) return false
          await task()
          return true
        },
      )
    },
  }
}

function requestDone<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
}

function transactionDone(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve()
    tx.onerror = () => reject(tx.error)
    tx.onabort = () => reject(tx.error ?? new Error('transaction aborted'))
  })
}

// Version 3 changes no store or index. It is a barrier (TKT-399): a tab on the
// older code opens version 2, reconciles every QUEUED row and sends none with a
// direction, so it would send a queued exit as an entry. That tab never closes
// for a version change, so this version cannot open while it is open, and once
// this version has opened, the older code cannot open the database at all.
const DB_VERSION = 3

/** The open waits for another tab, on older code, to close its connection. */
export class OccurrenceStoreBlockedError extends Error {
  constructor() {
    super('another scanner tab holds the occurrence database open')
    this.name = 'OccurrenceStoreBlockedError'
  }
}

/** The open did not succeed in time. The page does not know why. */
export class OccurrenceStoreOpenTimeoutError extends Error {
  constructor() {
    super('the occurrence database did not open in time')
    this.name = 'OccurrenceStoreOpenTimeoutError'
  }
}

/** The store was closed, by this page or by a later version in another tab. */
export class OccurrenceStoreClosedError extends Error {
  constructor() {
    super('the occurrence store is closed')
    this.name = 'OccurrenceStoreClosedError'
  }
}

// A policy limit, not a measured value: we assume that an open with nothing in
// front of it takes much less time, and the ticket holder is still at the gate
// when the scanner refuses.
export const OPEN_TIMEOUT_MS = 3_000

/** Starts the time limit of one open and returns a function that stops it. */
export type OpenTimer = (onTimeout: () => void) => () => void

const defaultOpenTimer: OpenTimer = (onTimeout) => {
  const timer = globalThis.setTimeout(onTimeout, OPEN_TIMEOUT_MS)
  return () => globalThis.clearTimeout(timer)
}

// Read through globalThis at each call, so a test that replaces the clock
// replaces it here too.
const defaultOpenClock = () => globalThis.performance.now()

// Databases with an open request that this page gave up on and that still
// waits, with the error that request gave. The browser keeps one queue of open
// requests for each storage key (the origin, for this page) and database name,
// so a new request for that database would wait behind it. This page cannot
// see the requests of other tabs, so the time limit is what stops a request
// that waits behind theirs.
const abandonedOpens = new Map<string, () => Error>()

function openDatabase(dbName: string, openTimer: OpenTimer, openClock: () => number): Promise<IDBDatabase> {
  const abandoned = abandonedOpens.get(dbName)
  if (abandoned) return Promise.reject(abandoned())
  return new Promise((resolve, reject) => {
    // Once the caller has its error, no connection may reach it. The request
    // still waits, so the success handler below closes the connection it gets.
    let gaveUp = false
    let stopTimer = () => {}
    const giveUp = (error: () => Error) => {
      gaveUp = true
      stopTimer()
      abandonedOpens.set(dbName, error)
      reject(error())
    }
    const timeout = () => new OccurrenceStoreOpenTimeoutError()
    // Elapsed time decides the limit. A browser may run the timer callback late,
    // so the timer only lets a waiting caller learn early.
    const expiresAt = openClock() + OPEN_TIMEOUT_MS
    const open = indexedDB.open(dbName, DB_VERSION)
    stopTimer = openTimer(() => giveUp(timeout))
    open.onupgradeneeded = () => {
      // From 2 to 3 nothing changes, so every row stays as it is.
      const db = open.result
      if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE, { keyPath: 'occurrenceId' })
      if (!db.objectStoreNames.contains(REVOCATIONS)) db.createObjectStore(REVOCATIONS, { keyPath: 'ticketID' })
      if (!db.objectStoreNames.contains(REVOCATION_META)) db.createObjectStore(REVOCATION_META, { keyPath: 'key' })
    }
    // A tab on the older code holds version 2 open. The caller fails closed now.
    open.onblocked = () => giveUp(() => new OccurrenceStoreBlockedError())
    open.onsuccess = () => {
      stopTimer()
      // A success at or after the limit is a timeout, also when the timer has
      // not fired yet. A clock value that is not a number refuses too.
      if (!gaveUp && !(openClock() < expiresAt)) giveUp(timeout)
      if (!gaveUp) return resolve(open.result)
      abandonedOpens.delete(dbName)
      open.result.close()
    }
    open.onerror = () => {
      stopTimer()
      if (gaveUp) abandonedOpens.delete(dbName)
      reject(open.error)
    }
  })
}

export async function openOccurrenceStore(
  dbName = 'gate-occurrences',
  options: OccurrenceStoreOptions = {},
): Promise<OccurrenceStore> {
  const ownerId = options.ownerId ?? crypto.randomUUID()
  const pendingLeaseMs = options.pendingLeaseMs ?? DEFAULT_PENDING_LEASE_MS
  const now = options.now ?? Date.now
  const ownerLiveness = options.ownerLiveness ?? browserOwnerLiveness()
  if (!ownerId || !Number.isSafeInteger(pendingLeaseMs) || pendingLeaseMs <= 0) {
    throw new TypeError('occurrence owner and pending lease must be valid')
  }
  const db = await openDatabase(dbName, options.openTimer ?? defaultOpenTimer, options.openClock ?? defaultOpenClock)

  // Set by close(), and by a later version that asks this connection to close.
  // Every operation checks it, so a closed store refuses with one known error.
  let closed = false
  let heartbeat: ReturnType<typeof globalThis.setInterval> | undefined
  let releaseOwner: (() => void) | undefined
  const close = () => {
    if (closed) return
    closed = true
    globalThis.clearInterval(heartbeat)
    db.close()
    releaseOwner?.()
  }
  // A later version cannot open while this connection stays open. Close at once.
  // The later version can still wait for transactions that were already running
  // here, but not for new ones: every new operation then fails closed.
  db.onversionchange = close
  const assertOpen = () => {
    if (closed) throw new OccurrenceStoreClosedError()
  }

  const recoverRecord = async (occurrenceId: string, recoverOwnerId?: string): Promise<void> => {
    const tx = db.transaction(STORE, 'readwrite')
    const done = transactionDone(tx)
    const os = tx.objectStore(STORE)
    const record = await requestDone(os.get(occurrenceId) as IDBRequest<OccurrenceRecord | undefined>)
    const timestamp = now()
    if (record?.state === 'PENDING' && record.ownerId !== ownerId) {
      const belongsToReloadedTab = recoverOwnerId !== undefined && record.ownerId === recoverOwnerId
      const leaseExpired = typeof record.leaseExpiresAt !== 'number'
        || !Number.isFinite(record.leaseExpiresAt)
        || record.leaseExpiresAt <= timestamp
      if (belongsToReloadedTab || leaseExpired) os.put({ ...record, state: 'QUEUED' })
    }
    await done
  }

  const recoverPending = async (recoverOwnerId?: string): Promise<void> => {
    assertOpen()
    const tx = db.transaction(STORE, 'readonly')
    const existing = await requestDone(tx.objectStore(STORE).getAll() as IDBRequest<OccurrenceRecord[]>)
    for (const record of existing) {
      if (record.state !== 'PENDING' || record.ownerId === ownerId) continue
      if (record.ownerId === undefined) {
        await recoverRecord(record.occurrenceId, recoverOwnerId)
      } else {
        await ownerLiveness.runIfOwnerAbsent(record.ownerId, () =>
          recoverRecord(record.occurrenceId, recoverOwnerId))
      }
    }
  }

  try {
    releaseOwner = await ownerLiveness.hold(ownerId)
  } catch (cause) {
    close()
    throw cause
  }
  // A version change during hold() closed the store before it held the owner.
  if (closed) {
    releaseOwner()
    throw new OccurrenceStoreClosedError()
  }

  // A reload gets a new page-owner id but carries the old id through
  // sessionStorage, so it can recover its own interrupted request immediately.
  // A foreign row needs an expired lease and an absent browser lock.
  try {
    await recoverPending(options.recoverOwnerId)
    assertOpen()
  } catch (cause) {
    close()
    throw cause
  }

  const renewOwnedPending = async (): Promise<void> => {
    if (closed) return
    const tx = db.transaction(STORE, 'readwrite')
    const done = transactionDone(tx)
    const os = tx.objectStore(STORE)
    const existing = await requestDone(os.getAll() as IDBRequest<OccurrenceRecord[]>)
    const leaseExpiresAt = now() + pendingLeaseMs
    for (const record of existing) {
      if (record.state === 'PENDING' && record.ownerId === ownerId) {
        os.put({ ...record, leaseExpiresAt })
      }
    }
    await done
  }
  heartbeat = globalThis.setInterval(() => {
    void renewOwnedPending().catch(() => {
      // Foreground operations report IndexedDB failures. The browser lock still
      // prevents another tab from taking this owner's row after a missed renewal.
    })
  }, Math.max(1, Math.floor(pendingLeaseMs / 3)))

  const put = async (record: OccurrenceRecord): Promise<void> => {
    assertOpen()
    const tx = db.transaction(STORE, 'readwrite')
    tx.objectStore(STORE).put(record)
    await transactionDone(tx)
  }
  const read = async (occurrenceId: string): Promise<OccurrenceRecord | undefined> => {
    assertOpen()
    const tx = db.transaction(STORE, 'readonly')
    return requestDone(tx.objectStore(STORE).get(occurrenceId) as IDBRequest<OccurrenceRecord | undefined>)
  }
  const transition = async (
    occurrenceId: string,
    apply: (record: OccurrenceRecord) => OccurrenceRecord | null,
  ): Promise<boolean> => {
    // One readwrite transaction covers the get and the put: IDB serializes
    // same-store readwrite transactions, so two racers cannot both see the
    // pre-transition record.
    assertOpen()
    const tx = db.transaction(STORE, 'readwrite')
    const os = tx.objectStore(STORE)
    const record = (await requestDone(os.get(occurrenceId) as IDBRequest<OccurrenceRecord | undefined>)) ?? null
    const next = record ? apply(record) : null
    if (next) os.put(next)
    await transactionDone(tx)
    return next !== null
  }

  return {
    async mint(qrPayload, occurredAt, direction) {
      const record: MintedOccurrence = {
        occurrenceId: crypto.randomUUID(),
        qrPayload,
        occurredAt,
        state: 'PENDING',
        actuated: false,
        createdAt: new Date().toISOString(),
        ownerId,
        leaseExpiresAt: now() + pendingLeaseMs,
        direction,
      }
      await put(record)
      return record
    },
    async mintRefusal(qrPayload, occurredAt) {
      const record: OccurrenceRecord = {
        occurrenceId: crypto.randomUUID(),
        qrPayload,
        occurredAt,
        state: 'QUEUED',
        actuated: false,
        createdAt: new Date().toISOString(),
        localDecision: 'revocation_refused',
      }
      await put(record)
      return record
    },
    async isRevoked(ticketID) {
      assertOpen()
      const tx = db.transaction(REVOCATIONS, 'readonly')
      return (await requestDone(tx.objectStore(REVOCATIONS).get(ticketID.toLowerCase()) as IDBRequest<unknown>)) !== undefined
    },
    async mergeRevoked(ticketIDs) {
      assertOpen()
      const tx = db.transaction(REVOCATIONS, 'readwrite')
      for (const ticketID of ticketIDs) tx.objectStore(REVOCATIONS).put({ ticketID: ticketID.toLowerCase() })
      await transactionDone(tx)
    },
    async completeRevocationPull(fingerprint, completedAt) {
      assertOpen()
      const tx = db.transaction(REVOCATION_META, 'readwrite')
      const os = tx.objectStore(REVOCATION_META)
      os.put({ key: `pull:${fingerprint}`, completedAt } satisfies RevocationMeta)
      await transactionDone(tx)
    },
    async revocationPull(fingerprint) {
      assertOpen()
      const tx = db.transaction(REVOCATION_META, 'readonly')
      const meta = await requestDone(tx.objectStore(REVOCATION_META).get(`pull:${fingerprint}`) as IDBRequest<RevocationMeta | undefined>)
      await transactionDone(tx)
      return { completedAt: meta?.completedAt }
    },
    actuate(occurrenceId) {
      return transition(occurrenceId, (record) =>
        record.actuated || record.state !== 'PENDING' || record.ownerId !== ownerId
          ? null
          : { ...record, actuated: true, state: 'ACTUATED' },
      )
    },
    async markQueued(occurrenceId) {
      await transition(occurrenceId, (record) => ({ ...record, state: 'QUEUED' }))
    },
    async markSynced(occurrenceId, result) {
      const terminal: OccurrenceState = result === 'recorded' || result === 'synced' ? 'SYNCED' : 'RESOLVED'
      await transition(occurrenceId, (record) => ({ ...record, state: terminal, result }))
    },
    async queued() {
      await recoverPending(options.recoverOwnerId)
      const tx = db.transaction(STORE, 'readonly')
      const all = await requestDone(tx.objectStore(STORE).getAll() as IDBRequest<OccurrenceRecord[]>)
      return all.filter((record) => record.state === 'QUEUED')
    },
    get: read,
    close,
  }
}
