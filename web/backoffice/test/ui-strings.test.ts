import { describe, expect, it } from 'vitest';

import {
  LANGUAGE_NAMES,
  UI_LANGUAGES,
  UI_STRINGS,
  fill,
  otherLanguage,
  parseUiLanguage,
  splitAt,
} from '../src/lib/ui-strings';

// TKT-368. The back-office UI language is a closed set of two. These tests pin
// the parser that turns an untrusted cookie or form value into that set, and the
// completeness of the French table against the English one.

describe('parseUiLanguage', () => {
  it('accepts exactly the two supported values', () => {
    expect(parseUiLanguage('en')).toBe('en');
    expect(parseUiLanguage('fr')).toBe('fr');
  });

  it('resolves every other value to English, case-sensitively', () => {
    for (const raw of ['FR', 'Fr', ' fr', 'fr ', '', 'de', 'fr-CA', 'french', undefined, null, 1, {}]) {
      expect(parseUiLanguage(raw), JSON.stringify(raw)).toBe('en');
    }
  });

  it('does not accept a File from a multipart form as a language', () => {
    expect(parseUiLanguage(new File(['fr'], 'fr'))).toBe('en');
  });
});

describe('the switch names the other language', () => {
  it('maps each language to the other one', () => {
    expect(otherLanguage('en')).toBe('fr');
    expect(otherLanguage('fr')).toBe('en');
  });

  it('names each language in that language', () => {
    expect(LANGUAGE_NAMES).toEqual({ en: 'English', fr: 'Français' });
  });
});

const placeholders = (text: string) => [...text.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();

describe('the string tables', () => {
  it('lists the two supported languages', () => {
    expect([...UI_LANGUAGES].sort()).toEqual(['en', 'fr']);
  });

  it('gives French exactly the English keys', () => {
    expect(Object.keys(UI_STRINGS.fr).sort()).toEqual(Object.keys(UI_STRINGS.en).sort());
  });

  it('has no empty message in either language', () => {
    for (const lang of UI_LANGUAGES) {
      for (const [key, text] of Object.entries(UI_STRINGS[lang])) {
        expect(text.trim(), `${lang}.${key}`).not.toBe('');
      }
    }
  });

  it('keeps the same placeholders in both languages', () => {
    for (const [key, english] of Object.entries(UI_STRINGS.en)) {
      const french = UI_STRINGS.fr[key as keyof typeof UI_STRINGS.fr];
      expect(placeholders(french), key).toEqual(placeholders(english));
    }
  });

  // French typography: a non-breaking space before the high punctuation and
  // inside guillemets, so a line never starts with ":" or "»".
  it('puts a no-break space before French high punctuation and inside guillemets', () => {
    for (const [key, text] of Object.entries(UI_STRINGS.fr)) {
      expect(text, key).not.toMatch(/[^\u00a0\u202f][:;!?»]/);
      expect(text, key).not.toMatch(/«[^\u00a0\u202f]/);
    }
  });
});

describe('fill', () => {
  it('replaces each named placeholder', () => {
    expect(fill('Step {step} of 4', { step: 2 })).toBe('Step 2 of 4');
  });

  it('leaves an unknown placeholder visible', () => {
    expect(fill('Step {step} of 4', {})).toBe('Step {step} of 4');
  });
});

describe('splitAt', () => {
  it('splits a message around one placeholder, for markup between the halves', () => {
    expect(splitAt('Catalog reports this date as {status}.', 'status')).toEqual([
      'Catalog reports this date as ',
      '.',
    ]);
  });

  it('refuses a message that does not carry the placeholder', () => {
    expect(() => splitAt('No placeholder here.', 'status')).toThrow();
  });
});
