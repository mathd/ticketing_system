import { describe, expect, it } from 'vitest';

import {
  allocationErrors,
  parseAllocationForm,
  toAllocationRequest,
  toMinuteInput,
  UnknownAllocationChannel,
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
  ...over,
});

/** The current server-side set, which is the ONLY source for unrendered fields. */
const held = (over: Partial<CurrentAllocation> = {}): CurrentAllocation => ({
  channel: 'reseller-acme',
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
      // The fields this screen does not render come from inventory's current set, and
      // ONLY from there — they are not submittable at all (ai-review passes 2 and 3).
      [
        held({
          opens_at: '2026-08-01T09:00:00.000000Z',
          closes_at: '2026-08-31T23:00:00.000000Z',
          requires_code: true,
          sold_by: '22222222-2222-2222-2222-222222222222',
        }),
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
    const [a] = toAllocationRequest(
      '11111111-1111-1111-1111-111111111111',
      [row()],
      [held()],
    ).allocations;
    expect(a).not.toHaveProperty('release_at');
    expect(a).not.toHaveProperty('opens_at');
    expect(a).not.toHaveProperty('closes_at');
    expect(a).not.toHaveProperty('sold_by');
    // An allocation inventory holds without a gate stays ungated. `false` is explicit
    // rather than omitted, because it is what UN-gates a channel.
    expect(a.requires_code).toBe(false);
  });
});

describe('the form cannot carry a field this screen does not edit', () => {
  // ai-review pass 3, [high]. The earlier defence made these fields server-sourced when
  // the submitted channel matched a current row — but the channel is client-supplied, so
  // a row naming an unknown code fell back to client values. The type no longer has the
  // fields at all, and the parser ignores them: a field the form cannot submit cannot be
  // forged, whatever channel key is presented.
  it('ignores requires_code and sold_by if a crafted POST sends them', () => {
    const form = new FormData();
    form.set('channel.0', 'reseller-acme');
    form.set('cap.0', '40');
    form.set('requiresCode.0', 'true');
    form.set('soldBy.0', '99999999-9999-9999-9999-999999999999');
    form.set('opensAt.0', '2030-01-01T00:00');
    const parsed = parseAllocationForm(form);
    expect(parsed[0]).toEqual({ channel: 'reseller-acme', cap: '40', releaseAt: '' });

    // And they reach the wire only from the server's set.
    const [a] = toAllocationRequest('11111111-1111-1111-1111-111111111111', parsed, [
      held({ requires_code: false }),
    ]).allocations;
    expect(a.requires_code).toBe(false);
    expect(a).not.toHaveProperty('sold_by');
    expect(a).not.toHaveProperty('opens_at');
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

  // pass 2 [high]. Whatever the form carries, the gate, the binding and the window come
  // from inventory — the row type cannot even express them.
  it('sources the gate, the binding and the window from inventory alone', () => {
    const a = build({ ...row(), cap: '50', releaseAt: toMinuteInput(current().release_at) }, [current()]);
    expect(a.requires_code).toBe(true);
    expect(a.sold_by).toBe('22222222-2222-2222-2222-222222222222');
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

  // ai-review pass 3, [high]. A row naming a channel inventory does not hold is REFUSED,
  // not treated as new.
  //
  // An earlier version of this test asserted the opposite — "takes a new row entirely from
  // the form" — and it was green, because it pinned what the code did rather than what the
  // requirement is. That is the defect being blessed by a passing test, and no mutation
  // check could have found it: the mutant flips the mechanism, and the assertion was
  // written to match the mechanism.
  //
  // The requirement, stated without naming the implementation: THIS SCREEN CHANGES
  // EXISTING ALLOCATIONS AND CREATES NONE. Inventory validates only channel, cap,
  // duplicates, capacity and consumption — it never constrains sold_by — so a row that
  // could arrive unmatched is a row whose seller binding the client chooses.
  it('refuses a row naming a channel inventory does not hold', () => {
    expect(() => build({ ...row(), channel: 'brand-new', cap: '10' }, [current()])).toThrow(
      UnknownAllocationChannel,
    );
  });

  // The match is by EXACT channel code (ADR-024 opacity), so a whitespace variant is a
  // DIFFERENT channel — and therefore also refused rather than silently treated as new.
  it('refuses a whitespace variant of a known code', () => {
    expect(() => build({ ...row(), channel: ' reseller-acme ' }, [current()])).toThrow(
      UnknownAllocationChannel,
    );
  });

  it('names the offending channel on the refusal, so the page can say which', () => {
    try {
      build({ ...row(), channel: 'ghost' }, [current()]);
      expect.unreachable('should have thrown');
    } catch (e) {
      expect((e as UnknownAllocationChannel).channel).toBe('ghost');
    }
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
