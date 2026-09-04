import { describe, expect, it } from 'vitest';
import { remainingMilliseconds } from '../src/components/HoldPicker';

describe('hold countdown', () => {
  it('uses server time rather than the device wall clock', () => {
    expect(remainingMilliseconds({ server_time: '2026-01-01T00:00:00Z', expires_at: '2026-01-01T00:10:00Z' })).toBe(600_000);
  });
  it('never returns a negative duration', () => {
    expect(remainingMilliseconds({ server_time: '2026-01-01T00:10:00Z', expires_at: '2026-01-01T00:00:00Z' })).toBe(0);
  });
});
