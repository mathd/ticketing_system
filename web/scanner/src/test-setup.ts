// The occurrence queue (ADR-025 §D3) makes IndexedDB part of the scan path;
// jsdom has none, so every suite runs over fake-indexeddb.
import 'fake-indexeddb/auto'

type TestLockCallback<T> = (lock: Lock | null) => PromiseLike<T> | T

class TestLockManager {
  private readonly held = new Set<string>()
  private readonly waiters = new Map<string, Array<() => void>>()

  async request<T>(
    name: string,
    optionsOrCallback: LockOptions | TestLockCallback<T>,
    callbackArgument?: TestLockCallback<T>,
  ): Promise<T> {
    const options = typeof optionsOrCallback === 'function' ? {} : optionsOrCallback
    const callback = typeof optionsOrCallback === 'function' ? optionsOrCallback : callbackArgument
    if (!callback) throw new TypeError('lock callback is required')

    if (options.ifAvailable && this.held.has(name)) return callback(null)
    while (this.held.has(name)) {
      await new Promise<void>((resolve) => {
        const waiting = this.waiters.get(name) ?? []
        waiting.push(resolve)
        this.waiters.set(name, waiting)
      })
    }

    this.held.add(name)
    try {
      return await callback({ name, mode: options.mode ?? 'exclusive' } as Lock)
    } finally {
      this.held.delete(name)
      const next = this.waiters.get(name)?.shift()
      if (next) next()
    }
  }
}

if (!navigator.locks) {
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: new TestLockManager(),
  })
}
