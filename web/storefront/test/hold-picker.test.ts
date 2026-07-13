import { describe, expect, it } from 'vitest';
import { formatMoney, remainingMilliseconds } from '../src/components/HoldPicker';

describe('hold countdown', () => {
  it('uses server time rather than the device wall clock', () => {
    expect(remainingMilliseconds({ server_time: '2026-01-01T00:00:00Z', expires_at: '2026-01-01T00:10:00Z' })).toBe(600_000);
  });
  it('never returns a negative duration', () => {
    expect(remainingMilliseconds({ server_time: '2026-01-01T00:10:00Z', expires_at: '2026-01-01T00:00:00Z' })).toBe(0);
  });
});

describe('formatMoney', () => {
  it('formats whole and fractional EUR minor units in English', () => {
    expect(formatMoney(1200, 'EUR', 'en')).toBe('€12.00');
    expect(formatMoney(1210, 'EUR', 'en')).toBe('€12.10');
  });

  it('uses French currency conventions', () => {
    expect(formatMoney(1210, 'EUR', 'fr')).toMatch(/^12,10\s€$/u);
  });
});
