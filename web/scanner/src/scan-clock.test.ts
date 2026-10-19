import { afterEach, expect, it, vi } from 'vitest'
import { captureScanClock, checkScanClock, productionScanClock } from './scan-clock'

afterEach(() => vi.restoreAllMocks())

it.each([3000, 30000] as const)('uses the strict literal limit %s with both clocks', (limit) => {
  const start = { wall: 100000, monotonic: 100 }
  expect(checkScanClock(start, { wall: start.wall + limit - 1, monotonic: start.monotonic + limit - 1 }, limit).ok).toBe(true)
  for (const [wall, monotonic] of [[limit, limit], [limit, limit - 1], [limit - 1, limit]]) {
    expect(checkScanClock(start, { wall: start.wall + wall, monotonic: start.monotonic + monotonic }, limit)).toMatchObject({ ok: false })
  }
})

it('allows equality at 1000 ms disagreement, refuses excess and monotonic regression', () => {
  const start = { wall: 0, monotonic: 0 }
  expect(checkScanClock(start, { wall: 2000, monotonic: 1000 }, 3000).ok).toBe(true)
  for (const reading of [{ wall: 2001, monotonic: 1000 }, { wall: 0, monotonic: -1 }]) {
    expect(checkScanClock(start, reading, 3000)).toMatchObject({ ok: false, reason: 'clock' })
  }
})

it.each([NaN, Infinity, -Infinity])('refuses every nonfinite start and current reading: %s', (bad) => {
  const good = { wall: 10, monotonic: 10 }
  for (const key of ['wall', 'monotonic'] as const) {
    expect(checkScanClock({ ...good, [key]: bad }, good, 3000).ok).toBe(false)
    expect(checkScanClock(good, { ...good, [key]: bad }, 3000).ok).toBe(false)
  }
})

it('reads independent production sources and propagates reader exceptions', () => {
  vi.spyOn(Date, 'now').mockReturnValue(123456)
  vi.spyOn(performance, 'now').mockReturnValue(123)
  expect(captureScanClock(productionScanClock)).toEqual({ wall: 123456, monotonic: 123 })
  expect(() => captureScanClock({ wall: () => { throw new Error('clock failed') }, monotonic: () => 1 })).toThrow('clock failed')
})
