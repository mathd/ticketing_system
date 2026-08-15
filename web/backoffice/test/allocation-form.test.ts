import { describe, expect, it } from 'vitest';

import {
  allocationErrors,
  parseAllocationForm,
  toAllocationRequest,
  type AllocationRow,
} from '../src/lib/allocation-form';

// TKT-244. The allocation editor's pure half: turning a submitted form into the
// full-set replace inventory expects, and turning inventory's refusals into a message
// beside the field an operator has to fix.
//
// Kept out of the page so the mapping is unit-testable without a server, exactly as
// channel-form.ts is for TKT-236: an error banner reading "invalid allocation" satisfies
// a naive assertion and fails this ticket's COS, which asks for the message beside the
// field.

const row = (over: Partial<AllocationRow> = {}): AllocationRow => ({
  channel: 'reseller-acme',
  cap: '40',
  releaseAt: '',
  opensAt: '',
  closesAt: '',
  requiresCode: 'false',
  soldBy: '',
  ...over,
});

describe('the full set survives a round trip', () => {
  // The write is a full-set atomic replace that DELETEs and re-INSERTs (ADR-024), so a
  // field the form does not carry is a field the save DESTROYS. For sold_by that is an
  // authorization regression: TKT-246 judges it under the pool row lock, so blanking it
  // returns a reseller's bound stock to the public pool.
  it('carries every field, including the ones the operator cannot edit', () => {
    const req = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      row({
        cap: '50',
        releaseAt: '2026-09-01T10:00',
        opensAt: '2026-08-01T09:00',
        closesAt: '2026-08-31T23:00',
        requiresCode: 'true',
        soldBy: '22222222-2222-2222-2222-222222222222',
      }),
    ]);

    expect(req.organizer_id).toBe('11111111-1111-1111-1111-111111111111');
    expect(req.allocations).toHaveLength(1);
    const [a] = req.allocations;
    expect(a.channel).toBe('reseller-acme');
    expect(a.cap).toBe(50);
    expect(a.requires_code).toBe(true);
    expect(a.sold_by).toBe('22222222-2222-2222-2222-222222222222');
    // A `datetime-local` value carries no zone; the contract types these as date-time,
    // so each must arrive as an explicit instant denoting the same moment the operator
    // picked. Asserted as a round trip rather than a literal — pinning the UTC string
    // would encode the machine's zone into the test.
    expect(new Date(a.release_at!).getTime()).toBe(new Date('2026-09-01T10:00').getTime());
    expect(new Date(a.opens_at!).getTime()).toBe(new Date('2026-08-01T09:00').getTime());
    expect(new Date(a.closes_at!).getTime()).toBe(new Date('2026-08-31T23:00').getTime());
    for (const v of [a.release_at, a.opens_at, a.closes_at]) {
      expect(v).toMatch(/Z$/); // zoned, or inventory's request validation refuses it
    }
  });

  // Absent optional fields must be OMITTED, not sent as empty strings: the contract
  // types them as date-time/uuid, so "" fails request validation.
  it('omits the optional fields that are unset rather than sending empty values', () => {
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [row()]).allocations;
    expect(a).not.toHaveProperty('release_at');
    expect(a).not.toHaveProperty('opens_at');
    expect(a).not.toHaveProperty('closes_at');
    expect(a).not.toHaveProperty('sold_by');
  });
});

describe('requires_code is read as an explicit value, never as a checkbox', () => {
  // AGENTS.md: a hidden input ALWAYS submits, so `value=""` is present-and-empty and
  // reads as true under checkbox semantics ("the key is there"). That is exactly how
  // renaming a disabled channel silently re-enabled it in TKT-236. The parser must
  // decide on the VALUE, not on the key's presence.
  it.each([
    ['true', true],
    ['false', false],
    ['', false],
  ])('parses requires_code=%o as %o', (value, want) => {
    const form = new FormData();
    form.set('channel.0', 'reseller-acme');
    form.set('cap.0', '40');
    form.set('requiresCode.0', value);
    expect(parseAllocationForm(form)[0].requiresCode === 'true').toBe(want);
  });

  it('an empty hidden requires_code does not gate the channel', () => {
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      row({ requiresCode: '' }),
    ]).allocations;
    expect(a.requires_code).toBe(false);
  });
});

describe('a refusal lands beside the field the operator must fix', () => {
  it('puts an over-capacity refusal on the total, not on a row', () => {
    const errs = allocationErrors(
      { code: 'allocation_caps_exceed_capacity', error: 'channel allocations exceed pool capacity' },
      [row({ channel: 'presale' }), row({ channel: 'reseller-acme' })],
    );
    expect(errs.total).toMatch(/capacity/i);
    expect(errs.rows).toEqual({});
  });

  // The channel comes from the SERVER. A client cannot re-derive which row is below
  // consumption the way TKT-236's channel form re-derives its length bounds: those are
  // static, while consumption moves between the read that fills the form and the write
  // that submits it, so a local guess can name the wrong row with full confidence.
  it('puts a below-consumption refusal on the row the server named', () => {
    const errs = allocationErrors(
      {
        code: 'allocation_cap_below_consumption',
        channel: 'reseller-acme',
        error: 'channel "reseller-acme" is allocated below its current consumption',
      },
      [row({ channel: 'presale' }), row({ channel: 'reseller-acme' })],
    );
    expect(errs.rows).toHaveProperty('reseller-acme');
    expect(errs.rows['reseller-acme']).toMatch(/sold|consumption|held/i);
    expect(errs.rows).not.toHaveProperty('presale');
    expect(errs.total).toBeUndefined();
  });

  // A code the client does not know about must not vanish: an unattributable refusal
  // still has to reach the operator, as a form-level message.
  it('surfaces an unrecognised refusal at form level rather than dropping it', () => {
    const errs = allocationErrors({ code: 'slot_archived', error: 'slot archived' }, [row()]);
    expect(errs.form).toMatch(/archived/i);
  });

  it('surfaces a refusal with no code at all', () => {
    const errs = allocationErrors({ error: 'something inventory did not classify' }, [row()]);
    expect(errs.form).toMatch(/inventory did not classify/);
  });

  // If the server names a channel the form no longer shows, the message must not be
  // silently discarded — the operator would see a rejected save with no explanation.
  it('falls back to form level when the named channel is not on the form', () => {
    const errs = allocationErrors(
      { code: 'allocation_cap_below_consumption', channel: 'ghost-channel', error: 'below consumption' },
      [row({ channel: 'presale' })],
    );
    expect(errs.rows).toEqual({});
    expect(errs.form).toMatch(/ghost-channel/);
  });
});

describe('client-side validation is presentation only', () => {
  it('refuses a non-positive or non-numeric cap beside the row', () => {
    for (const cap of ['0', '-1', 'abc', '']) {
      const errs = allocationErrors(null, [row({ cap })]);
      expect(errs.rows['reseller-acme']).toBeTruthy();
    }
  });

  it('accepts a valid set', () => {
    expect(allocationErrors(null, [row()])).toEqual({ rows: {} });
  });

  // Duplicate channel codes are refused by inventory with a 400 before the pool lock;
  // catching them here puts the message on the offending row instead.
  it('refuses a duplicate channel code', () => {
    const errs = allocationErrors(null, [row(), row()]);
    expect(errs.rows['reseller-acme']).toMatch(/twice|duplicate/i);
  });
});

// ai-review pass 1, [high] × 2. Both findings were about the round trip CORRUPTING a
// value rather than dropping it — a class the existing tests could not see, because they
// asserted presence and not fidelity.
describe('the round trip preserves values byte for byte, not just presence', () => {
  // ADR-024: channel codes are "exact opaque strings — no normalization, no case
  // folding", and the contract permits any 1..100 characters. So `" reseller "` is a
  // legal and DISTINCT code. Trimming it submits a different identity: the full-set
  // replace deletes the original row and inserts the trimmed one, while live claims keep
  // the original code — stranding that consumption and detaching the allocation from fee
  // and split rules keyed on the exact string.
  it.each([
    [' reseller-acme '],
    ['reseller acme'],
    ['  '],
    ['présale'],
    ['POS/Booth #2'],
  ])('submits the channel code %o verbatim', (code) => {
    const form = new FormData();
    form.set('channel.0', code);
    form.set('cap.0', '40');
    form.set('requiresCode.0', 'false');
    const [parsed] = parseAllocationForm(form);
    expect(parsed.channel).toBe(code);
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      { ...row(), channel: parsed.channel },
    ]).allocations;
    expect(a.channel).toBe(code);
  });

  // A `datetime-local` input holds MINUTES. release_at/opens_at/closes_at are timestamptz
  // compared against clock_timestamp(), so re-deriving an untouched boundary from the
  // input would move it up to a minute earlier — returning inventory to public sale, or
  // opening admission, sooner than configured. An unrelated cap edit must not move it.
  it('keeps an untouched timestamp EXACTLY, seconds and all', () => {
    const exact = '2026-09-01T10:17:43.123456Z';
    const rendered = '2026-09-01T10:17'; // what the input showed, minutes only
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      {
        ...row(),
        releaseAt: rendered,
        renderedReleaseAt: rendered,
        originalReleaseAt: exact,
      },
    ]).allocations;
    expect(a.release_at).toBe(exact);
  });

  it('takes the operator’s new value when they DID edit the field', () => {
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      {
        ...row(),
        releaseAt: '2026-09-02T08:00',
        renderedReleaseAt: '2026-09-01T10:17',
        originalReleaseAt: '2026-09-01T10:17:43.123456Z',
      },
    ]).allocations;
    expect(new Date(a.release_at!).getTime()).toBe(new Date('2026-09-02T08:00').getTime());
    expect(a.release_at).not.toBe('2026-09-01T10:17:43.123456Z');
  });

  it('lets the operator CLEAR a timestamp', () => {
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      {
        ...row(),
        releaseAt: '',
        renderedReleaseAt: '2026-09-01T10:17',
        originalReleaseAt: '2026-09-01T10:17:43.123456Z',
      },
    ]).allocations;
    expect(a).not.toHaveProperty('release_at');
  });

  it('preserves all three boundaries independently', () => {
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', [
      {
        ...row(),
        releaseAt: '2026-09-01T10:17',
        opensAt: '2026-08-01T09:05',
        closesAt: '2026-08-31T23:59',
        renderedReleaseAt: '2026-09-01T10:17',
        renderedOpensAt: '2026-08-01T09:05',
        renderedClosesAt: '2026-08-31T23:59',
        originalReleaseAt: '2026-09-01T10:17:43.123456Z',
        originalOpensAt: '2026-08-01T09:05:11.500000Z',
        originalClosesAt: '2026-08-31T23:59:59.999999Z',
      },
    ]).allocations;
    expect(a.release_at).toBe('2026-09-01T10:17:43.123456Z');
    expect(a.opens_at).toBe('2026-08-01T09:05:11.500000Z');
    expect(a.closes_at).toBe('2026-08-31T23:59:59.999999Z');
  });
});
