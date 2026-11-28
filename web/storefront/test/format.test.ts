import { readdirSync, readFileSync } from 'node:fs';
import { join, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { accountDateFormatter, formatMoney, formatStartsAt, minorToDecimalString } from '../src/lib/format';

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
    // fr displays as fr-CA (ADR-089): U+00A0 groups the thousands and comes before the
    // symbol. fr would group with U+202F, so the amount needs a thousands group.
    expect(formatMoney(123450, 'EUR', 'fr')).toBe('1\u00a0234,50\u00a0€');
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
    // fr-CA writes the time as "19 h 30", with plain spaces (ADR-089).
    expect(fr).toBe('vendredi 18 septembre 2026 à 19 h 30');
    expect(en).toContain('Friday, September 18, 2026');
    expect(en).toContain('7:30');
  });
});

describe('accountDateFormatter', () => {
  // The long date reads the same in fr and fr-CA, so the resolved locale is what tells
  // the display locale apart.
  it('formats the wallet date as a long date in the display locale', () => {
    const fr = accountDateFormatter('fr').resolvedOptions();
    expect(fr.locale).toBe('fr-CA');
    expect(fr.dateStyle).toBe('long');
    const en = accountDateFormatter('en').resolvedOptions();
    expect(en.locale).toBe('en');
    expect(en.dateStyle).toBe('long');
  });
});

// TKT-531 (ADR-089): lib/format.ts maps the content locale to the display locale (fr is
// shown as fr-CA). A formatter built anywhere else would skip that map, so this scan
// refuses Intl and toLocale*String outside lib/format.ts. lib/locales.ts keeps two uses:
// plural selection follows the CONTENT locale, and the type names the plural categories.
//
// Limit: the scan reads text. It does not see Intl reached by destructuring or by a
// computed member, such as Intl['DateTimeFormat'].
const SRC = fileURLToPath(new URL('../src/', import.meta.url));
const SOURCE_FILE = /\.(?:ts|tsx|js|jsx|mjs|astro)$/;
const INTL_MEMBER = /\bIntl\s*\.\s*([A-Za-z_$][\w$]*)/g;
const TO_LOCALE_STRING = /\b(toLocale\w*String)\s*\(/g;
const LOCALES_EXEMPTIONS = [/\bIntl\.LDMLPluralRule\b/g, /\bnew Intl\.PluralRules\(locale\)/g];

/** Each display-format call in `text` outside lib/format.ts, as `path:line: sink`. */
function intlSinkViolations(relativePath: string, text: string): string[] {
  if (relativePath === 'lib/format.ts') return [];
  let scanned = text;
  if (relativePath === 'lib/locales.ts') {
    // Removed within a line, so the line numbers of what remains do not move.
    for (const exempt of LOCALES_EXEMPTIONS) scanned = scanned.replace(exempt, '');
  }
  const lineOf = (index: number) => scanned.slice(0, index).split('\n').length;
  return [
    ...[...scanned.matchAll(INTL_MEMBER)].map((m) => ({ at: m.index, sink: `Intl.${m[1]}` })),
    ...[...scanned.matchAll(TO_LOCALE_STRING)].map((m) => ({ at: m.index, sink: m[1] })),
  ]
    .sort((a, b) => a.at - b.at)
    .map(({ at, sink }) => `${relativePath}:${lineOf(at)}: ${sink}`);
}

/** Every storefront source file, relative to src/ with forward slashes. */
function sourceFiles(): string[] {
  return readdirSync(SRC, { recursive: true, encoding: 'utf8' })
    .map((path) => path.split(sep).join('/'))
    .filter((path) => SOURCE_FILE.test(path))
    .sort();
}

describe('intlSinkViolations', () => {
  it.each([
    ['lib/locales.ts', 'const f = new Intl.NumberFormat(locale);', ['lib/locales.ts:1: Intl.NumberFormat']],
    ['lib/locales.ts', "new Intl.PluralRules('fr').select(1);", ['lib/locales.ts:1: Intl.PluralRules']],
    ['components/X.tsx', 'new Intl.PluralRules(locale).select(1);', ['components/X.tsx:1: Intl.PluralRules']],
    ['pages/a.astro', 'x.toLocaleString (lang)', ['pages/a.astro:1: toLocaleString']],
    ['pages/a.astro', "first\nd.toLocaleDateString('fr')", ['pages/a.astro:2: toLocaleDateString']],
    ['lib/x.ts', 'const F = Intl . DateTimeFormat;', ['lib/x.ts:1: Intl.DateTimeFormat']],
    ['lib/x.js', 'a\nb\nnew Intl\n  .RelativeTimeFormat(l)', ['lib/x.js:3: Intl.RelativeTimeFormat']],
  ])('reports %s: %j', (path, text, found) => {
    expect(intlSinkViolations(path, text)).toEqual(found);
  });

  it.each([
    ['lib/format.ts', 'new Intl.DateTimeFormat(locale, options); n.toLocaleString(locale);'],
    ['lib/locales.ts', "Partial<Record<Exclude<Intl.LDMLPluralRule, 'one'>, string>>"],
    ['lib/locales.ts', 'message[new Intl.PluralRules(locale).select(count)]'],
    ['components/X.tsx', 'String(count); text.toLowerCase(); locale.toString();'],
  ])('does not report %s: %j', (path, text) => {
    expect(intlSinkViolations(path, text)).toEqual([]);
  });
});

describe('storefront display formatting', () => {
  it('walks the islands and the pages', () => {
    const files = sourceFiles();
    expect(files).toContain('components/HoldPicker.tsx');
    expect(files).toContain('lib/format.ts');
    expect(files.some((path) => path.endsWith('.astro'))).toBe(true);
  });

  it('formats numbers, money and dates only through lib/format.ts', () => {
    const found = sourceFiles().flatMap((path) => intlSinkViolations(path, readFileSync(join(SRC, path), 'utf8')));
    expect(found).toEqual([]);
  });
});
