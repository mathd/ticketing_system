// Raw IndexedDB access for fixtures and exact readback. It opens the current
// version, so it never triggers a version change on a page's connection.

import { requestDone } from '../occurrence-store'

export async function openRaw(dbName: string, version?: number, upgrade?: (db: IDBDatabase, tx: IDBTransaction, oldVersion: number) => void): Promise<IDBDatabase> {
  const request = version === undefined ? indexedDB.open(dbName) : indexedDB.open(dbName, version)
  if (upgrade) request.onupgradeneeded = (event) => upgrade(request.result, request.transaction!, event.oldVersion)
  return requestDone(request as IDBRequest<IDBDatabase>)
}

async function withRaw<T>(dbName: string, body: (db: IDBDatabase) => Promise<T>): Promise<T> {
  const db = await openRaw(dbName)
  try {
    return await body(db)
  } finally {
    db.close()
  }
}

function txDone(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve()
    tx.onabort = () => reject(tx.error ?? new Error('aborted'))
  })
}

export function rawGet(dbName: string, store: string, key: IDBValidKey): Promise<unknown> {
  return withRaw(dbName, (db) => requestDone(db.transaction(store).objectStore(store).get(key)))
}

export function rawGetAll(dbName: string, store: string): Promise<unknown[]> {
  return withRaw(dbName, (db) => requestDone(db.transaction(store).objectStore(store).getAll()))
}

export function rawPut(dbName: string, store: string, value: unknown): Promise<void> {
  return withRaw(dbName, async (db) => {
    const tx = db.transaction(store, 'readwrite')
    tx.objectStore(store).put(value)
    await txDone(tx)
  })
}

export function rawStoreNames(dbName: string): Promise<string[]> {
  return withRaw(dbName, async (db) => [...db.objectStoreNames].sort())
}

/** Creates a database at an old version with its literal stores and rows. */
export async function seedDatabase(
  dbName: string,
  version: number,
  stores: Array<{ name: string; keyPath: string }>,
  rows: Record<string, unknown[]>,
): Promise<void> {
  const db = await openRaw(dbName, version, (created) => {
    for (const { name, keyPath } of stores) {
      if (!created.objectStoreNames.contains(name)) created.createObjectStore(name, { keyPath })
    }
  })
  try {
    for (const [store, values] of Object.entries(rows)) {
      const tx = db.transaction(store, 'readwrite')
      for (const value of values) tx.objectStore(store).put(value)
      await txDone(tx)
    }
  } finally {
    db.close()
  }
}

export const V1_STORES = [{ name: 'occurrences', keyPath: 'occurrenceId' }]
export const V2_STORES = [
  { name: 'occurrences', keyPath: 'occurrenceId' },
  { name: 'revocations', keyPath: 'ticketID' },
  { name: 'revocation_meta', keyPath: 'key' },
]
