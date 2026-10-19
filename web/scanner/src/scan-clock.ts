export type ScanClock = { wall(): number; monotonic(): number }
export type ScanReading = Readonly<{ wall: number; monotonic: number }>
export type ClockCheck = { ok: true; remaining: number } | { ok: false; reason: 'clock' | 'deadline' }

export const productionScanClock: ScanClock = { wall: () => Date.now(), monotonic: () => performance.now() }
export const captureScanClock = (clock: ScanClock): ScanReading => ({ wall: clock.wall(), monotonic: clock.monotonic() })

/** A timer can wake a scan; only these elapsed readings authorize its next step. */
export function checkScanClock(start: ScanReading, now: ScanReading, limit: 3000 | 30000): ClockCheck {
  if (![start.wall, start.monotonic, now.wall, now.monotonic].every(Number.isFinite)) return { ok: false, reason: 'clock' }
  const wall = now.wall - start.wall
  const monotonic = now.monotonic - start.monotonic
  if (!Number.isFinite(wall) || !Number.isFinite(monotonic) || monotonic < 0 || Math.abs(wall - monotonic) > 1000) {
    return { ok: false, reason: 'clock' }
  }
  if (wall >= limit || monotonic >= limit) return { ok: false, reason: 'deadline' }
  return { ok: true, remaining: limit - monotonic }
}

export type ScanScheduler = { set(callback: () => void, delay: number): unknown; clear(handle: unknown): void }
export const productionScanScheduler: ScanScheduler = {
  set: (callback, delay) => setTimeout(callback, delay),
  clear: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
}
