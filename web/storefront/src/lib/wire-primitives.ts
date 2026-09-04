const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const NIL_UUID = '00000000-0000-0000-0000-000000000000';
const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|[+-](\d{2}):(\d{2}))$/;

export function uuidField(value: unknown, name: string): string {
  if (typeof value !== 'string' || !UUID.test(value) || value.toLowerCase() === NIL_UUID) {
    throw new TypeError(`${name} must be a non-nil UUID`);
  }
  return value.toLowerCase();
}

export function sameUuid(left: string, right: string): boolean {
  return UUID.test(left) && UUID.test(right) && left.toLowerCase() === right.toLowerCase();
}

function leapYear(year: number): boolean {
  return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
}

/** Validates the services' non-leap-second RFC 3339 contract without normalising the timestamp. */
export function dateTimeField(value: unknown, name: string): string {
  if (typeof value !== 'string') throw new TypeError(`${name} must be an RFC 3339 date-time`);
  const match = RFC3339.exec(value);
  if (!match) throw new TypeError(`${name} must be an RFC 3339 date-time`);

  const [, yearText, monthText, dayText, hourText, minuteText, secondText, offsetHourText, offsetMinuteText] = match;
  const year = Number(yearText);
  const month = Number(monthText);
  const day = Number(dayText);
  const hour = Number(hourText);
  const minute = Number(minuteText);
  const second = Number(secondText);
  const offsetHour = offsetHourText === undefined ? 0 : Number(offsetHourText);
  const offsetMinute = offsetMinuteText === undefined ? 0 : Number(offsetMinuteText);
  const monthDays = [31, leapYear(year) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

  if (
    month < 1 || month > 12 ||
    day < 1 || day > (monthDays[month - 1] ?? 0) ||
    hour > 23 || minute > 59 || second > 59 ||
    offsetHour > 23 || offsetMinute > 59
  ) {
    throw new TypeError(`${name} must be an RFC 3339 date-time`);
  }
  return value;
}
