// The time limits of one scan (TKT-399, D27, ADR-025 §3).
//
// The rule: nothing is minted or sent 3 seconds or more after the scan started,
// and the gate never opens 30 seconds or more after the scan started. A scan
// reads both clocks when it starts, and checks them again before each of those
// steps, against the limit of that step.
//
// Two clocks, because each one can be wrong in a way the other shows. A
// monotonic clock (`performance.now()`) is not changed when someone sets the
// device time, but some browsers stop it while the device sleeps (WebKit bug
// 225610). The wall
// clock (`Date.now()`) keeps running during sleep, but it can jump when the
// device time is set.

/**
 * The limit to mint and send a scan. A policy limit, not a measured value: the
 * ticket holder is still at the gate. It stops a scan that waited on the device,
 * for example behind a blocked store.
 */
export const SCAN_SEND_LIMIT_MS = 3_000

/**
 * The limit to open the gate after Access accepted the scan. A policy limit, not
 * a measured value. It counts the network round trip, and the holder waits at
 * the gate while the answer travels, so it is longer than the limit to send.
 */
export const SCAN_OPEN_LIMIT_MS = 30_000

export const CLOCK_DISAGREEMENT_LIMIT_MS = 1_000

export type ScanClockReading = { monotonic: number; wall: number }

/** Reads both clocks. Tests replace this module to control time. */
export function readScanClock(): ScanClockReading {
  return { monotonic: globalThis.performance.now(), wall: Date.now() }
}

/**
 * True only if less than `limitMs` passed since the scan started, on both
 * clocks, and the clocks can be trusted. Each line below refuses one case, and
 * a case that the code cannot measure refuses too.
 */
export function scanStillInTime(start: ScanClockReading, now: ScanClockReading, limitMs: number): boolean {
  const values = [start.monotonic, start.wall, now.monotonic, now.wall]
  // Every comparison below is false for NaN, so this line alone refuses a clock
  // that gives no usable time.
  if (!values.every(Number.isFinite)) return false
  const monotonic = now.monotonic - start.monotonic
  const wall = now.wall - start.wall
  // A monotonic clock never goes back. If it did, it cannot be used.
  if (monotonic < 0) return false
  if (monotonic >= limitMs) return false
  // Catches a monotonic clock that stopped while the device slept.
  if (wall >= limitMs) return false
  // Catches a stopped monotonic clock hidden by a wall clock set back, and a wall
  // clock that jumped either way.
  if (Math.abs(monotonic - wall) > CLOCK_DISAGREEMENT_LIMIT_MS) return false
  return true
}
