// The page owner lock (ADR-085 §2.1, I2).
//
// One page holds one exclusive browser lock for its whole JavaScript life. A
// connection close, a failed write, a React cleanup or a version change does not
// release it. Only the end of the page releases it, and then the browser does.
// Another tab may recover this page's PENDING rows only while this lock is absent.

export const ownerLockName = (ownerId: string) => `ticketing.scanner.occurrence-owner.${ownerId}`

export type OwnerAbsentResult<T> = { ran: true; value: T } | { ran: false }

export interface OwnerLock {
  /** Resolves when the browser grants this page's owner lock. The lock stays held for the page lifetime. */
  holdForPageLifetime(ownerId: string): Promise<void>
  /** Runs the task while it holds another owner's lock. A live owner keeps the task from running. */
  runIfOwnerAbsent<T>(ownerId: string, task: () => Promise<T>): Promise<OwnerAbsentResult<T>>
}

export type BrowserOwnerLockOptions = {
  /**
   * The page lifetime. Production uses a promise that never settles, so the
   * browser releases the lock only when the page ends. A test page fixture
   * passes a promise that it settles to simulate page loss.
   */
  pageLifetime?: Promise<void>
}

const productionPageLifetime = new Promise<void>(() => {})

function webLocks(): LockManager {
  const locks = globalThis.navigator?.locks
  if (!locks) throw new Error('Web Locks are required for durable occurrence ownership')
  return locks
}

export function createBrowserOwnerLock(options: BrowserOwnerLockOptions = {}): OwnerLock {
  const lifetime = options.pageLifetime ?? productionPageLifetime
  return {
    holdForPageLifetime(ownerId) {
      return new Promise<void>((resolve, reject) => {
        const locks = webLocks()
        locks.request(ownerLockName(ownerId), { mode: 'exclusive' }, async (lock) => {
          if (!lock) throw new Error('browser did not grant the occurrence-owner lock')
          resolve()
          await lifetime
        }).catch(reject)
      })
    },

    async runIfOwnerAbsent<T>(ownerId: string, task: () => Promise<T>): Promise<OwnerAbsentResult<T>> {
      const locks = webLocks()
      return locks.request(ownerLockName(ownerId), { mode: 'exclusive', ifAvailable: true }, async (lock) => {
        if (!lock) return { ran: false } as const
        return { ran: true, value: await task() } as const
      })
    },
  }
}
