// Durable occurrence queue + actuation state (ADR-025 §D3/§D6).
//
// Every physical gate decision mints a UUIDv4 occurrence id, committed to
// IndexedDB BEFORE the request is sent; a response — first-time or replay —
// may actuate only while this device holds a pending, never-actuated record
// for that id, and the record is marked actuated BEFORE the accepted screen
// renders (fail-closed). IndexedDB over localStorage because the
// PENDING→ACTUATED transition needs a transactional read-modify-write: a
// racing duplicate response must not observe actuated:false twice.

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
}

export interface OccurrenceStore {
  /** Commits the PENDING record; resolves only after the IDB transaction completes. */
  mint(qrPayload: string, occurredAt: string): Promise<OccurrenceRecord>
  /**
   * Atomic PENDING-and-never-actuated → ACTUATED transition. Resolves true iff
   * THIS call performed it — the one signal that may open the gate.
   */
  actuate(occurrenceId: string): Promise<boolean>
  markQueued(occurrenceId: string): Promise<void>
  /** recorded/synced → SYNCED; conflict/rejected → RESOLVED (operator-facing, never a gate action). */
  markSynced(occurrenceId: string, result: string): Promise<void>
  queued(): Promise<OccurrenceRecord[]>
  get(occurrenceId: string): Promise<OccurrenceRecord | undefined>
  close(): void
}

const STORE = 'occurrences'
const DEFAULT_PENDING_LEASE_MS = 30_000

export type OccurrenceStoreOptions = {
  ownerId?: string
  /** Previous owner from this tab, supplied only after an actual page reload. */
  recoverOwnerId?: string
  pendingLeaseMs?: number
  now?: () => number
  ownerLiveness?: OwnerLiveness
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
  const open = indexedDB.open(dbName, 1)
  open.onupgradeneeded = () => {
    open.result.createObjectStore(STORE, { keyPath: 'occurrenceId' })
  }
  const db = await requestDone(open as IDBRequest<IDBDatabase>)

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

  let releaseOwner: () => void
  try {
    releaseOwner = await ownerLiveness.hold(ownerId)
  } catch (cause) {
    db.close()
    throw cause
  }

  // A reload gets a new page-owner id but carries the old id through
  // sessionStorage, so it can recover its own interrupted request immediately.
  // A foreign row needs an expired lease and an absent browser lock.
  try {
    await recoverPending(options.recoverOwnerId)
  } catch (cause) {
    releaseOwner()
    db.close()
    throw cause
  }

  let closed = false
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
  const heartbeat = globalThis.setInterval(() => {
    void renewOwnedPending().catch(() => {
      // Foreground operations report IndexedDB failures. The browser lock still
      // prevents another tab from taking this owner's row after a missed renewal.
    })
  }, Math.max(1, Math.floor(pendingLeaseMs / 3)))

  const put = async (record: OccurrenceRecord): Promise<void> => {
    const tx = db.transaction(STORE, 'readwrite')
    tx.objectStore(STORE).put(record)
    await transactionDone(tx)
  }
  const read = async (occurrenceId: string): Promise<OccurrenceRecord | undefined> => {
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
    const tx = db.transaction(STORE, 'readwrite')
    const os = tx.objectStore(STORE)
    const record = (await requestDone(os.get(occurrenceId) as IDBRequest<OccurrenceRecord | undefined>)) ?? null
    const next = record ? apply(record) : null
    if (next) os.put(next)
    await transactionDone(tx)
    return next !== null
  }

  return {
    async mint(qrPayload, occurredAt) {
      const record: OccurrenceRecord = {
        occurrenceId: crypto.randomUUID(),
        qrPayload,
        occurredAt,
        state: 'PENDING',
        actuated: false,
        createdAt: new Date().toISOString(),
        ownerId,
        leaseExpiresAt: now() + pendingLeaseMs,
      }
      await put(record)
      return record
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
    close() {
      if (closed) return
      closed = true
      globalThis.clearInterval(heartbeat)
      db.close()
      releaseOwner()
    },
  }
}
