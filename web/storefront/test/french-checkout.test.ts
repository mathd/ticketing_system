import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { formatHistoryTime, historyLabel } from '../src/lib/format';
import { LOCALES, UI_STRINGS, fill, plural, type MessageKey } from '../src/lib/locales';

// TKT-365: one French purchase reads French throughout. These tests pin the string table,
// the plural rule and the history formatting that the pages and islands share.

const placeholders = (text: string) => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1]).sort();

function forms(value: unknown): string[] {
  return typeof value === 'string' ? [value] : Object.values(value as Record<string, string>);
}

describe('the string table', () => {
  it('has exactly the same keys in every locale', () => {
    const english = Object.keys(UI_STRINGS.en).sort();
    for (const locale of LOCALES) expect(Object.keys(UI_STRINGS[locale]).sort()).toEqual(english);
  });

  it('keeps the same placeholders in every locale and every plural form', () => {
    for (const key of Object.keys(UI_STRINGS.en) as MessageKey[]) {
      const expected = placeholders(forms(UI_STRINGS.en[key])[0]);
      for (const locale of LOCALES) {
        for (const form of forms(UI_STRINGS[locale][key])) {
          expect(placeholders(form), `${locale}.${key}: ${form}`).toEqual(expected);
        }
      }
    }
  });

  // The keys this ticket added are all visible on the French purchase path. A French
  // value equal to the English one is an untranslated string.
  it.each([
    'languageLabel', 'fakePaymentLabel', 'fakePaymentSuccess', 'fakePaymentDecline', 'fakePaymentTimeout',
    'testBuyerName', 'holdCountdown', 'venueLine', 'notFound',
    'historyIssued', 'historyDelivered', 'historyRedelivered', 'historyRedeemed', 'historyEntry', 'historyExit',
    'historyDuplicateAdmit', 'historyRefunded', 'historyExchanged', 'historyOther',
  ] as const)('translates %s', (key) => {
    expect(UI_STRINGS.fr[key]).not.toBe(UI_STRINGS.en[key]);
    expect(UI_STRINGS.fr[key].trim()).not.toBe('');
  });
});

describe('fill', () => {
  it('substitutes every named placeholder and leaves an unknown one visible', () => {
    expect(fill('{a} and {b} and {c}', { a: 1, b: 'two' })).toBe('1 and two and {c}');
  });
});

// The plural forms are written from French and English grammar, not read back from a
// run: French treats 0 and 1 as singular, English treats only 1 as singular.
describe('plural', () => {
  it.each([
    [0, '0 place sélectionnée sur 4'],
    [1, '1 place sélectionnée sur 4'],
    [2, '2 places sélectionnées sur 4'],
  ])('French counts %i selected seats', (n, text) => {
    expect(plural('fr', UI_STRINGS.fr.seatsSelected, n, { max: 4 })).toBe(text);
  });

  it.each([
    [0, '0 seats selected out of 4'],
    [1, '1 seat selected out of 4'],
    [2, '2 seats selected out of 4'],
  ])('English counts %i selected seats', (n, text) => {
    expect(plural('en', UI_STRINGS.en.seatsSelected, n, { max: 4 })).toBe(text);
  });

  it('agrees the seat ceiling with its count', () => {
    expect(plural('fr', UI_STRINGS.fr.seatLimitReached, 1)).toBe('Vous pouvez sélectionner au plus 1 place.');
    expect(plural('fr', UI_STRINGS.fr.seatLimitReached, 6)).toBe('Vous pouvez sélectionner au plus 6 places.');
    expect(plural('en', UI_STRINGS.en.seatLimitReached, 1)).toBe('You can select at most 1 seat.');
    expect(plural('en', UI_STRINGS.en.seatLimitReached, 6)).toBe('You can select at most 6 seats.');
  });

  it('falls back to the other form for a category a message does not spell out', () => {
    // French has a `many` category (1 000 000); the table does not need a form for it.
    expect(new Intl.PluralRules('fr').select(1_000_000)).toBe('many');
    expect(plural('fr', { one: '{n} place', other: '{n} places' }, 1_000_000)).toBe('1000000 places');
  });
});

describe('history labels', () => {
  const KNOWN = {
    issued: 'historyIssued',
    delivered: 'historyDelivered',
    redelivered: 'historyRedelivered',
    redeemed: 'historyRedeemed',
    entry: 'historyEntry',
    exit: 'historyExit',
    duplicate_admit: 'historyDuplicateAdmit',
    refunded: 'historyRefunded',
    exchanged: 'historyExchanged',
  } as const;

  it.each(LOCALES)('labels each of the nine lifecycle types with its own message (%s)', (locale) => {
    const t = UI_STRINGS[locale];
    const labels = Object.entries(KNOWN).map(([type, key]) => {
      expect(historyLabel(type, locale), type).toBe(t[key]);
      return t[key];
    });
    expect(new Set(labels).size).toBe(labels.length);
  });

  it.each(['future_kind', 'ISSUED', '', 'toString', 'constructor', '__proto__'])(
    'labels an unknown type %j with the generic message, never the raw value',
    (type) => {
      expect(historyLabel(type, 'fr')).toBe(UI_STRINGS.fr.historyOther);
      expect(historyLabel(type, 'en')).toBe(UI_STRINGS.en.historyOther);
    },
  );
});

// Every case names its zone. The host running these tests is in its own zone, so a
// formatter that fell back to the process default would pass on one machine only.
describe('formatHistoryTime', () => {
  const instant = '2026-07-14T22:30:05Z';

  it('shows the instant in the performance zone, with the zone named', () => {
    // Kolkata is UTC+05:30: the minutes move too, so no whole-hour default matches.
    const fr = formatHistoryTime(instant, 'Asia/Kolkata', 'fr');
    expect(fr).toContain('15');
    expect(fr).toContain('juil.');
    expect(fr).toContain('04:00:05');
    expect(fr).toContain('UTC+5:30');
    expect(fr).not.toContain('22:30');
    const en = formatHistoryTime(instant, 'Asia/Kolkata', 'en');
    expect(en).toContain('Jul 15');
    expect(en).toContain('04:00:05');
    expect(en).toContain('GMT+5:30');
  });

  it('follows daylight saving time in the performance zone', () => {
    const winter = formatHistoryTime('2026-01-15T12:00:00Z', 'Europe/Paris', 'fr');
    const summer = formatHistoryTime('2026-07-15T12:00:00Z', 'Europe/Paris', 'fr');
    expect(winter).toContain('13:00:00');
    expect(winter).toContain('UTC+1');
    expect(summer).toContain('14:00:00');
    expect(summer).toContain('UTC+2');
  });

  it('crosses the date line by zone, not by the UTC date', () => {
    expect(formatHistoryTime(instant, 'America/Los_Angeles', 'fr')).toContain('14 juil.');
    expect(formatHistoryTime(instant, 'Asia/Kolkata', 'fr')).toContain('15 juil.');
  });

  // The fallback must name UTC itself, not inherit the process zone. CI and the storefront
  // container run in UTC, where a fallback that forgot `timeZone: 'UTC'` would still pass,
  // so this block moves the PROCESS to Kathmandu (UTC+05:45) for its duration. Node reads
  // TZ again when it changes; the guard below fails the block if it did not.
  describe('when Access sent no usable zone', () => {
    let previousTZ: string | undefined;
    beforeAll(() => {
      previousTZ = process.env.TZ;
      process.env.TZ = 'Asia/Kathmandu';
    });
    afterAll(() => {
      if (previousTZ === undefined) delete process.env.TZ;
      else process.env.TZ = previousTZ;
    });

    it('runs in a process zone that is not UTC', () => {
      expect(new Date(instant).getMinutes()).toBe(15);
    });

    it.each([
      ['a missing zone', null],
      ['a zone the runtime does not know', 'Mars/Olympus_Mons'],
      ['an empty zone', ''],
    ])('formats %s in labelled UTC', (_name, zone) => {
      const fr = formatHistoryTime(instant, zone, 'fr');
      expect(fr).toContain('22:30:05');
      expect(fr).toContain('14 juil.');
      expect(fr).toContain('UTC');
      expect(fr).not.toContain('UTC+');
      const en = formatHistoryTime(instant, zone, 'en');
      expect(en).toContain('10:30:05');
      expect(en).toContain('UTC');
      expect(en).not.toContain('GMT+');
    });
  });
});
