// The time limit of one scan (TKT-399, D27, ADR-025 §3).
//
// The rule: no scan may be minted, sent or open the gate after the operator was
// left waiting 3 seconds or more since the scan started. A scan reads both
// clocks when it starts, and checks them again before each of those steps.
//
// Two clocks, because each one can be wrong in a way the other shows. A
// monotonic clock (`performance.now()`) is not changed when someone sets the
// device time, but some browsers stop it while the device sleeps (WebKit bug
// 225610). The wall
// clock (`Date.now()`) keeps running during sleep, but it can jump when the
// device time is set.

/** A policy limit, not a measured value: the ticket holder is still at the gate. */
export const SCAN_TIME_LIMIT_MS = 3_000

/**
 * The largest difference allowed between the two elapsed times. We assume a
 * healthy device stays far below it: we expect browsers to round these clocks by
 * 100 ms or less, and clock correction to move the wall clock by a few
 * milliseconds in 3 seconds. A larger difference means one clock stopped or
 * jumped, and the scanner cannot tell which one is true, so it refuses.
 */
export const CLOCK_DISAGREEMENT_LIMIT_MS = 1_000

export type ScanClockReading = { monotonic: number; wall: number }

/** Reads both clocks. Tests replace this module to control time. */
export function readScanClock(): ScanClockReading {
  return { monotonic: globalThis.performance.now(), wall: Date.now() }
}

/**
 * True only if the scan is still in time. Each line below refuses one case, and a
 * case that the code cannot measure refuses too.
 */
export function scanStillInTime(start: ScanClockReading, now: ScanClockReading): boolean {
  const values = [start.monotonic, start.wall, now.monotonic, now.wall]
  // Every comparison below is false for NaN, so this line alone refuses a clock
  // that gives no usable time.
  if (!values.every(Number.isFinite)) return false
  const monotonic = now.monotonic - start.monotonic
  const wall = now.wall - start.wall
  // A monotonic clock never goes back. If it did, it cannot be used.
  if (monotonic < 0) return false
  if (monotonic >= SCAN_TIME_LIMIT_MS) return false
  // Catches a monotonic clock that stopped while the device slept.
  if (wall >= SCAN_TIME_LIMIT_MS) return false
  // Catches a stopped monotonic clock hidden by a wall clock set back, and a wall
  // clock that jumped either way.
  if (Math.abs(monotonic - wall) > CLOCK_DISAGREEMENT_LIMIT_MS) return false
  return true
}
