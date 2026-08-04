// Input parsing for the event builder (TKT-192 / US-B3).
//
// Separated from the page so the rules that decide whether a submission is
// valid are unit-testable without Astro, and so the page stays wiring. Every
// function returns a discriminated result rather than throwing: a bad
// submission is an expected outcome that re-renders the form with the
// operator's values intact (COS-4), not an exception.

import type { components } from './api-types.gen';

export type LocalizedString = components['schemas']['LocalizedString'];

export type Parsed<T> = { ok: true; value: T } | { ok: false; field: string; message: string };

/** The locales catalog requires on every localized name (`SupportedLocales`). */
export const REQUIRED_LOCALES = ['en', 'fr'] as const;

/**
 * Minor units, as an integer, from a digits-only string.
 *
 * The field IS minor units — 4550 for €45.50 — and a decimal is refused rather
 * than converted. The tempting implementation is `parseFloat(x) * 100`, which is
 * wrong twice over: it is inexact in binary (`12.10 * 100` is
 * `1210.0000000000002`) and it assumes a two-decimal currency, which JPY is not.
 * ADR-001 bans floats on money paths, and this is the path.
 *
 * Bounded at MAX_SAFE_INTEGER because past it a JS number stops being the
 * integer that was typed — silently, which on money is the whole problem.
 */
export function parseMinorUnits(raw: string): Parsed<number> {
  if (!/^[0-9]+$/.test(raw)) {
    return {
      ok: false,
      field: 'amount',
      message: 'Enter the amount in minor units — digits only, no decimal point (€45.50 is 4550).',
    };
  }
  // BigInt first: parsing to a number and then checking the bound would already
  // have lost precision by the time we looked.
  const value = BigInt(raw);
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    return { ok: false, field: 'amount', message: 'That amount is too large to represent exactly.' };
  }
  return { ok: true, value: Number(value) };
}

/**
 * An ISO-4217 code, exactly as the contract spells it. Not upper-cased for the
 * operator: "eur" is a typo, and silently fixing it teaches them the field is
 * looser than the contract, which is a lesson that expires badly.
 */
export function parseCurrency(raw: string): Parsed<string> {
  if (!/^[A-Z]{3}$/.test(raw)) {
    return { ok: false, field: 'currency', message: 'Use a three-letter uppercase ISO code, e.g. EUR.' };
  }
  return { ok: true, value: raw };
}

/**
 * A localized name carrying every locale catalog requires.
 *
 * Catalog rejects a name missing a supported locale, so this is not merely
 * politeness: without it the operator gets a generic 400 that names no field.
 * Checking locally is what lets the form point at the empty box (COS-4).
 */
export function parseLocalizedName(raw: Record<string, string>): Parsed<LocalizedString> {
  const value: Record<string, string> = {};
  for (const locale of REQUIRED_LOCALES) {
    const text = (raw[locale] ?? '').trim();
    if (!text) {
      return { ok: false, field: locale, message: `A name in ${locale.toUpperCase()} is required.` };
    }
    value[locale] = text;
  }
  return { ok: true, value };
}
