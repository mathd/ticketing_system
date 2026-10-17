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
  /**
   * Commits the PENDING record; resolves only after the IDB transaction completes.
   * The row is in flight for the page from before the write until its outcome
   * is written or decided (TKT-399 D35). See PageScans.
   */
  mint(qrPayload: string, occurredAt: string, direction: ScanDirection): Promise<MintedOccurrence>
  /**
   * Commits a local refusal as a PENDING record of this page, like mint. It
   * never actuates. The page queues it with markQueued after its time check.
   */
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
  /**
   * Declares that the scan of this row ran out of time, then closes the row
   * with `result` (TKT-399 D30). The declaration is made before the write and
   * stays for the page when the write fails. See PageScans.
   */
  closeLate(occurrenceId: string, result: string): Promise<void>
  /** The QUEUED rows to reconcile. Never a row that this page declared late. */
  queued(): Promise<OccurrenceRecord[]>
  get(occurrenceId: string): Promise<OccurrenceRecord | undefined>
  close(): void
}

const STORE = 'occurrences'
const REVOCATIONS = 'revocations'
const REVOCATION_META = 'revocation_meta'
type RevocationMeta = { key: `pull:${string}`; completedAt: string }
const DEFAULT_PENDING_LEASE_MS = 30_000

const terminalState = (result: string): OccurrenceState =>
  result === 'recorded' || result === 'synced' ? 'SYNCED' : 'RESOLVED'

/**
 * The scans of one page that are not decided yet, and the rows that it
 * declared late (TKT-399 D30, D35). A scan that the time limit stopped is never
 * synced to Access, also when the write that closes its row fails. The page
 * keeps one instance for its life and gives it to every store it opens, because
 * a failed write makes the page replace its store.
 *
 * A row is in flight from its mint until the page decides its outcome:
 * - A store records the row before the write of the mint. A mint write that
 *   fails wrote no row, so the record goes.
 * - The record goes when a store writes the outcome: the actuation (open or
 *   not), the queue write, or the terminal write.
 * - The page calls decided() when it decided an outcome in time and the write
 *   of that outcome failed. Recovery then queues the row, which is that outcome.
 * - A scan that ran out of time declares the row late. A declared row stays
 *   open until a store of this page writes its close.
 *
 * Each store of the page applies the record:
 * - close() keeps the owner lock while that owner has a row in flight or a
 *   declared row that is not closed. Recovery in any tab, this page too, needs
 *   that lock, so no tab can queue the row while this page lives.
 * - Recovery closes a declared row and never queues it.
 * - queued() never lists a declared row, and first tries again to close it.
 *
 * The record is in memory. When the page is gone, the browser releases its
 * locks, and a row that is open is recovered like the row of a page that
 * crashed (ADR-025 §3).
 */
export class PageScans {
  private readonly inFlight = new Map<string, string>()
  private readonly declared = new Map<string, { ownerId: string; result: string; closed: boolean }>()
  private readonly heldOwners = new Map<string, Array<() => void>>()

  /** A store of this page mints this row. Call before the write of the mint. */
  minted(occurrenceId: string, ownerId: string): void {
    if (!this.declared.has(occurrenceId)) this.inFlight.set(occurrenceId, ownerId)
  }

  /**
   * The outcome of this row is decided and is not late. A declared row stays
   * declared. Call it also when the write of the outcome failed, if recovery
   * of the row gives the same outcome.
   */
  decided(occurrenceId: string): void {
    const ownerId = this.inFlight.get(occurrenceId)
    if (ownerId === undefined) return
    this.inFlight.delete(occurrenceId)
    this.releaseIfDone(ownerId)
  }

  declare(occurrenceId: string, ownerId: string, result: string): void {
    if (this.declared.has(occurrenceId)) return
    // The lock to keep is that of the owner that minted the row.
    const minter = this.inFlight.get(occurrenceId) ?? ownerId
    this.inFlight.delete(occurrenceId)
    this.declared.set(occurrenceId, { ownerId: minter, result, closed: false })
  }

  /** The terminal result of a declared row, or undefined if the row is not declared. */
  result(occurrenceId: string): string | undefined {
    return this.declared.get(occurrenceId)?.result
  }

  /** Declared rows whose close has not been written yet. */
  unclosed(): Array<{ occurrenceId: string; result: string }> {
    return [...this.declared]
      .filter(([, entry]) => !entry.closed)
      .map(([occurrenceId, entry]) => ({ occurrenceId, result: entry.result }))
  }

  /** Records that the close was written, and lets the owner go when it has no other open row. */
  closed(occurrenceId: string): void {
    const entry = this.declared.get(occurrenceId)
    if (!entry || entry.closed) return
    entry.closed = true
    this.releaseIfDone(entry.ownerId)
  }

  /** Releases the owner lock now, or when the last open row of that owner is decided or closed. */
  releaseOwnerWhenDone(ownerId: string, release: () => void): void {
    if (!this.ownerHasOpenRows(ownerId)) {
      release()
      return
    }
    this.heldOwners.set(ownerId, [...(this.heldOwners.get(ownerId) ?? []), release])
  }

  /** The page is going away: let every owner go. */
  releaseOwners(): void {
    const releases = [...this.heldOwners.values()].flat()
    this.heldOwners.clear()
    for (const release of releases) release()
  }

  private releaseIfDone(ownerId: string): void {
    if (this.ownerHasOpenRows(ownerId)) return
    const releases = this.heldOwners.get(ownerId) ?? []
    this.heldOwners.delete(ownerId)
    for (const release of releases) release()
  }

  private ownerHasOpenRows(ownerId: string): boolean {
    for (const owner of this.inFlight.values()) {
      if (owner === ownerId) return true
    }
    for (const entry of this.declared.values()) {
      if (entry.ownerId === ownerId && !entry.closed) return true
    }
    return false
  }
}

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
  /**
   * The scans of this page that are in flight or declared late. Shared by
   * every store of one page. A store without it stands alone: its close() lets
   * its owner go at once, as when its page is gone.
   */
  pageScans?: PageScans
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
  const pageScans = options.pageScans ?? new PageScans()
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
    if (!releaseOwner) return
    // While this page lives, keep the owner of each row that is in flight or
    // declared late and not closed (D35).
    if (options.pageScans) pageScans.releaseOwnerWhenDone(ownerId, releaseOwner)
    else releaseOwner()
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
    // A row that this page declared late is closed, never queued (D30).
    const late = pageScans.result(occurrenceId)
    let closedLate = false
    if (record?.state === 'PENDING' && record.ownerId !== ownerId) {
      const belongsToReloadedTab = recoverOwnerId !== undefined && record.ownerId === recoverOwnerId
      const leaseExpired = typeof record.leaseExpiresAt !== 'number'
        || !Number.isFinite(record.leaseExpiresAt)
        || record.leaseExpiresAt <= timestamp
      if (belongsToReloadedTab || leaseExpired) {
        closedLate = late !== undefined
        os.put(late === undefined ? { ...record, state: 'QUEUED' } : { ...record, state: terminalState(late), result: late })
      }
    }
    await done
    if (closedLate) pageScans.closed(occurrenceId)
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

  // Writes the close of a declared row. Any state becomes the terminal one:
  // the row may still be PENDING, or a recovery may have queued it before the
  // scan declared it late.
  const closeDeclared = async (occurrenceId: string, result: string): Promise<void> => {
    await transition(occurrenceId, (record) => ({ ...record, state: terminalState(result), result }))
    pageScans.closed(occurrenceId)
  }

  // Writes a new row of this page. The row is in flight before the write, so a
  // close() during the write keeps its owner. A write that fails wrote no row.
  const putMinted = async (record: OccurrenceRecord): Promise<void> => {
    pageScans.minted(record.occurrenceId, ownerId)
    try {
      await put(record)
    } catch (cause) {
      pageScans.decided(record.occurrenceId)
      throw cause
    }
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
      await putMinted(record)
      return record
    },
    async mintRefusal(qrPayload, occurredAt) {
      // PENDING under this page, like a scan: until the page has checked the
      // time of the mint, no other tab may queue it (D30). A page that crashed
      // first leaves it to recovery, which queues it as a refusal.
      const record: OccurrenceRecord = {
        occurrenceId: crypto.randomUUID(),
        qrPayload,
        occurredAt,
        state: 'PENDING',
        actuated: false,
        createdAt: new Date().toISOString(),
        ownerId,
        leaseExpiresAt: now() + pendingLeaseMs,
        localDecision: 'revocation_refused',
      }
      await putMinted(record)
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
    async actuate(occurrenceId) {
      // A local refusal is PENDING too, but it never opens the gate.
      const opened = await transition(occurrenceId, (record) =>
        record.actuated || record.state !== 'PENDING' || record.ownerId !== ownerId || record.localDecision !== undefined
          ? null
          : { ...record, actuated: true, state: 'ACTUATED' },
      )
      // Decided either way: the row is ACTUATED now, or it was not PENDING for
      // this page. A failure above leaves the row in flight.
      pageScans.decided(occurrenceId)
      return opened
    },
    async markQueued(occurrenceId) {
      await transition(occurrenceId, (record) => ({ ...record, state: 'QUEUED' }))
      pageScans.decided(occurrenceId)
    },
    async markSynced(occurrenceId, result) {
      const terminal = terminalState(result)
      await transition(occurrenceId, (record) => ({ ...record, state: terminal, result }))
      pageScans.decided(occurrenceId)
    },
    async closeLate(occurrenceId, result) {
      // Declared first, with no wait before it, so a failed write below still
      // leaves the declaration, and a closed store still makes it.
      pageScans.declare(occurrenceId, ownerId, result)
      await closeDeclared(occurrenceId, result)
    },
    async queued() {
      assertOpen()
      // Try again to close each declared row. A failure here does not stop the
      // listing: the filter below keeps the row out in any case.
      for (const { occurrenceId, result } of pageScans.unclosed()) {
        try {
          await closeDeclared(occurrenceId, result)
        } catch {
          // The next listing tries again.
        }
      }
      await recoverPending(options.recoverOwnerId)
      const tx = db.transaction(STORE, 'readonly')
      const all = await requestDone(tx.objectStore(STORE).getAll() as IDBRequest<OccurrenceRecord[]>)
      return all.filter((record) => record.state === 'QUEUED' && pageScans.result(record.occurrenceId) === undefined)
    },
    get: read,
    close,
  }
}
