import { describe, expect, it } from 'vitest';

import { formatMoney, formatStartsAt, minorToDecimalString } from '../src/lib/format';

describe('minorToDecimalString', () => {
  it('builds the decimal with integer arithmetic only', () => {
    expect(minorToDecimalString(4550, 2)).toBe('45.50');
    expect(minorToDecimalString(5, 2)).toBe('0.05');
    expect(minorToDecimalString(100, 2)).toBe('1.00');
    expect(minorToDecimalString(0, 2)).toBe('0.00');
    expect(minorToDecimalString(-4550, 2)).toBe('-45.50');
    expect(minorToDecimalString(1250, 0)).toBe('1250');
  });

  it('is exact where float division would not be', () => {
    // 0.1 + 0.2 style traps: huge amounts stay exact.
    expect(minorToDecimalString(9007199254740991, 2)).toBe('90071992547409.91');
  });
});

describe('formatMoney', () => {
  it('formats EUR per locale from minor units', () => {
    // \s: Intl separates with narrow no-break space (U+202F) in fr.
    expect(formatMoney(4550, 'EUR', 'fr')).toMatch(/^45,50\s€$/);
    expect(formatMoney(4550, 'EUR', 'en')).toBe('€45.50');
  });

  it('honors zero-decimal currencies', () => {
    // JPY has no minor unit: 4550 minor units is ¥4550, not ¥45.50.
    expect(formatMoney(4550, 'JPY', 'en')).toBe('¥4,550');
  });

  it('rejects non-integer amounts (floats are banned on money paths)', () => {
    expect(() => formatMoney(45.5, 'EUR', 'en')).toThrow(TypeError);
  });
});

describe('formatStartsAt', () => {
  const iso = '2026-09-18T17:30:00Z';
  it('renders in the performance timezone, per locale', () => {
    const fr = formatStartsAt(iso, 'Europe/Paris', 'fr');
    const en = formatStartsAt(iso, 'Europe/Paris', 'en');
    expect(fr).toContain('vendredi 18 septembre 2026');
    expect(fr).toContain('19:30');
    expect(en).toContain('Friday, September 18, 2026');
    expect(en).toContain('7:30');
  });
});
