import { describe, expect, it } from 'vitest';

import { parseMinorUnits, parseCurrency, parseLocalizedName } from '../src/lib/event-builder';

/**
 * TKT-192 COS-5. Money is integer minor units + ISO currency (ADR-001), and
 * floats are banned on money paths.
 *
 * The banned implementation is the tempting one: take "45.50", parseFloat, times
 * a hundred. It is wrong in binary — 12.10 * 100 is 1210.0000000000002 — and it
 * assumes every currency has two decimal places, which JPY does not. So the
 * field IS minor units, and a decimal is refused rather than converted.
 */
describe('minor units (COS-5)', () => {
  it('accepts a run of digits and yields the integer', () => {
    expect(parseMinorUnits('4550')).toEqual({ ok: true, value: 4550 });
    expect(parseMinorUnits('0')).toEqual({ ok: true, value: 0 });
    expect(parseMinorUnits('1')).toEqual({ ok: true, value: 1 });
  });

  it('refuses anything that is not a run of digits', () => {
    // Each of these is a way a float could sneak in, or an input the operator
    // means differently from what it would parse as.
    for (const bad of ['45.50', '45,50', '4550.0', '1e3', '-1', '+1', ' 4550', '4550 ', '', 'abc', '4 550']) {
      expect(parseMinorUnits(bad).ok, `accepted ${JSON.stringify(bad)}`).toBe(false);
    }
  });

  it('refuses a value beyond exact integer representation', () => {
    // Above MAX_SAFE_INTEGER a JS number silently stops being the integer that
    // was typed, which on a money path is the whole problem.
    expect(parseMinorUnits(String(Number.MAX_SAFE_INTEGER)).ok).toBe(true);
    expect(parseMinorUnits('9007199254740992').ok).toBe(false);
    expect(parseMinorUnits('99999999999999999999').ok).toBe(false);
  });

  it('never produces a non-integer', () => {
    for (const input of ['0', '1', '99', '4550', '123456789']) {
      const r = parseMinorUnits(input);
      expect(r.ok && Number.isInteger(r.value)).toBe(true);
    }
  });
});

describe('currency (COS-5)', () => {
  it('accepts exactly three uppercase letters', () => {
    expect(parseCurrency('EUR')).toEqual({ ok: true, value: 'EUR' });
    expect(parseCurrency('JPY')).toEqual({ ok: true, value: 'JPY' });
  });

  it('refuses anything else, including lower case', () => {
    // Not lower-cased silently: "eur" is a typo, and quietly fixing it teaches
    // the operator the field is looser than the contract.
    for (const bad of ['eur', 'Eur', 'EURO', 'EU', '', '123', 'E U']) {
      expect(parseCurrency(bad).ok, `accepted ${JSON.stringify(bad)}`).toBe(false);
    }
  });
});

/**
 * COS-4 and the trap catalog enforces: SupportedLocales is ["en","fr"], and an
 * event whose name omits either is rejected server-side. Catching it locally is
 * what lets the form say WHICH field is missing instead of surfacing a generic
 * 400.
 */
describe('localized names (COS-4)', () => {
  it('requires both supported locales', () => {
    expect(parseLocalizedName({ en: 'Night', fr: 'Nuit' })).toEqual({
      ok: true,
      value: { en: 'Night', fr: 'Nuit' },
    });
  });

  it('names the missing locale rather than failing generically', () => {
    expect(parseLocalizedName({ en: 'Night', fr: '' })).toMatchObject({ ok: false, field: 'fr' });
    expect(parseLocalizedName({ en: '', fr: 'Nuit' })).toMatchObject({ ok: false, field: 'en' });
  });

  it('treats whitespace as missing, and trims what it keeps', () => {
    expect(parseLocalizedName({ en: '   ', fr: 'Nuit' })).toMatchObject({ ok: false, field: 'en' });
    expect(parseLocalizedName({ en: '  Night  ', fr: 'Nuit' })).toEqual({
      ok: true,
      value: { en: 'Night', fr: 'Nuit' },
    });
  });
});
