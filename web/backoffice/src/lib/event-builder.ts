// Input parsing for the event builder (TKT-192 / US-B3).
//
// Separated from the page so the rules that decide whether a submission is
// valid are unit-testable without Astro, and so the page stays wiring. Every
// function returns a discriminated result rather than throwing: a bad
// submission is an expected outcome that re-renders the form with the
// operator's values intact (COS-4), not an exception.

import type { components } from './api-types.gen';
import type { MessageKey } from './ui-strings';

export type LocalizedString = components['schemas']['LocalizedString'];

/**
 * Why a submission was refused, as a key into the UI string table (TKT-368) —
 * not as prose. The page says it in the operator's UI language; the parser only
 * decides that it is wrong and where.
 */
export type ParseError = { key: MessageKey; params?: Record<string, string> };

export type Parsed<T> = { ok: true; value: T } | { ok: false; field: string; error: ParseError };

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
  // Length FIRST. A regex over an arbitrarily long string is itself the
  // unbounded work this bound exists to cap, so checking the shape before the
  // size defeats it (ai-review pass 4). 64 characters is far more than any
  // legitimate amount and cheap to reject.
  //
  // Then significant digits, which caps the VALUE without rejecting
  // "00000000000000001" — that is 1 wearing seventeen characters (pass 2). An
  // all-zero megabyte has no significant digits at all, which is why the length
  // bound cannot be dropped in favour of this one (pass 3).
  if (raw.length > 64 || raw.replace(/^0+/, '').length > 16) {
    return { ok: false, field: 'amount', error: { key: 'amountTooLarge' } };
  }
  if (!/^[0-9]+$/.test(raw)) {
    return { ok: false, field: 'amount', error: { key: 'amountInvalid' } };
  }
  // BigInt only after the bound: parsing to a Number first and checking after
  // would already have lost the precision we are checking for.
  const value = BigInt(raw);
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    return { ok: false, field: 'amount', error: { key: 'amountTooLarge' } };
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
    return { ok: false, field: 'currency', error: { key: 'currencyInvalid' } };
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
      return {
        ok: false,
        field: locale,
        error: { key: 'nameRequired', params: { locale: locale.toUpperCase() } },
      };
    }
    value[locale] = text;
  }
  return { ok: true, value };
}

/**
 * The lead a catalog refusal gets in the UI language (TKT-368, D1(d)), chosen by
 * STATUS CLASS only. Catalog's own text follows it, unchanged and marked English:
 * it is the part that says WHY (ADR-018's refusals are catalog's to make), and
 * catalog sends no stable code for these, so matching its sentences would drift
 * silently the day one is reworded. Stable catalog error codes are follow-up work.
 *
 * Only 4xx reach here: a 5xx or an unreadable success is an AmbiguousMutationError
 * (upstream.ts), which is not a refusal and has its own message.
 */
export function catalogRefusalKey(status: number): MessageKey {
  return status === 409 ? 'catalogRefusedChange' : 'catalogRefusedStep';
}
