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
}

const STORE = 'occurrences'

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

export async function openOccurrenceStore(dbName = 'gate-occurrences'): Promise<OccurrenceStore> {
  const open = indexedDB.open(dbName, 1)
  open.onupgradeneeded = () => {
    open.result.createObjectStore(STORE, { keyPath: 'occurrenceId' })
  }
  const db = await requestDone(open as IDBRequest<IDBDatabase>)

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
      }
      await put(record)
      return record
    },
    actuate(occurrenceId) {
      return transition(occurrenceId, (record) =>
        record.actuated || record.state !== 'PENDING' ? null : { ...record, actuated: true, state: 'ACTUATED' },
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
      const tx = db.transaction(STORE, 'readonly')
      const all = await requestDone(tx.objectStore(STORE).getAll() as IDBRequest<OccurrenceRecord[]>)
      return all.filter((record) => record.state === 'QUEUED')
    },
    get: read,
  }
}
