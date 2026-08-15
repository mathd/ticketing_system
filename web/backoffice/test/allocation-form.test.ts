import { describe, expect, it } from 'vitest';

import {
  allocationErrors,
  parseAllocationForm,
  toAllocationRequest,
  toMinuteInput,
  type AllocationRow,
  type CurrentAllocation,
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
    const req = toAllocationRequest(
      '11111111-1111-1111-1111-111111111111',
      [row({ cap: '50', releaseAt: '2026-09-01T10:00' })],
      // The fields this screen does not render come from inventory's current set, not the
      // form (ai-review pass 2): a client-carried copy can be stale or forged.
      [
        {
          channel: 'reseller-acme',
          opens_at: '2026-08-01T09:00:00.000000Z',
          closes_at: '2026-08-31T23:00:00.000000Z',
          requires_code: true,
          sold_by: '22222222-2222-2222-2222-222222222222',
        },
      ],
    );

    expect(req.organizer_id).toBe('11111111-1111-1111-1111-111111111111');
    expect(req.allocations).toHaveLength(1);
    const [a] = req.allocations;
    expect(a.channel).toBe('reseller-acme');
    expect(a.cap).toBe(50);
    expect(a.requires_code).toBe(true);
    expect(a.sold_by).toBe('22222222-2222-2222-2222-222222222222');
    expect(a.opens_at).toBe('2026-08-01T09:00:00.000000Z');
    expect(a.closes_at).toBe('2026-08-31T23:00:00.000000Z');
    // The release time IS editable, and a `datetime-local` value carries no zone, so it
    // must arrive as an explicit instant denoting the moment the operator picked.
    // Asserted as a round trip rather than a literal — pinning the UTC string would
    // encode the machine's timezone into the test.
    expect(new Date(a.release_at!).getTime()).toBe(new Date('2026-09-01T10:00').getTime());
    expect(a.release_at).toMatch(/Z$/); // zoned, or request validation refuses it
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

// ai-review pass 1 ([high] × 2) and pass 2 ([high] + [medium]). Three findings, one root
// cause: the round trip CORRUPTED values rather than dropping them, and pass 1's fix tried
// to carry the true values through the CLIENT — which the client can forge or hold stale.
//
// The fix pass 2 forced: every field this screen does not edit comes from the SERVER's
// current allocation set, read on the same request. The form contributes only what an
// operator can see and change.
describe('the write takes unrendered fields from the server, never from the client', () => {
  const current = (over: Partial<CurrentAllocation> = {}): CurrentAllocation => ({
    channel: 'reseller-acme',
    release_at: '2026-09-01T10:17:43.123456Z',
    opens_at: '2026-08-01T09:05:11.500000Z',
    closes_at: '2026-12-31T23:59:59.999999Z',
    requires_code: true,
    sold_by: '22222222-2222-2222-2222-222222222222',
    ...over,
  });

  const build = (r: AllocationRow, c: CurrentAllocation[]) =>
    toAllocationRequest('11111111-1111-1111-1111-111111111111', [r], c).allocations[0];

  // The invariant, without naming the implementation: EDITING A CAP CHANGES THE CAP AND
  // NOTHING ELSE — to the microsecond, because these boundaries are compared against
  // clock_timestamp() and a truncated one still reads as "set".
  it('a cap edit moves no boundary and touches no binding', () => {
    const a = build({ ...row(), cap: '50', releaseAt: toMinuteInput(current().release_at) }, [current()]);
    expect(a.cap).toBe(50);
    expect(a.release_at).toBe('2026-09-01T10:17:43.123456Z');
    expect(a.opens_at).toBe('2026-08-01T09:05:11.500000Z');
    expect(a.closes_at).toBe('2026-12-31T23:59:59.999999Z');
    expect(a.requires_code).toBe(true);
    expect(a.sold_by).toBe('22222222-2222-2222-2222-222222222222');
  });

  // pass 2 [high]. Every one of these is a field a crafted or stale POST could carry; none
  // may reach the wire when the server holds a value for that row.
  it('ignores forged hidden values and uses what inventory holds', () => {
    const a = build(
      {
        ...row(),
        cap: '50',
        releaseAt: toMinuteInput(current().release_at),
        requiresCode: 'false', // forged: try to un-gate a presale channel
        soldBy: '99999999-9999-9999-9999-999999999999', // forged: try to re-assign the seller
      },
      [current()],
    );
    expect(a.requires_code).toBe(true);
    expect(a.sold_by).toBe('22222222-2222-2222-2222-222222222222');
    // The window is not on the form at all, so it can only come from the server.
    expect(a.opens_at).toBe('2026-08-01T09:05:11.500000Z');
    expect(a.closes_at).toBe('2026-12-31T23:59:59.999999Z');
  });

  // pass 2 [medium]. A `datetime-local` selection means the displayed minute. Re-picking
  // the SAME minute is not an edit, so the stored instant stands — the previous fix could
  // not tell this from a stale echo, because both were client strings.
  it('re-picking the same displayed minute keeps the stored instant', () => {
    const a = build({ ...row(), releaseAt: toMinuteInput(current().release_at) }, [current()]);
    expect(a.release_at).toBe('2026-09-01T10:17:43.123456Z');
  });

  it('a genuinely different minute replaces the stored instant', () => {
    const a = build({ ...row(), releaseAt: '2026-09-02T08:00' }, [current()]);
    expect(new Date(a.release_at!).getTime()).toBe(new Date('2026-09-02T08:00').getTime());
    expect(a.release_at).not.toBe('2026-09-01T10:17:43.123456Z');
  });

  it('clearing the release time removes it', () => {
    const a = build({ ...row(), releaseAt: '' }, [current()]);
    expect(a).not.toHaveProperty('release_at');
  });

  // A row inventory does not hold is NEW, so the form is the only source it has.
  it('takes a new row entirely from the form', () => {
    const a = build({ ...row(), channel: 'brand-new', cap: '10', requiresCode: 'true' }, [current()]);
    expect(a.channel).toBe('brand-new');
    expect(a.requires_code).toBe(true);
    expect(a).not.toHaveProperty('opens_at');
    expect(a).not.toHaveProperty('sold_by');
  });

  // The server match is by EXACT channel code, so a whitespace variant is a different row
  // — which is the same opacity rule that forbids trimming (ADR-024).
  it('matches the server row by the exact channel code', () => {
    const a = build({ ...row(), channel: ' reseller-acme ' }, [current()]);
    expect(a.channel).toBe(' reseller-acme ');
    expect(a).not.toHaveProperty('opens_at'); // no server row under that code
  });
});

describe('the channel code is opaque and never normalized', () => {
  // ADR-024: codes are "exact opaque strings — no normalization, no case folding", and the
  // contract permits any 1..100 characters. Trimming would submit a DIFFERENT identity: the
  // full-set replace deletes the original row and inserts the trimmed one while live claims
  // keep the original code, stranding that consumption.
  it.each([[' reseller-acme '], ['reseller acme'], ['  '], ['présale'], ['POS/Booth #2']])(
    'reads the channel code %o verbatim',
    (code) => {
      const form = new FormData();
      form.set('channel.0', code);
      form.set('cap.0', '40');
      form.set('requiresCode.0', 'false');
      expect(parseAllocationForm(form)[0].channel).toBe(code);
    },
  );
});
