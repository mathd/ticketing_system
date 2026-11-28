import { describe, expect, it } from 'vitest';

import { UI_STRINGS } from '../src/lib/locales';

// TKT-531: Québec French typography (ADR-089). The rule is the owner's working rule until
// TKT-593 (native-speaker review) confirms or changes it. This helper stays local to this
// suite; the back office and commerce keep their own copies (ADR-089).

const NBSP = '\u00a0';
const NNBSP = '\u202f';

// Each predicate is one entry, so each one can be tested and mutated alone. A match
// means the rule is broken. White_Space is the Unicode property: a tab, a no-break space
// and a narrow no-break space count as spaces, not only U+0020.
const RULES: ReadonlyArray<readonly [string, RegExp]> = [
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
  return RULES.filter(([, broken]) => broken.test(text)).map(([rule]) => rule);
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

/** Every French message by id: `key` for a plain string, `key.form` for each plural form. */
function frenchMessages(): Array<[string, string]> {
  return Object.entries(UI_STRINGS.fr).flatMap(([key, value]): Array<[string, string]> =>
    typeof value === 'string'
      ? [[key, value]]
      : Object.entries(value).map(([form, text]): [string, string] => [`${key}.${form}`, text]));
}

describe('the French table (ADR-089)', () => {
  it('scans every plural form, not only plain strings', () => {
    const ids = frenchMessages().map(([id]) => id);
    expect(ids).toContain('seatsNoLongerAvailable.one');
    expect(ids).toContain('seatsNoLongerAvailable.other');
  });

  it('follows the typography rule in every string and every plural form', () => {
    const broken = Object.fromEntries(
      frenchMessages()
        .map(([id, text]) => [id, typographyViolations(text)] as const)
        .filter(([, rules]) => rules.length > 0),
    );
    expect(broken).toEqual({});
  });
});
