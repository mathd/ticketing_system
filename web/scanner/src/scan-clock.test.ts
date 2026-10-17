import { afterEach, describe, expect, it, vi } from 'vitest'
import { readScanClock, SCAN_OPEN_LIMIT_MS, SCAN_SEND_LIMIT_MS, scanStillInTime } from './scan-clock'

// The expected values come from the rule (ADR-025 §3, D27): a 3000 ms limit on
// each clock to mint and send a scan, a 30 000 ms limit on each clock to open
// the gate, and at most 1000 ms between the two elapsed times. The times are
// literals, not the module's constants, so a change to a constant shows here.
const start = { monotonic: 10_000, wall: 1_760_000_000_000 }
const after = (monotonic: number, wall: number) => ({
  monotonic: start.monotonic + monotonic,
  wall: start.wall + wall,
})

describe('scanStillInTime (TKT-399 D27)', () => {
  const inTimeToSend = (now: { monotonic: number; wall: number }, from = start) => scanStillInTime(from, now, SCAN_SEND_LIMIT_MS)
  const inTimeToOpen = (now: { monotonic: number; wall: number }, from = start) => scanStillInTime(from, now, SCAN_OPEN_LIMIT_MS)

  it.each([
    { monotonic: 0, wall: 0, inTime: true },
    { monotonic: 2_999, wall: 2_999, inTime: true },
    { monotonic: 3_000, wall: 3_000, inTime: false },
    { monotonic: 3_000, wall: 2_999, inTime: false },
    { monotonic: 2_999, wall: 3_000, inTime: false },
  ])('the 3000 ms limit to send: monotonic $monotonic ms, wall $wall ms, in time=$inTime', ({ monotonic, wall, inTime }) => {
    expect(inTimeToSend(after(monotonic, wall))).toBe(inTime)
  })

  it.each([
    { monotonic: 3_000, wall: 3_000, inTime: true },
    { monotonic: 29_999, wall: 29_999, inTime: true },
    { monotonic: 30_000, wall: 30_000, inTime: false },
    { monotonic: 30_000, wall: 29_999, inTime: false },
    { monotonic: 29_999, wall: 30_000, inTime: false },
  ])('the 30 000 ms limit to open: monotonic $monotonic ms, wall $wall ms, in time=$inTime', ({ monotonic, wall, inTime }) => {
    expect(inTimeToOpen(after(monotonic, wall))).toBe(inTime)
  })

  // Some browsers stop performance.now() while the device sleeps. The wall
  // clock still runs. When the clock stopped for only part of the wait, the two
  // clocks agree within 1000 ms, and the wall clock alone shows the wait.
  it.each([
    { stopped: 'for the whole wait', monotonic: 0 },
    { stopped: 'for 500 ms', monotonic: 2_500 },
  ])('refuses to send when the monotonic clock stopped $stopped while 3000 ms passed on the wall clock', ({ monotonic }) => {
    expect(inTimeToSend(after(monotonic, 3_000))).toBe(false)
  })

  it('refuses to open when the monotonic clock stopped for 500 ms while 30 000 ms passed on the wall clock', () => {
    expect(inTimeToOpen(after(29_500, 30_000))).toBe(false)
  })

  // The scanner cannot tell which clock is true, so a difference above 1000 ms
  // refuses in both directions. A wall clock that jumped forward looks the same
  // as a monotonic clock that stopped; one set back can hide a stopped one.
  it.each([
    { name: 'forward by 1000 ms', monotonic: 100, wall: 1_100, inTime: true },
    { name: 'forward by 1001 ms', monotonic: 100, wall: 1_101, inTime: false },
    { name: 'back by 1000 ms', monotonic: 100, wall: -900, inTime: true },
    { name: 'back by 1001 ms', monotonic: 100, wall: -901, inTime: false },
    { name: 'back by an hour', monotonic: 100, wall: -3_600_000, inTime: false },
  ])('a wall clock that jumped $name: in time=$inTime', ({ monotonic, wall, inTime }) => {
    expect(inTimeToSend(after(monotonic, wall))).toBe(inTime)
    expect(inTimeToOpen(after(monotonic, wall))).toBe(inTime)
  })

  // The same 1000 ms limit applies well inside the 30 000 ms limit to open.
  it.each([
    { name: 'forward by 1001 ms', monotonic: 15_000, wall: 16_001, inTime: false },
    { name: 'back by 1001 ms', monotonic: 15_000, wall: 13_999, inTime: false },
    { name: 'forward by 1000 ms', monotonic: 15_000, wall: 16_000, inTime: true },
    { name: 'back by 1000 ms', monotonic: 15_000, wall: 14_000, inTime: true },
    { name: 'forward while the monotonic clock stopped', monotonic: 0, wall: 15_000, inTime: false },
  ])('to open, after 15 000 ms, a wall clock that jumped $name: in time=$inTime', ({ monotonic, wall, inTime }) => {
    expect(inTimeToOpen(after(monotonic, wall))).toBe(inTime)
  })

  // Both clocks agree, so only the rule that a monotonic clock never goes back
  // can refuse.
  it('refuses a monotonic clock that went back', () => {
    expect(inTimeToSend(after(-1, -1))).toBe(false)
    expect(inTimeToOpen(after(-1, -1))).toBe(false)
  })

  const unusable = [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]
  it.each(unusable.flatMap((value) => [
    { name: `start monotonic ${value}`, from: { ...start, monotonic: value }, to: after(0, 0) },
    { name: `start wall ${value}`, from: { ...start, wall: value }, to: after(0, 0) },
    { name: `now monotonic ${value}`, from: start, to: { ...after(0, 0), monotonic: value } },
    { name: `now wall ${value}`, from: start, to: { ...after(0, 0), wall: value } },
  ]))('refuses a clock that gives no usable time: $name', ({ from, to }) => {
    expect(inTimeToSend(to, from)).toBe(false)
    expect(inTimeToOpen(to, from)).toBe(false)
  })
})

// Every scan test replaces readScanClock, so only this test sees which clock
// the production reader uses. The two clocks give different values here, so a
// reader that takes one clock for the other, or reads the wall clock another
// way (new Date().getTime()), fails.
describe('readScanClock (TKT-399 D27)', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('reads the monotonic clock from performance.now() and the wall clock from Date.now()', () => {
    vi.spyOn(globalThis.performance, 'now').mockReturnValue(1_234.5)
    vi.spyOn(Date, 'now').mockReturnValue(1_760_000_000_000)
    expect(readScanClock()).toEqual({ monotonic: 1_234.5, wall: 1_760_000_000_000 })
  })
})
