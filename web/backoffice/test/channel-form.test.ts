import { describe, expect, it } from 'vitest';

import {
  CHANNEL_CODE_MAX,
  CHANNEL_DISPLAY_NAME_MAX,
  asChannelKind,
  checkboxChecked,
  classifyCatalogError,
  hasErrors,
  validateSubmission,
} from '../src/lib/channel-form';

const ok = { code: 'pos', displayName: 'Box office', kind: 'pos' };

describe('validateSubmission', () => {
  it('accepts a well-formed submission', () => {
    expect(hasErrors(validateSubmission(ok))).toBe(false);
  });

  it('names the field that is wrong, not the form', () => {
    expect(validateSubmission({ ...ok, code: '' }).fields).toEqual({
      code: 'A channel code is required.',
    });
    expect(validateSubmission({ ...ok, displayName: '' }).fields).toEqual({
      display_name: 'A display name is required.',
    });
    expect(validateSubmission({ ...ok, kind: 'partner' }).fields).toEqual({
      kind: 'Choose one of web, pos, presale or reseller.',
    });
  });

  it('reports every wrong field at once, so one submit fixes one round', () => {
    const errors = validateSubmission({ code: '', displayName: '', kind: 'nope' });
    expect(Object.keys(errors.fields).sort()).toEqual(['code', 'display_name', 'kind']);
  });

  // TKT-237's lesson, applied to the UI: PostgreSQL's length() and OpenAPI's
  // maxLength both count CHARACTERS. `s.length` counts UTF-16 units, so an
  // astral-plane code of 100 characters would measure 200 and be refused here
  // while the server accepts it — the client disagreeing with the server about
  // what is legal.
  it('counts characters, not UTF-16 units', () => {
    const astral = '\u{1F3AB}'.repeat(CHANNEL_CODE_MAX); // 100 code points, 200 UTF-16 units
    expect(astral.length).toBe(CHANNEL_CODE_MAX * 2); // the trap, made visible
    expect(hasErrors(validateSubmission({ ...ok, code: astral }))).toBe(false);

    const tooLong = '\u{1F3AB}'.repeat(CHANNEL_CODE_MAX + 1);
    expect(validateSubmission({ ...ok, code: tooLong }).fields.code).toContain('at most');
  });

  it('bounds the display name at its own limit', () => {
    const atLimit = 'é'.repeat(CHANNEL_DISPLAY_NAME_MAX);
    expect(hasErrors(validateSubmission({ ...ok, displayName: atLimit }))).toBe(false);
    const over = 'é'.repeat(CHANNEL_DISPLAY_NAME_MAX + 1);
    expect(validateSubmission({ ...ok, displayName: over }).fields.display_name).toContain('at most');
  });

  // Codes are exact and case-sensitive (ADR-024). The form must not normalise
  // what the server stores verbatim.
  it('accepts codes that differ only by case or spacing', () => {
    for (const code of ['pos', 'POS', ' pos', 'pos ']) {
      expect(hasErrors(validateSubmission({ ...ok, code }))).toBe(false);
    }
  });
});

describe('classifyCatalogError', () => {
  it('attaches a 409 to the code field and passes catalog’s message through', () => {
    const errors = classifyCatalogError(409, 'this organizer already has a channel with that code', ok);
    expect(errors.fields.code).toBe('this organizer already has a channel with that code');
    expect(errors.form).toBeUndefined();
  });

  it('re-derives which field a 400 is about', () => {
    const errors = classifyCatalogError(400, 'invalid channel: …', { ...ok, code: '' });
    expect(errors.fields.code).toBeDefined();
    expect(errors.form).toBeUndefined();
  });

  // The case that stops a guess: a 400 nothing local explains must NOT be
  // attached to a field, or the operator edits something that is already right.
  it('leaves an unexplained 400 at form level rather than blaming a field', () => {
    const errors = classifyCatalogError(400, 'invalid channel: something else', ok);
    expect(errors.fields).toEqual({});
    expect(errors.form).toBe('invalid channel: something else');
  });

  it('keeps a 5xx generic and never echoes the upstream message', () => {
    const errors = classifyCatalogError(500, 'pq: connection refused on 10.0.0.4', ok);
    expect(errors.fields).toEqual({});
    expect(errors.form).not.toContain('10.0.0.4');
    expect(errors.form).toContain('not saved');
  });
});

describe('asChannelKind', () => {
  it('narrows the four kinds and rejects everything else', () => {
    expect(asChannelKind('reseller')).toBe('reseller');
    for (const bad of ['', 'partner', 'WEB', 'web ']) {
      expect(asChannelKind(bad)).toBeUndefined();
    }
  });
});

// The defect the plan's pre-mortem predicted: an unchecked HTML checkbox
// submits NOTHING, so absent means false. That is the opposite of
// ChannelCreate's `default: true`, and conflating them is how a disable
// silently re-enables on the next save.
describe('checkboxChecked', () => {
  it('reads an absent value as false and any present value as true', () => {
    expect(checkboxChecked(null)).toBe(false);
    expect(checkboxChecked('on')).toBe(true);
    // A checkbox with an explicit value="false" is still CHECKED — the browser
    // only submits it when checked, so the string is irrelevant.
    expect(checkboxChecked('false')).toBe(true);
    expect(checkboxChecked('')).toBe(true);
  });
});
