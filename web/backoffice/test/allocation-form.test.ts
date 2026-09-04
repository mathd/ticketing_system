import { describe, expect, expectTypeOf, it } from 'vitest';

import {
  allocationErrors,
  allocationRowsFromAvailability,
  parseAllocationForm,
  parseAllocationRevision,
  toAllocationRequest,
  toMinuteInput,
  UnzonedReleaseTime,
  BlankedReleaseTime,
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
  clearRelease: false,
  ...over,
});
const REVISION = 0;

describe('stored allocation mapping', () => {
  it('builds a complete untouched form row', () => {
    expect(allocationRowsFromAvailability([{
      channel: 'reseller-acme',
      cap: 40,
      release_at: '2026-09-01T10:15:30.123Z',
    }])).toEqual([{
      channel: 'reseller-acme',
      cap: '40',
      releaseAt: '2026-09-01T10:15:30Z',
      clearRelease: false,
    }]);
  });
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
      REVISION,
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
    // correct answer everywhere — and it is the submitted components, since the
    // parser returns those rather than Date's millisecond-capped serialisation.
    expect(a.release_at).toBe('2026-09-01T10:00:00Z');
  });

  // Absent optional fields must be OMITTED, not sent as empty strings: the contract
  // types them as date-time/uuid, so "" fails request validation.
  it('omits the optional fields that are unset rather than sending empty values', () => {
    const [a] = toAllocationRequest(
      '11111111-1111-1111-1111-111111111111',
      [row()],
      [held()],
      REVISION,
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
    expect(parsed[0]).toEqual({ channel: 'reseller-acme', cap: '40', releaseAt: '', clearRelease: false });

    // And they reach the wire only from the server's set.
    const [a] = toAllocationRequest(
      '11111111-1111-1111-1111-111111111111',
      parsed,
      [held({ requires_code: false })],
      REVISION,
    ).allocations;
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
    toAllocationRequest('11111111-1111-1111-1111-111111111111', [r], c, REVISION).allocations[0];

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
    expect(a.release_at).toBe('2026-09-02T08:00:00Z');
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

    // ASSERTED AS AN INSTANT, not as a string, and that is the point of the test.
    // The parser now returns the submitted components verbatim, so comparing text
    // would only prove the three inputs differ — which they visibly do — while
    // saying nothing about what instant each one denotes. Parsing the result is
    // what makes the assertion discriminating: it is the same question the server
    // asks, and it is the question the zone defect got wrong.
    const at = (v: string | undefined) => new Date(v!).getTime();
    expect(at(utc.release_at)).toBe(Date.UTC(2026, 8, 1, 10, 0, 0));
    expect(at(toronto.release_at)).toBe(Date.UTC(2026, 8, 1, 14, 0, 0));
    expect(at(paris.release_at)).toBe(Date.UTC(2026, 8, 1, 8, 0, 0));

    // And the three are genuinely distinct instants, which is what a server-zone
    // collapse would destroy: under the old behaviour all three read as one.
    expect(new Set([at(utc.release_at), at(toronto.release_at), at(paris.release_at)]).size).toBe(3);
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

  // ai-review pass 2, [high]. The FIRST version of this test used only
  // '2026-13-01T10:00:00Z', which `new Date` happens to reject — one malformation
  // class generalised to all of them. `Date` NORMALISES the rest instead:
  // 2026-02-30 becomes March 2, 2026-04-31 becomes May 1, hour 24 becomes the next
  // day. Each matches the input's `pattern`, so an operator who typed a wrong date
  // got a redirect reporting success and a gate moved by DAYS.
  //
  // Enumerated by malformation CLASS rather than by example (AGENTS.md): syntax,
  // range, and the normalising impossibilities that are the actual hazard here.
  it.each([
    ['month 13 — rejected by Date, the only class the first version covered', '2026-13-01T10:00:00Z'],
    ['February 30 — Date NORMALISES this to March 2', '2026-02-30T10:00:00Z'],
    ['April 31 — normalises to May 1', '2026-04-31T10:00:00Z'],
    ['February 29 in a non-leap year — normalises to March 1', '2025-02-29T10:00:00Z'],
    ['hour 24 — legal ISO 8601 end-of-day, but means the NEXT day', '2026-09-01T24:00:00Z'],
    ['minute 60', '2026-09-01T10:60:00Z'],
    // A leap second is LEGAL RFC 3339, and JavaScript cannot represent it —
    // new Date(...) is Invalid Date. Refused because there is no instant to store,
    // which is a different reason from the malformations around it.
    ['second 60, a leap second JavaScript cannot represent', '2026-12-31T23:59:60Z'],
    // Finer than MICROSECONDS — seven digits. Six are accepted and preserved (below);
    // beyond that PostgreSQL timestamptz has nowhere to put the digits either, so the
    // refusal is the honest answer rather than a silent round.
    //
    // Two earlier versions got this wrong in opposite directions. One accepted
    // `.123456Z` and asserted it stored as `.123Z`, encoding JavaScript's truncation
    // as the requirement (ai-review pass 4, [medium]). The next refused anything past
    // three digits — which refused a value the database itself produces.
    ['a fraction finer than microseconds, which nothing downstream can store', '2026-09-01T10:17:43.1234567Z'],
    ['day 0', '2026-09-00T10:00:00Z'],
    // Refused by the Date parse, NOT by an offset-range rule: there is no offset
    // Date accepts that such a rule would reject, so the rule this file used to
    // carry was structurally inert and was deleted (ai-review pass 3, [low]).
    ['an out-of-range offset, refused by the Date parse', '2026-09-01T10:00:00+99:00'],
    ['no zone at all', '2026-09-02T08:00'],
  ])('throws on %s', (_name, value) => {
    expect(() => build({ ...row(), releaseAt: value }, [current()])).toThrow(UnzonedReleaseTime);
  });

  it('accepts the boundaries a real calendar allows', () => {
    // The negative cases above must not have been bought by refusing everything.
    // February 29 in a LEAP year is legal; so is a seconds-less value, and an
    // offset at the edge of the range.
    // The value is the SUBMITTED components, normalised only in the ways the parser
    // states: seconds defaulted, `z`/`t` upper-cased. It is NOT Date's serialisation,
    // so an offset is preserved rather than converted to UTC — Go parses either
    // spelling to the same instant, and keeping the operator's offset keeps the
    // microseconds Date would drop.
    const leap = build({ ...row(), releaseAt: '2028-02-29T10:00:00Z' }, [current()]);
    expect(leap!.release_at).toBe('2028-02-29T10:00:00Z');

    const noSeconds = build({ ...row(), releaseAt: '2026-09-01T10:00Z' }, [current()]);
    expect(noSeconds!.release_at).toBe('2026-09-01T10:00:00Z');

    const edgeOffset = build({ ...row(), releaseAt: '2026-09-01T10:00:00+14:00' }, [current()]);
    expect(edgeOffset!.release_at).toBe('2026-09-01T10:00:00+14:00');
  });

  // ai-review pass 3, (a). A validator that refuses LEGITIMATE input is a new
  // defect, and the first component-wise version refused three RFC 3339 forms the
  // implementation it replaced had accepted. The fractional case is the one that
  // would have bitten: stored values carry MICROSECONDS, so a fraction is exactly
  // what an operator pasting from a log or the database types.
  it.each([
    ['a lowercase zone marker, which RFC 3339 permits', '2026-09-01T10:00:00z', '2026-09-01T10:00:00Z'],
    ['fractional seconds', '2026-09-01T10:00:00.500Z', '2026-09-01T10:00:00.500Z'],
    // The case the whole precision contract exists for: six digits, preserved exactly.
    // Date.toISOString() would return `.123Z` here, losing 456µs.
    ['microseconds, preserved to the digit', '2026-09-01T10:17:43.123456Z', '2026-09-01T10:17:43.123456Z'],
    ['microseconds on an offset zone', '2026-09-01T06:17:43.123456-04:00', '2026-09-01T06:17:43.123456-04:00'],
    ['a lowercase date/time separator', '2026-09-01t10:00:00Z', '2026-09-01T10:00:00Z'],
    ['+00:00, which is Z spelled out', '2026-09-01T10:00:00+00:00', '2026-09-01T10:00:00+00:00'],
    ['-00:00', '2026-09-01T10:00:00-00:00', '2026-09-01T10:00:00-00:00'],
  ])('accepts %s', (_name, value, want) => {
    expect(build({ ...row(), releaseAt: value }, [current()])!.release_at).toBe(want);
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

  // ai-review pass 2, [medium]. Blanking a free-text field is far easier to reach
  // by accident than typing a malformed value — which is refused outright — so
  // removal is an explicit act. Blank-means-clear predates this ticket (the
  // datetime-local input behaved the same way); the free-text field is what makes
  // the accident cheap.
  it('THROWS when a set release time is blanked without confirmation', () => {
    expect(() => build({ ...row(), releaseAt: '' }, [current()])).toThrow(BlankedReleaseTime);
  });

  it('removes the gate when the operator ticks the confirmation', () => {
    const a = build({ ...row(), releaseAt: '', clearRelease: true }, [current()]);
    expect(a).not.toHaveProperty('release_at');
  });

  it('a blank field where no gate was set is a no-op, not a refusal', () => {
    // The refusal is about DESTROYING something. There is nothing to destroy here,
    // so refusing would block a save for no reason.
    const noRelease = { ...current(), release_at: undefined };
    const a = build({ ...row(), releaseAt: '' }, [noRelease]);
    expect(a).not.toHaveProperty('release_at');
  });

  it('the confirmation wins even if text is also present', () => {
    // Ticked and non-empty is contradictory; removal is the safer reading of an
    // explicit act, and it must not fall through to parsing the leftover text.
    const a = build({ ...row(), releaseAt: '2026-09-02T08:00:00Z', clearRelease: true }, [current()]);
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
        [row({ releaseAt: toMinuteInput(current().release_at) }), row({ channel: 'brand-new', cap: '10' })],
        [current()],
        REVISION,
      ),
    ).toThrow(UnknownAllocationChannel);
  });

  // The match is by EXACT channel code (ADR-024 opacity), so a whitespace variant is a
  // DIFFERENT channel — and therefore also refused rather than silently treated as new.
  it('refuses a whitespace variant of a known code', () => {
    expect(() =>
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row({ releaseAt: toMinuteInput(current().release_at) }), row({ channel: ' reseller-acme ' })],
        [current()],
        REVISION,
      ),
    ).toThrow(UnknownAllocationChannel);
  });

  it('names the offending channel on the refusal, so the page can say which', () => {
    try {
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row({ releaseAt: toMinuteInput(current().release_at) }), row({ channel: 'ghost' })],
        [current()],
        REVISION,
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
    expect(() => toAllocationRequest('11111111-1111-1111-1111-111111111111', [], two, REVISION)).toThrow(
      MissingAllocationChannel,
    );
  });

  it('refuses a submission that drops one current channel', () => {
    expect(() =>
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row({ channel: 'reseller-acme' })],
        two,
        REVISION,
      ),
    ).toThrow(MissingAllocationChannel);
  });

  it('names the omitted channel, so the page can say which', () => {
    try {
      toAllocationRequest(
        '11111111-1111-1111-1111-111111111111',
        [row({ channel: 'reseller-acme' })],
        two,
        REVISION,
      );
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
      REVISION,
    );
    expect(req.allocations.map((a) => a.channel)).toEqual(['reseller-acme', 'presale']);
  });

  // An empty submission against an empty current set is not a deletion, so it is allowed.
  it('allows an empty submission when the slot has no allocations', () => {
    expect(toAllocationRequest('11111111-1111-1111-1111-111111111111', [], [], REVISION).allocations).toEqual([]);
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

  it('requires both the current set and its revision', () => {
    expectTypeOf<Parameters<typeof toAllocationRequest>>().toEqualTypeOf<[
      organizerId: string,
      rows: AllocationRow[],
      current: CurrentAllocation[],
      revision: number,
    ]>();
    expectTypeOf<ReturnType<typeof toAllocationRequest>['allocation_revision']>().toEqualTypeOf<number>();
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
