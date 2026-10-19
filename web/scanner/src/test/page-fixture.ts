// Test page fixture. It builds the production coordinator, lock adapter and
// storage controller. The only test-specific input is the page lifetime: the
// fixture ends it in destroy(), which is the one simulated page loss. Closing a
// connection, a failed write or a version change never ends it.

import { createPageCoordinator, type CapturedOccurrence, type PageCoordinator } from '../occurrence-coordinator'
import { createBrowserOwnerLock } from '../occurrence-lock'
import { createStorageController, type StorageController, type StorageReadiness } from '../occurrence-readiness'
import type { OccurrenceStore } from '../occurrence-store'
import { captured } from './scans'

export { captured }

export const BOUND_MS = 2_000

export async function bounded<T>(promise: Promise<T>, label: string, ms = BOUND_MS): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`bounded wait expired: ${label}`)), ms)
      }),
    ])
  } finally {
    clearTimeout(timer)
  }
}

export async function eventually(check: () => boolean, label: string, ms = BOUND_MS): Promise<void> {
  const deadline = Date.now() + ms
  while (!check()) {
    if (Date.now() > deadline) throw new Error(`bounded wait expired: ${label}`)
    await new Promise((resolve) => setTimeout(resolve, 1))
  }
}

/** Lets pending IndexedDB and promise work run for a short real time. Not a synchronization barrier. */
export const settle = (ms = 20) => new Promise((resolve) => setTimeout(resolve, ms))

export type TestPage = {
  coordinator: PageCoordinator
  storage: StorageController
  states: StorageReadiness[]
  ready(): Promise<OccurrenceStore>
  store(): OccurrenceStore
  waitFor(status: StorageReadiness['status'], label?: string): Promise<StorageReadiness>
  destroy(): void
  destroyed: boolean
}

export type TestPageOptions = {
  dbName: string
  ownerId?: string
  recoverOwnerId?: string
  now?: () => number
  leaseMs?: number
  renewalMs?: number
}

const pages: TestPage[] = []

export function createTestPage(options: TestPageOptions): TestPage {
  let endLife!: () => void
  const pageLifetime = new Promise<void>((resolve) => { endLife = resolve })
  const coordinator = createPageCoordinator({
    owner: { ownerId: options.ownerId ?? crypto.randomUUID(), recoverOwnerId: options.recoverOwnerId },
    lock: createBrowserOwnerLock({ pageLifetime }),
  })
  const storage = createStorageController({
    coordinator,
    dbName: options.dbName,
    now: options.now,
    leaseMs: options.leaseMs,
    renewalMs: options.renewalMs,
  })
  const states: StorageReadiness[] = []
  storage.subscribe((state) => states.push(state))

  const page: TestPage = {
    coordinator,
    storage,
    states,
    destroyed: false,
    async waitFor(status, label = status) {
      await eventually(() => storage.state().status === status, `${options.ownerId ?? 'page'} ${label}`)
      return storage.state()
    },
    async ready() {
      storage.start()
      await page.waitFor('ready')
      return page.store()
    },
    store() {
      const store = storage.readyStore()
      if (!store) throw new Error(`storage is ${storage.state().status}, not ready`)
      return store
    },
    destroy() {
      if (page.destroyed) return
      page.destroyed = true
      storage.stop()
      endLife()
    },
  }
  pages.push(page)
  return page
}

/** Ends every fixture page created by the current test. Call in afterEach. */
export function destroyAllPages() {
  for (const page of pages.splice(0)) page.destroy()
}

/** Registers and mints one occurrence through the production path. */
export async function mintOn(page: TestPage, overrides: Partial<CapturedOccurrence> = {}) {
  const scan = captured(overrides)
  page.coordinator.register(scan)
  const store = page.store()
  const record = scan.type === 'refusal' ? await store.mintRefusal(scan.occurrenceId) : await store.mintNormal(scan.occurrenceId)
  return { scan, record }
}

export async function lockAvailable(name: string): Promise<boolean> {
  return navigator.locks.request(name, { ifAvailable: true }, (lock) => lock !== null)
}
