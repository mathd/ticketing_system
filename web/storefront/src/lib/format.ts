// Locale-aware display formatting. Money stays integer minor units end to
// end (ADR-001): the decimal string handed to Intl is built with integer
// arithmetic only — no float ever holds a monetary value.

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
