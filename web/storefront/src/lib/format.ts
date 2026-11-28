// Locale-aware display formatting. Money stays integer minor units end to
// end (ADR-001): the decimal string handed to Intl is built with integer
// arithmetic only — no float ever holds a monetary value.
//
// Every display format of the storefront is built in this file, so that each one
// goes through displayLocale (ADR-089). A source scan in test/format.test.ts
// refuses Intl and toLocale*String anywhere else in src/.

import { UI_STRINGS, isLocale, type Locale, type MessageKey } from './locales';

// The content locale selects the strings, the route (/fr/) and the lang attribute.
// The display locale formats numbers, money and dates. French content is shown in
// Québec French (ADR-089, owner decision): fr-CA groups thousands with U+00A0 and
// writes times as "19 h 30". A France French variant would be a new entry here, and
// in the back office's one capacity sink (web/backoffice/src/pages/index.astro).
const DISPLAY_LOCALES = { en: 'en', fr: 'fr-CA' } as const satisfies Record<Locale, string>;

/** The Intl locale for a content locale. A string outside the set passes through. */
function displayLocale(locale: string): string {
  return isLocale(locale) ? DISPLAY_LOCALES[locale] : locale;
}

/** Formats integer minor units as a localized currency string. */
export function formatMoney(amountMinor: number, currency: string, locale: string): string {
  if (!Number.isSafeInteger(amountMinor)) {
    throw new TypeError(`money must be an integer amount of minor units, got ${amountMinor}`);
  }
  const formatter = new Intl.NumberFormat(displayLocale(locale), { style: 'currency', currency });
  const digits = formatter.resolvedOptions().maximumFractionDigits ?? 2;
  return formatter.format(minorToDecimalString(amountMinor, digits) as unknown as number);
}

/** Builds "45.50" from 4550 with integer ops only (exported for tests). */
export function minorToDecimalString(amountMinor: number, fractionDigits: number): string {
  const sign = amountMinor < 0 ? '-' : '';
  const abs = Math.abs(amountMinor);
  if (fractionDigits === 0) return `${sign}${abs}`;
  const scale = 10 ** fractionDigits;
  const units = Math.trunc(abs / scale);
  const fraction = String(abs % scale).padStart(fractionDigits, '0');
  return `${sign}${units}.${fraction}`;
}

/** Localized date + time, rendered in the performance's own timezone. */
export function formatStartsAt(isoDate: string, timezone: string, locale: string): string {
  return new Intl.DateTimeFormat(displayLocale(locale), {
    dateStyle: 'full',
    timeStyle: 'short',
    timeZone: timezone,
  }).format(new Date(isoDate));
}

// Ticket history times (TKT-365, ADR-083). Date, time to the second, and the zone's
// name: a buyer reads the zone, so a time is never ambiguous. dateStyle/timeStyle
// cannot be combined with timeZoneName, hence the component options.
const HISTORY_TIME_OPTIONS: Intl.DateTimeFormatOptions = {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  timeZoneName: 'short',
};

/**
 * One history instant, in the performance's zone. A null zone (Catalog did not answer)
 * or a zone this runtime's ICU does not know (Catalog checks zones with Go's tz data,
 * this formats with ICU's) formats in UTC, labelled as UTC. Never the process default:
 * the server runs in UTC and a developer machine does not, and an unlabelled time in
 * either is a guess.
 */
export function formatHistoryTime(iso: string, timezone: string | null, locale: Locale): string {
  const instant = new Date(iso);
  if (timezone) {
    try {
      return new Intl.DateTimeFormat(displayLocale(locale), { ...HISTORY_TIME_OPTIONS, timeZone: timezone }).format(instant);
    } catch (error) {
      if (!(error instanceof RangeError)) throw error;
    }
  }
  return new Intl.DateTimeFormat(displayLocale(locale), { ...HISTORY_TIME_OPTIONS, timeZone: 'UTC' }).format(instant);
}

/**
 * The wallet's purchase date (TKT-222): a long date in the display locale. Build it once
 * per render. It formats in the process zone, as the wallet did before ADR-089; ADR-083 §7
 * governs ticket history times, not this date.
 */
export function accountDateFormatter(locale: Locale): Intl.DateTimeFormat {
  return new Intl.DateTimeFormat(displayLocale(locale), { dateStyle: 'long' });
}

// Access's lifecycle types (the CHECK constraint in access migration 0011). A Map, not an
// object: an object lookup would answer `toString` or `constructor` with a function.
const HISTORY_LABELS: ReadonlyMap<string, MessageKey> = new Map<string, MessageKey>([
  ['issued', 'historyIssued'],
  ['delivered', 'historyDelivered'],
  ['redelivered', 'historyRedelivered'],
  ['redeemed', 'historyRedeemed'],
  ['entry', 'historyEntry'],
  ['exit', 'historyExit'],
  ['duplicate_admit', 'historyDuplicateAdmit'],
  ['refunded', 'historyRefunded'],
  ['exchanged', 'historyExchanged'],
]);

/**
 * The buyer-facing label of a lifecycle type. The contract declares `type` an open
 * string, so a type added in Access before this table knows it shows as historyOther,
 * never as the raw service value.
 */
export function historyLabel(type: string, locale: Locale): string {
  const label = UI_STRINGS[locale][HISTORY_LABELS.get(type) ?? 'historyOther'];
  if (typeof label !== 'string') throw new TypeError(`history label for ${type} is not a plain message`);
  return label;
}
