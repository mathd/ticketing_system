// Locale-aware display formatting. Money stays integer minor units end to
// end (ADR-001): the decimal string handed to Intl is built with integer
// arithmetic only — no float ever holds a monetary value.

import { UI_STRINGS, type Locale, type MessageKey } from './locales';

/** Formats integer minor units as a localized currency string. */
export function formatMoney(amountMinor: number, currency: string, locale: string): string {
  if (!Number.isSafeInteger(amountMinor)) {
    throw new TypeError(`money must be an integer amount of minor units, got ${amountMinor}`);
  }
  const formatter = new Intl.NumberFormat(locale, { style: 'currency', currency });
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
  return new Intl.DateTimeFormat(locale, {
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
      return new Intl.DateTimeFormat(locale, { ...HISTORY_TIME_OPTIONS, timeZone: timezone }).format(instant);
    } catch (error) {
      if (!(error instanceof RangeError)) throw error;
    }
  }
  return new Intl.DateTimeFormat(locale, { ...HISTORY_TIME_OPTIONS, timeZone: 'UTC' }).format(instant);
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
