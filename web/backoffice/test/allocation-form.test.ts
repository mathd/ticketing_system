import { describe, expect, it } from 'vitest';

import {
  allocationErrors,
  parseAllocationForm,
  parseAllocationRevision,
  toAllocationRequest,
  toMinuteInput,
  UnzonedReleaseTime,
  MissingAllocationChannel,
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
      [row({ cap: '50', releaseAt: '2026-09-01T10:00:00Z' })],
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
    // The release time IS editable and now arrives ZONED (TKT-302), so this is a
    // literal rather than a round trip. The old version compared
    // `new Date(a.release_at)` against `new Date('2026-09-01T10:00')` and noted
    // that pinning the UTC string "would encode the machine's timezone into the
    // test" — which was true, and was a symptom: both sides resolved a zoneless
    // string in the same local zone, so the assertion held on every machine
    // while the value it checked was wrong on all but one. A zoned input has one
    // correct answer everywhere.
    expect(a.release_at).toBe('2026-09-01T10:00:00.000Z');
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

  // TKT-307. The second per-row refusal, and the reason it needs its own case rather
  // than the default branch: the default surfaces the message at FORM level, which
  // reaches the operator but does not put it beside the window inputs they must fix —
  // which is the entire reason the server carries a channel on this code.
  it('puts a reversed-window refusal on the row the server named', () => {
    const errs = allocationErrors(
      {
        code: 'allocation_window_reversed',
        channel: 'presale',
        error: 'channel "presale" has closes_at at or before opens_at',
      },
      [row({ channel: 'presale' }), row({ channel: 'reseller-acme' })],
    );
    expect(errs.rows).toHaveProperty('presale');
    expect(errs.rows['presale']).toMatch(/window|closes|opens/i);
    expect(errs.rows).not.toHaveProperty('reseller-acme');
    expect(errs.form).toBeUndefined();
    expect(errs.total).toBeUndefined();
  });

  // Same fallback as the below-consumption case. Worth its own test rather than trust
  // in the copied shape: this is where a refusal gets silently dropped if the branch is
  // written to return nothing when the channel is unknown.
  it('falls back to form level when a reversed window names a channel not on the form', () => {
    const errs = allocationErrors(
      { code: 'allocation_window_reversed', channel: 'ghost-channel', error: 'window reversed' },
      [row({ channel: 'presale' })],
    );
    expect(errs.form).toMatch(/ghost-channel/);
    expect(errs.rows).toEqual({});
  });

  // The server can send this code with NO channel — the bare sentinel path in
  // problem() omits the field rather than panicking. The message must still arrive.
  it('surfaces a reversed-window refusal that names no channel', () => {
    const errs = allocationErrors(
      { code: 'allocation_window_reversed', error: 'allocation sales window closes at or before it opens' },
      [row({ channel: 'presale' })],
    );
    expect(errs.form).toMatch(/closes at or before it opens/);
    expect(errs.rows).toEqual({});
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

  it('a genuinely different instant replaces the stored one', () => {
    const a = build({ ...row(), releaseAt: '2026-09-02T08:00:00Z' }, [current()]);
    // Asserted as an exact UTC instant, derived from the SUBMITTED offset rather
    // than from whatever `new Date` would make of a zoneless string in the
    // server's zone. Before TKT-302 this test passed a bare local value and
    // compared it against `new Date` of the same bare value — self-consistent,
    // and blind to the shift it was supposed to catch.
    expect(a.release_at).toBe('2026-09-02T08:00:00.000Z');
  });

  // TKT-302. The defect: a zoneless value was resolved in the SSR process's
  // zone, so the stored instant depended on where the server was.
  it('takes the instant from the submitted OFFSET, not the server zone', () => {
    // Same wall-clock reading, three zones, three different instants. Under the
    // old `new Date(bare)` behaviour all three would have collapsed to whatever
    // the server's zone made of "10:00", which is the bug.
    const utc = build({ ...row(), releaseAt: '2026-09-01T10:00:00Z' }, [current()]);
    const toronto = build({ ...row(), releaseAt: '2026-09-01T10:00:00-04:00' }, [current()]);
    const paris = build({ ...row(), releaseAt: '2026-09-01T10:00:00+02:00' }, [current()]);

    expect(utc.release_at).toBe('2026-09-01T10:00:00.000Z');
    expect(toronto.release_at).toBe('2026-09-01T14:00:00.000Z');
    expect(paris.release_at).toBe('2026-09-01T08:00:00.000Z');
  });

  // ai-review [high]. The FIRST version of this test asserted that the row simply
  // carried no `release_at`, and called that a refusal. It was not: the write is a
  // full-set replace, so an omitted release_at CLEARS the stored release gate and
  // redirects as a successful save. The test encoded the defect — an omission and a
  // refusal are indistinguishable in the request body, and only one of them is safe.
  //
  // A non-empty unusable value must THROW, so nothing reaches inventory at all.
  it('THROWS on a non-empty value with no zone rather than clearing the gate', () => {
    expect(() => build({ ...row(), releaseAt: '2026-09-02T08:00' }, [current()])).toThrow(
      UnzonedReleaseTime,
    );
  });

  it('throws on a value that matches the input pattern but is not a real instant', () => {
    // The `pattern` attribute is a browser convenience, not a boundary: this shape
    // satisfies it and is still impossible. Month 13.
    expect(() => build({ ...row(), releaseAt: '2026-13-01T10:00:00Z' }, [current()])).toThrow(
      UnzonedReleaseTime,
    );
  });

  it('names the channel and the submitted text, so the error can sit beside the field', () => {
    try {
      build({ ...row(), releaseAt: '2026-09-02T08:00' }, [current()]);
      throw new Error('expected UnzonedReleaseTime');
    } catch (e) {
      expect(e).toBeInstanceOf(UnzonedReleaseTime);
      expect((e as UnzonedReleaseTime).channel).toBe(row().channel);
      expect((e as UnzonedReleaseTime).submitted).toBe('2026-09-02T08:00');
    }
  });

  it('an EMPTY release time still clears the gate — that is a real thing to want', () => {
    const a = build({ ...row(), releaseAt: '' }, [current()]);
    expect(a).not.toHaveProperty('release_at');
  });

  it('renders a stored instant in UTC with an explicit zone, to the second', () => {
    // The other half of the defect: rendering used getFullYear()/getHours(), so
    // a round trip through this screen moved every boundary by the server's
    // offset even when nothing was edited. UTC makes the field self-describing.
    expect(toMinuteInput('2026-09-01T10:17:43.123456Z')).toBe('2026-09-01T10:17:43Z');
    expect(toMinuteInput(undefined)).toBe('');
    expect(toMinuteInput('not a date')).toBe('');
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
    // The current row IS submitted, so the omission check below is satisfied and this
    // isolates the unknown-row rule.
    expect(() =>
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row(), row({ channel: 'brand-new', cap: '10' })],
        [current()],
      ),
    ).toThrow(UnknownAllocationChannel);
  });

  // The match is by EXACT channel code (ADR-024 opacity), so a whitespace variant is a
  // DIFFERENT channel — and therefore also refused rather than silently treated as new.
  it('refuses a whitespace variant of a known code', () => {
    expect(() =>
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row(), row({ channel: ' reseller-acme ' })],
        [current()],
      ),
    ).toThrow(UnknownAllocationChannel);
  });

  it('names the offending channel on the refusal, so the page can say which', () => {
    try {
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row(), row({ channel: 'ghost' })],
        [current()],
      );
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

// ai-review pass 4, [high]. The mirror of the refusal above: checking that every SUBMITTED
// row exists is only half a boundary, because the write is a full-set replace and omission
// is therefore deletion.
//
// The requirement, without naming the implementation: THIS SCREEN CHANGES EXISTING
// ALLOCATIONS — IT CREATES NONE AND DELETES NONE. A save that dropped rows would destroy
// their seller bindings and presale gates while the page reported success.
describe('a submitted set that omits a current allocation is refused', () => {
  const two = [
    { channel: 'reseller-acme', sold_by: '22222222-2222-2222-2222-222222222222' },
    { channel: 'presale', requires_code: true },
  ];

  it('refuses an EMPTY submission against a non-empty current set', () => {
    // The sharpest case: `channel.0` absent parses to no rows at all, and a full-set
    // replace with an empty list clears every allocation the slot has.
    expect(() => toAllocationRequest('11111111-1111-1111-1111-111111111111', [], two)).toThrow(
      MissingAllocationChannel,
    );
  });

  it('refuses a submission that drops one current channel', () => {
    expect(() =>
      toAllocationRequest('11111111-1111-1111-1111-111111111111', [row({ channel: 'reseller-acme' })], two),
    ).toThrow(MissingAllocationChannel);
  });

  it('names the omitted channel, so the page can say which', () => {
    try {
      toAllocationRequest('11111111-1111-1111-1111-111111111111', [row({ channel: 'reseller-acme' })], two);
      expect.unreachable('should have thrown');
    } catch (e) {
      expect((e as MissingAllocationChannel).channel).toBe('presale');
    }
  });

  it('accepts a submission covering exactly the current set', () => {
    const req = toAllocationRequest(
      '11111111-1111-1111-1111-111111111111',
      [row({ channel: 'reseller-acme' }), row({ channel: 'presale' })],
      two,
    );
    expect(req.allocations.map((a) => a.channel)).toEqual(['reseller-acme', 'presale']);
  });

  // An empty submission against an empty current set is not a deletion, so it is allowed.
  it('allows an empty submission when the slot has no allocations', () => {
    expect(toAllocationRequest('11111111-1111-1111-1111-111111111111', [], []).allocations).toEqual([]);
  });
});

// TKT-250. The allocation set carries a revision, so a save built on a stale read is
// refused instead of silently overwriting whoever saved in between.
describe('the allocation-set revision', () => {
  const org = '11111111-1111-1111-1111-111111111111';
  const current: CurrentAllocation[] = [{ channel: 'reseller-acme' }];

  it('is sent exactly as captured', () => {
    const req = toAllocationRequest(org, [row()], current, 7);
    expect(req.allocation_revision).toBe(7);
  });

  // Zero is a real revision — the value of a pool nobody has edited yet — and must be
  // SENT, not dropped as falsy. Dropped, inventory would read the request as "replace
  // unconditionally" and the first edit of every slot would be unprotected: exactly the
  // window in which two operators are most likely to be setting a slot up together.
  it('sends revision 0 rather than omitting it as falsy', () => {
    const req = toAllocationRequest(org, [row()], current, 0);
    expect(req.allocation_revision).toBe(0);
    expect('allocation_revision' in req).toBe(true);
  });

  // Omitted only when genuinely absent. The page refuses that case before calling this,
  // but the mapping must not invent a value of its own.
  it('omits the field entirely when no revision is supplied', () => {
    expect('allocation_revision' in toAllocationRequest(org, [row()], current)).toBe(false);
  });

  it('reads the revision from the submitted form', () => {
    const form = new FormData();
    form.set('allocationRevision', '12');
    expect(parseAllocationRevision(form)).toBe(12);
  });

  it('reads a submitted revision of 0 as the value 0, not as absence', () => {
    const form = new FormData();
    form.set('allocationRevision', '0');
    expect(parseAllocationRevision(form)).toBe(0);
  });

  // A missing or malformed revision is undefined, which the page turns into a refusal.
  // It must never become a number: any number here would be a guess presented to the
  // server as a fact.
  it.each([
    ['absent', undefined],
    ['empty', ''],
    ['not a number', 'abc'],
    ['negative', '-1'],
    ['fractional', '1.5'],
  ])('reads a %s revision as undefined', (_name, value) => {
    const form = new FormData();
    if (value !== undefined) form.set('allocationRevision', value);
    expect(parseAllocationRevision(form)).toBeUndefined();
  });

  // The refusal is form-level and tells the operator to reload. It must NOT land on a
  // row: every field they can see is fine, and pointing at one would send them to fix a
  // value that is not the problem.
  it('turns a stale-revision refusal into a reload instruction, not a field error', () => {
    const errors = allocationErrors(
      { error: 'conflict: allocation set revision mismatch', code: 'allocation_revision_mismatch' },
      [row()],
    );
    expect(errors.rows).toEqual({});
    expect(errors.total).toBeUndefined();
    expect(errors.form).toMatch(/reload/i);
  });
});
