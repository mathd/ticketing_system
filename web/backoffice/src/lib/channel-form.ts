// Turning catalog's refusals into errors beside the right field (TKT-236).
//
// Pure and separate from the page so the mapping is unit-testable without a
// server: an error banner that says "invalid channel" satisfies a naive
// assertion and fails the ticket's COS, which asks for the message beside the
// field the operator has to fix.
//
// Catalog's refusals arrive as one `{error}` string with an HTTP status. The
// store deliberately returns ONE sentinel for every bound violation
// (`ErrChannelInvalidInput`, mapped to a single 400 message naming all three
// bounds), so the status alone cannot say which field is wrong. This module
// re-derives that from the SUBMITTED VALUES, which the page still holds — the
// same check the store ran, run again locally for presentation only.
//
// That duplication is deliberate and bounded. It is never the authority: the
// server refuses regardless, and if these bounds ever disagree with the SQL the
// server still wins and the operator sees the generic message rather than a
// wrong one.

import type { ChannelKind } from './catalog';

/** The four kinds catalog's closed enum accepts (TKT-235, migration 0018). */
export const CHANNEL_KINDS = ['web', 'pos', 'presale', 'reseller'] as const;

/**
 * Bounds mirrored from `0018_channels.sql`. CHARACTERS, not bytes — PostgreSQL's
 * `length(text)` and OpenAPI's `maxLength` both count characters, and TKT-237
 * found what counting bytes costs. `[...s].length` counts code points; `s.length`
 * would count UTF-16 units and disagree with the server on astral input.
 */
export const CHANNEL_CODE_MAX = 100;
export const CHANNEL_DISPLAY_NAME_MAX = 200;

export type ChannelField = 'code' | 'display_name' | 'kind';

/** Errors keyed by field, plus a form-level message for anything unattributable. */
export interface ChannelFormErrors {
  fields: Partial<Record<ChannelField, string>>;
  form?: string;
}

export interface ChannelSubmission {
  code: string;
  displayName: string;
  kind: string;
}

function codePoints(value: string): number {
  return [...value].length;
}

/**
 * Client-side field validation, run before the request.
 *
 * Presentation only — it never decides whether the write happens. Its job is to
 * put a message beside a field the operator can see, so a submit that the server
 * would refuse anyway does not come back as one opaque line.
 */
export function validateSubmission(input: ChannelSubmission): ChannelFormErrors {
  const fields: Partial<Record<ChannelField, string>> = {};

  const codeLength = codePoints(input.code);
  if (codeLength === 0) {
    fields.code = 'A channel code is required.';
  } else if (codeLength > CHANNEL_CODE_MAX) {
    fields.code = `A channel code is at most ${CHANNEL_CODE_MAX} characters (this one is ${codeLength}).`;
  }

  const nameLength = codePoints(input.displayName);
  if (nameLength === 0) {
    fields.display_name = 'A display name is required.';
  } else if (nameLength > CHANNEL_DISPLAY_NAME_MAX) {
    fields.display_name = `A display name is at most ${CHANNEL_DISPLAY_NAME_MAX} characters (this one is ${nameLength}).`;
  }

  if (!(CHANNEL_KINDS as readonly string[]).includes(input.kind)) {
    fields.kind = 'Choose one of web, pos, presale or reseller.';
  }

  return { fields };
}

/**
 * Map a catalog refusal onto a field, using the submitted values to disambiguate.
 *
 * The status carries most of the signal:
 *   409 → a duplicate code, or an attempted rename. Both are about `code`.
 *   400 → a bound or the kind enum; which one is re-derived from the input.
 * Anything else is not attributable to a field and stays form-level, because
 * guessing would put a message beside a field that is fine.
 */
export function classifyCatalogError(
  status: number,
  message: string,
  input: ChannelSubmission,
): ChannelFormErrors {
  if (status === 409) {
    // Catalog answers 409 for both "this organizer already has that code" and
    // "the code is immutable". Its message distinguishes them and is written for
    // an operator, so it is passed through rather than replaced.
    return { fields: { code: message } };
  }

  if (status === 400) {
    const derived = validateSubmission(input);
    if (Object.keys(derived.fields).length > 0) {
      return derived;
    }
    // A 400 the local rules cannot explain. Do NOT invent a field: catalog
    // refused for a reason this page does not model, and attaching it to `code`
    // would send the operator to edit something that is correct.
    return { fields: {}, form: message };
  }

  // 5xx, a transport failure, or an unmapped status. Deliberately generic: the
  // upstream message may name internals, and none of it is actionable here.
  return {
    fields: {},
    form: 'Catalog could not be reached. The change was not saved — try again.',
  };
}

export function hasErrors(errors: ChannelFormErrors): boolean {
  return errors.form !== undefined || Object.keys(errors.fields).length > 0;
}

/** Narrow a raw form value to the kind union, or undefined if it is not one. */
export function asChannelKind(value: string): ChannelKind | undefined {
  return (CHANNEL_KINDS as readonly string[]).includes(value) ? (value as ChannelKind) : undefined;
}

/**
 * Read an HTML checkbox from submitted form data.
 *
 * **An unchecked checkbox submits NOTHING.** Absent means false, and that is the
 * opposite of `ChannelCreate`'s `default: true` in the contract — where an
 * omitted `enabled` means enabled. The two must not be conflated:
 *
 *   - CREATE sends no `enabled` key at all when the box is checked, so the
 *     contract's default applies; it sends `false` explicitly when unchecked.
 *   - UPDATE always sends an explicit boolean, because the PUT is a full
 *     replacement and an omitted field would be read as `false` rather than as
 *     "leave it alone".
 *
 * Named and centralised because reading it inline is how a disable silently
 * re-enables on the next save.
 */
export function checkboxChecked(value: FormDataEntryValue | null): boolean {
  return value !== null;
}
