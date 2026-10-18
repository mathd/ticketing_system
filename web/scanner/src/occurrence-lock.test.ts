import { afterEach, describe, expect, it } from 'vitest'
import { createBrowserOwnerLock, ownerLockName } from './occurrence-lock'

// LK tier: the production lock adapter over the controlled navigator.locks that
// test-setup.ts installs. Real cross-page Chrome locks are browser-spec work.

const lifetimes: Array<() => void> = []
afterEach(() => {
  for (const end of lifetimes.splice(0)) end()
})

function pageLifetime() {
  let end!: () => void
  const lifetime = new Promise<void>((resolve) => { end = resolve })
  lifetimes.push(end)
  return { lifetime, end }
}

/** Reports whether another holder could take the lock now, without waiting. */
async function lockAvailable(name: string): Promise<boolean> {
  return navigator.locks.request(name, { ifAvailable: true }, (lock) => lock !== null)
}

describe('page owner lock adapter', () => {
  it('uses the unchanged owner lock name', () => {
    expect(ownerLockName('abc')).toBe('ticketing.scanner.occurrence-owner.abc')
  })

  it('holds the owner lock for the page lifetime, not for a callback', async () => {
    const { lifetime, end } = pageLifetime()
    const lock = createBrowserOwnerLock({ pageLifetime: lifetime })
    await lock.holdForPageLifetime('page-1')
    expect(await lockAvailable(ownerLockName('page-1'))).toBe(false)
    await new Promise((resolve) => setTimeout(resolve, 5))
    expect(await lockAvailable(ownerLockName('page-1'))).toBe(false)
    end()
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(await lockAvailable(ownerLockName('page-1'))).toBe(true)
  })

  it('refuses to hold or recover without Web Locks', async () => {
    const original = Object.getOwnPropertyDescriptor(navigator, 'locks')
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    try {
      const lock = createBrowserOwnerLock()
      await expect(lock.holdForPageLifetime('page-2')).rejects.toThrow(/Web Locks/)
      await expect(lock.runIfOwnerAbsent('page-2', async () => 1)).rejects.toThrow(/Web Locks/)
    } finally {
      if (original) Object.defineProperty(navigator, 'locks', original)
    }
  })

  it('runs a recovery task only while the owner lock is absent, and holds it during the task', async () => {
    const { lifetime, end } = pageLifetime()
    const holder = createBrowserOwnerLock({ pageLifetime: lifetime })
    const observer = createBrowserOwnerLock()
    await holder.holdForPageLifetime('page-3')

    let ran = false
    expect(await observer.runIfOwnerAbsent('page-3', async () => { ran = true })).toEqual({ ran: false })
    expect(ran).toBe(false)

    end()
    await new Promise((resolve) => setTimeout(resolve, 0))
    let heldDuringTask: boolean | undefined
    const result = await observer.runIfOwnerAbsent('page-3', async () => {
      heldDuringTask = !(await lockAvailable(ownerLockName('page-3')))
      return 'done'
    })
    expect(result).toEqual({ ran: true, value: 'done' })
    expect(heldDuringTask).toBe(true)
    expect(await lockAvailable(ownerLockName('page-3'))).toBe(true)
  })
})
