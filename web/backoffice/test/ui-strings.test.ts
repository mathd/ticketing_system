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

  // Québec French typography (ADR-089, TKT-531): no space before ? ! ;, U+00A0 before a
  // colon and inside guillemets. The helper and its own cases are below.
  it('follows the French typography rule in every message', () => {
    const broken = Object.fromEntries(
      Object.entries(UI_STRINGS.fr)
        .map(([key, text]) => [key, typographyViolations(text)] as const)
        .filter(([, rules]) => rules.length > 0),
    );
    expect(broken).toEqual({});
  });
});

// The ADR-089 rule, local to this suite (the storefront and commerce keep their own
// copies). It is the owner's working rule until TKT-593 confirms or changes it. Each
// predicate is one entry, so each one can be tested and mutated alone; a match means the
// rule is broken. White_Space is the Unicode property, not only U+0020.
const NBSP = '\u00a0';
const NNBSP = '\u202f';
const TYPOGRAPHY_RULES: ReadonlyArray<readonly [string, RegExp]> = [
  // P1: no space of any kind before ? ! ;
  ['P1', /\p{White_Space}[?!;]/u],
  // P2a: the only space allowed directly before : is U+00A0, whatever follows the colon.
  ['P2a', /(?!\u00a0)\p{White_Space}:/u],
  // P2b: a colon followed by a space, or at the end of the text, needs U+00A0 before it.
  // A colon inside a token (https://, 19:30) has neither, so it passes.
  ['P2b', /(?<!\u00a0):(?=\p{White_Space}|$)/u],
  // P3a: U+00A0 directly after «, also when « ends the text.
  ['P3a', /«(?!\u00a0)/u],
  // P3b: U+00A0 directly before », also when » starts the text.
  ['P3b', /(?<!\u00a0)»/u],
];

/** The ADR-089 rules `text` breaks, in rule order. */
function typographyViolations(text: string): string[] {
  return TYPOGRAPHY_RULES.filter(([, broken]) => broken.test(text)).map(([rule]) => rule);
}

describe('typographyViolations (ADR-089)', () => {
  it.each([
    // P1, one case per mark, then a tab and both no-break spaces.
    ['Vraiment ?', ['P1']],
    ['Bravo !', ['P1']],
    ['Oui ; puis non', ['P1']],
    ['Vraiment\t?', ['P1']],
    [`Vraiment${NBSP}?`, ['P1']],
    [`Bravo${NNBSP}!`, ['P1']],
    // P2a alone: the colon is followed by neither a space nor the end of the text.
    ['Salle :{name}', ['P2a']],
    [`Salle${NNBSP}:{name}`, ['P2a']],
    ['Salle\t:{name}', ['P2a']],
    // P2b alone: no space at all before the colon.
    ['Salle: A', ['P2b']],
    ['Salle:', ['P2b']],
    // P2b alone: any White_Space after the colon counts, not only U+0020.
    ['Salle:\nA', ['P2b']],
    [`Salle:${NNBSP}A`, ['P2b']],
    // Both: a wrong space before a colon that needs U+00A0.
    ['Salle : A', ['P2a', 'P2b']],
    [`Salle${NNBSP}: A`, ['P2a', 'P2b']],
    ['Salle :', ['P2a', 'P2b']],
    // P3, each side alone, with the other side correct.
    [`« mot${NBSP}»`, ['P3a']],
    [`«${NNBSP}mot${NBSP}»`, ['P3a']],
    ['fin «', ['P3a']],
    [`«${NBSP}mot »`, ['P3b']],
    [`«${NBSP}mot${NNBSP}»`, ['P3b']],
    ['» début', ['P3b']],
  ])('finds %j breaks %j', (text, rules) => {
    expect(typographyViolations(text)).toEqual(rules);
  });

  it.each([
    'Vraiment?',
    'Bravo!',
    'Oui; puis non',
    `Salle${NBSP}: A`,
    `Salle${NBSP}:`,
    `Salle${NBSP}:{name}`,
    'https://example.test',
    '19:30',
    `«${NBSP}mot${NBSP}»`,
    '',
  ])('passes %j', (text) => {
    expect(typographyViolations(text)).toEqual([]);
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
