import { afterEach, describe, expect, it, vi } from 'vitest'
import { createOccurrenceOwner } from './occurrence-owner'

afterEach(() => {
  sessionStorage.clear()
  vi.restoreAllMocks()
})

describe('page occurrence ownership', () => {
  it.each([
    ['reload', true],
    ['navigate', false],
  ] as const)('recovers the previous owner only for a real %s', (navigationType, shouldRecover) => {
    sessionStorage.setItem('scanner.occurrence-owner', 'previous-page-owner')
    vi.spyOn(performance, 'getEntriesByType').mockReturnValue([
      { type: navigationType } as PerformanceNavigationTiming,
    ])

    const owner = createOccurrenceOwner()

    expect(owner.ownerId).not.toBe('previous-page-owner')
    expect(owner.recoverOwnerId).toBe(shouldRecover ? 'previous-page-owner' : undefined)
    expect(sessionStorage.getItem('scanner.occurrence-owner')).toBe(owner.ownerId)
  })

})
