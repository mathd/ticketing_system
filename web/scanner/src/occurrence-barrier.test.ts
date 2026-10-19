import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ownerLockName } from './occurrence-lock'
import type { StorageController, StorageReadiness } from './occurrence-readiness'
import { STORE } from './occurrence-store'
import * as frozen from './test/fixtures/occurrences-v2.frozen'
import frozenSource from './test/fixtures/occurrences-v2.frozen.ts?raw'
import manifestSource from './test/fixtures/occurrences-v2.manifest.json?raw'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { bounded, captured, eventually, settle } from './test/page-fixture'
import { openRaw, rawGetAll } from './test/raw-db'

// MX tier (COS-5): one jsdom realm with a shared IndexedDB factory and lock
// manager. The old page runs the frozen version-2 source byte for byte. Each new
// page is an isolated import of the production modules, so each has its own
// page coordinator singleton, owner and sessionStorage. This is not cross-page
// browser proof; browser spec B3 owns that.

const DB = 'gate-occurrences'

class MemoryStorage implements Storage {
  private readonly values = new Map<string, string>()
  get length() { return this.values.size }
  clear() { this.values.clear() }
  getItem(key: string) { return this.values.get(key) ?? null }
  key(index: number) { return [...this.values.keys()][index] ?? null }
  removeItem(key: string) { this.values.delete(key) }
  setItem(key: string, value: string) { this.values.set(key, String(value)) }
}

type IsolatedPage = { storage: StorageController; states: StorageReadiness[] }
const loaded: IsolatedPage[] = []
const oldStores: frozen.OccurrenceStore[] = []
let instrument: Instrument | undefined

/** Imports a fresh instance of the production modules, as one new page would load them. */
async function loadPage(): Promise<IsolatedPage> {
  vi.resetModules()
  vi.stubGlobal('sessionStorage', new MemoryStorage())
  const production = await import('./occurrence-readiness')
  const page = { storage: production.pageStorage, states: [] as StorageReadiness[] }
  page.storage.subscribe((state) => page.states.push(state))
  loaded.push(page)
  return page
}

async function openOld(ownerId: string) {
  const store = await frozen.openOccurrenceStore(DB, { ownerId })
  oldStores.push(store)
  return store
}

const isReady = (page: IsolatedPage) => page.storage.state().status === 'ready'

beforeEach(() => {
  vi.stubGlobal('indexedDB', new IDBFactory())
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  for (const page of loaded.splice(0)) page.storage.stop()
  for (const store of oldStores.splice(0)) store.close()
  vi.unstubAllGlobals()
})

describe('frozen version-2 reader (MX-01)', () => {
  it('is the unchanged baseline source', async () => {
    const manifest = JSON.parse(manifestSource) as { sha256: string; baselineCommit: string; databaseVersion: number }
    const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(frozenSource))
    const digest = Array.from(new Uint8Array(bytes), (byte) => byte.toString(16).padStart(2, '0')).join('')
    expect([digest, manifest.baselineCommit, manifest.databaseVersion]).toEqual([
      '0b8ea0c89c4f75ece7ce306d27d1a28ec4824365adef154faac172c624413bfc',
      'ec336db814d719130cdefd263ca84be73b9f06e2',
      2,
    ])
    expect(digest).toBe(manifest.sha256)
  })
})

describe('old reader, new A, new B (COS-5)', () => {
  it('keeps both new pages not ready while the old reader is open, then upgrades without replaying input (I5-01, RD-02, RD-03)', async () => {
    const old = await openOld('old-page')
    const legacy = await old.mint('legacy-qr', '2026-10-18T08:00:00.000Z')
    const a = await loadPage()
    const b = await loadPage()
    a.storage.start()
    b.storage.start()
    await eventually(() => a.states.some((state) => state.status === 'opening' && state.blocked), 'A blocked')
    await settle(30)
    for (const page of [a, b]) {
      expect(page.storage.state().status).toBe('opening')
      expect(page.storage.readyStore()).toBeNull()
    }
    // The old page still works on version 2 and sees only its own rows.
    expect(await old.queued()).toEqual([])
    expect(await old.get(legacy.occurrenceId)).toMatchObject({ state: 'PENDING', ownerId: 'old-page' })

    old.close()
    await eventually(() => isReady(a) && isReady(b), 'A and B ready')
    // No stopped input was stored or replayed: only the held legacy row exists.
    expect(await rawGetAll(DB, STORE.occurrences)).toStrictEqual([await rawLegacy(legacy.occurrenceId)])
    expect(await rawGetAll(DB, STORE.legacyClasses)).toEqual([{ occurrenceId: legacy.occurrenceId, legacyClass: 'M01', sourceVersion: 2 }])
    // A fresh presentation on A works.
    const scan = captured()
    a.storage.coordinator.register(scan)
    await a.storage.readyStore()!.mintNormal(scan.occurrenceId)
    expect((await rawGetAll(DB, STORE.occurrences)).length).toBe(2)
  })

  it('bars the old reader after the upgrade, so it never reads a new-format row (I5-06)', async () => {
    const old = await openOld('old-page')
    old.close()
    const a = await loadPage()
    a.storage.start()
    await eventually(() => isReady(a), 'A ready')
    const scan = captured()
    const coordinator = a.storage.coordinator
    coordinator.register(scan)
    await a.storage.readyStore()!.mintNormal(scan.occurrenceId)
    coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-18T09:00:00.000Z' })
    await coordinator.persist(scan.occurrenceId)
    expect((await a.storage.readyStore()!.queued()).entries.map((entry) => entry.key)).toEqual([scan.occurrenceId])
    await expect(frozen.openOccurrenceStore(DB, { ownerId: 'old-page-again' })).rejects.toMatchObject({ name: 'VersionError' })
  })
})

describe('readiness prerequisites', () => {
  it('stays not ready until the page owner lock is granted (RD-01a)', async () => {
    const a = await loadPage()
    let endHold!: () => void
    const held = new Promise<void>((resolve) => { endHold = resolve })
    void navigator.locks.request(ownerLockName(a.storage.coordinator.ownerId), () => held)
    await settle(5)
    a.storage.start()
    await eventually(() => a.states.some((state) => state.status === 'opening'
      && state.progress.opened && state.progress.recovered && state.progress.reads && state.progress.repairs), 'all but the lock')
    await settle(20)
    expect(a.storage.state()).toMatchObject({ status: 'opening', progress: { lock: false } })
    expect(a.storage.readyStore()).toBeNull()
    endHold()
    await eventually(() => isReady(a), 'A ready after the lock')
  })

  it.each([
    ['recovery', [STORE.occurrences, STORE.legacyClasses]],
    ['held read', [STORE.occurrences, STORE.legacyClasses, STORE.heldDisposals]],
    ['notice read', [STORE.recoveryNotices, STORE.occurrences]],
  ])('stays not ready while the %s is held (RD-01b)', async (_name, stores) => {
    const a = await loadPage()
    instrument = instrumentTransactions()
    const hold = instrument.hold({ stores, mode: 'readonly' })
    a.storage.start()
    await bounded(hold.started, 'held prerequisite')
    await settle(20)
    expect(a.storage.state().status).toBe('opening')
    expect(a.storage.readyStore()).toBeNull()
    hold.release()
    await eventually(() => isReady(a), 'A ready after release')
  })
})

describe('abandoned opens (RD-04, RD-05)', () => {
  it('closes a late success from an abandoned generation and never publishes it as ready', async () => {
    const old = await openOld('old-page')
    const a = await loadPage()
    a.storage.start()
    await eventually(() => a.states.some((state) => state.status === 'opening' && state.blocked), 'generation 1 blocked')
    a.storage.retry()
    await eventually(() => a.storage.state().status === 'opening' && (a.storage.state() as { generation: number }).generation === 2, 'generation 2 opening')
    old.close()
    await eventually(() => isReady(a), 'generation 2 ready')
    expect(a.storage.state()).toEqual({ status: 'ready', generation: 2 })
    expect(a.states.filter((state) => state.status === 'ready')).toEqual([{ status: 'ready', generation: 2 }])
    a.storage.stop()
    // No leaked connection: a later upgrade proceeds.
    const later = await bounded(openRaw(DB, 4), 'version-4 open after the abandoned generation')
    expect(later.version).toBe(4)
    later.close()
  })
})

async function rawLegacy(occurrenceId: string) {
  const all = await rawGetAll(DB, STORE.occurrences) as Array<{ occurrenceId: string }>
  return all.find((row) => row.occurrenceId === occurrenceId)
}
