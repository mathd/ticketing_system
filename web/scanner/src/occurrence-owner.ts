export type OccurrenceOwner = { ownerId: string; recoverOwnerId?: string }

const occurrenceOwnerKey = 'scanner.occurrence-owner'

/** Creates one owner for a page load. Call at module initialization, never during React render. */
export function createOccurrenceOwner(): OccurrenceOwner {
  const ownerId = crypto.randomUUID()
  try {
    const previousOwnerId = sessionStorage.getItem(occurrenceOwnerKey)?.trim()
    const navigation = performance.getEntriesByType?.('navigation')[0] as PerformanceNavigationTiming | undefined
    sessionStorage.setItem(occurrenceOwnerKey, ownerId)
    return navigation?.type === 'reload' && previousOwnerId
      ? { ownerId, recoverOwnerId: previousOwnerId }
      : { ownerId }
  } catch {
    // Lease expiry still recovers a crash when session storage is unavailable.
    return { ownerId }
  }
}

