// The channel-allocation editor's pure half (TKT-244): form → request, refusal → field.
//
// Separate from the page so the mapping is unit-testable without a server, exactly as
// channel-form.ts is for the channel registry (TKT-236). A banner reading "invalid
// allocation" satisfies a naive assertion and fails this ticket's COS, which asks for
// the message beside the field the operator has to fix.
//
// THE ONE THING THIS MODULE EXISTS TO GET RIGHT, and it took three review passes.
//
// The write is a FULL-SET ATOMIC REPLACE under the pool lock (ADR-024): inventory DELETEs
// every allocation row and re-INSERTs from what was submitted. So a field the request does
// not carry is a field the save DESTROYS, and a field it DOES carry is a field the client
// chooses. `sold_by` is the dangerous one — TKT-246 judges it in the claim paths under the
// pool row lock, so both losing it and choosing it are authorization changes, and neither
// is visible in a screenshot.
//
// Inventory cannot help: it validates the channel, the cap, duplicates, pool capacity and
// consumption, and nothing else. It never constrains `sold_by`. So this module is the only
// place the boundary exists, and it holds it by construction rather than by checking:
//
//   * the row type carries ONLY what the screen renders — channel, cap, release time;
//   * every other field comes from inventory's CURRENT set, read on the same request;
//   * a submitted row must match a current one by EXACT channel code, or it is refused.
//
// The three passes each broke a weaker version: carrying the values in hidden inputs let a
// crafted POST supply them; sourcing them from the server but keying on the client's
// channel let an unmatched row fall back to client values. A field that cannot be
// submitted cannot be forged.

import type { components } from './inventory-api-types.gen';

export type ChannelAllocation = components['schemas']['ChannelAllocation'];
export type ChannelAllocationSet = components['schemas']['ChannelAllocationSet'];

/**
 * One editor row — ONLY what this screen lets an operator change (ai-review pass 3).
 *
 * There is deliberately no `requiresCode` and no `soldBy` here, and no window. This
 * screen edits **caps and release times on allocations that already exist**; it does not
 * create allocations, set seller bindings, or gate channels behind presale codes.
 *
 * That is a security boundary, not a scope note. The write is a full-set replace, so any
 * field this type can carry is a field a crafted POST can choose — and inventory
 * validates only the channel, the cap, duplicates, pool capacity and consumption. It
 * never constrains `sold_by`, so the back office is the ONLY place that boundary exists.
 * Pass 2 moved these fields to a server merge keyed on the channel; pass 3 found the key
 * itself is client-supplied, so a row whose code matches nothing restored client control.
 * A field that cannot be submitted cannot be forged.
 */
export interface AllocationRow {
  channel: string;
  cap: string;
  releaseAt: string;
  /**
   * The operator asked to REMOVE this release gate (ai-review pass 2, [medium]).
   *
   * A real checkbox, so absent-means-false holds: `form.get` returns null when it
   * is unticked, which is reachable. A hidden input would always submit and
   * `value=""` would read as present-and-empty — the trap TKT-236 paid for.
   */
  clearRelease: boolean;
}

/**
 * One allocation as inventory reports it right now — the trustworthy source for every
 * field this screen does not render (ai-review pass 2, [high]).
 *
 * Structurally the subset of `ChannelAvailability` the write needs, declared here rather
 * than imported so this module stays free of the generated client.
 */
export interface CurrentAllocation {
  channel: string;
  release_at?: string;
  opens_at?: string;
  closes_at?: string;
  requires_code?: boolean;
  sold_by?: string;
}

/** Errors keyed by channel code, plus the total and a form-level fallback. */
export interface AllocationFormErrors {
  rows: Record<string, string>;
  total?: string;
  form?: string;
}

/** Inventory's refusal body: `{error}` always, `code`/`channel` on the coded 409s. */
export interface InventoryRefusal {
  error?: string;
  code?: string;
  channel?: string;
}

/**
 * Read the submitted rows off a form.
 *
 * Fields are indexed (`cap.0`, `cap.1`, …) rather than repeated, so a row keeps its
 * identity even when a value is empty — repeated names would collapse and silently
 * shift every later row's values by one.
 */
export function parseAllocationForm(form: FormData): AllocationRow[] {
  const rows: AllocationRow[] = [];
  for (let i = 0; form.has(`channel.${i}`); i++) {
    const at = (name: string) => String(form.get(`${name}.${i}`) ?? '').trim();
    // The channel code is read VERBATIM — never trimmed (ai-review pass 1, [high]).
    //
    // ADR-024: channel codes are "exact opaque strings — no normalization, no case
    // folding", and the contract permits any 1..100 characters, so `" reseller "` is a
    // legal and DISTINCT code. Trimming it here would submit a different identity: the
    // full-set replace would delete the original row and insert the trimmed one, while
    // live claims keep the original code. The consumption check would then run against a
    // code nothing has consumed — stranding that consumption, bypassing the
    // below-consumption refusal, and detaching the allocation from fee and split rules
    // keyed on the exact string.
    const verbatim = (name: string) => String(form.get(`${name}.${i}`) ?? '');
    rows.push({
      channel: verbatim('channel'),
      cap: at('cap'),
      releaseAt: at('releaseAt'),
      clearRelease: form.get(`clearRelease.${i}`) === 'true',
    });
  }
  return rows;
}

/**
 * Read the allocation-set revision the page was rendered from (TKT-250).
 *
 * Returns undefined when the field is missing or is not a non-negative integer, and the
 * page treats that as a refusal rather than as "replace unconditionally" — inventory
 * would accept the omission from an internal caller, but the back office never holds
 * that credential (ADR-057), so a missing revision here means a malformed submission,
 * not a licence to overwrite.
 *
 * A hidden input is the right shape for this and the wrong shape for the fields TKT-244
 * removed, which is worth stating because the two look identical in the HTML. Those
 * fields were TRUSTED by the server (`sold_by` decides who may sell), so client control
 * of them was the defect. This one is COMPARED by the server and grants nothing.
 */
export function parseAllocationRevision(form: FormData): number | undefined {
  const raw = String(form.get('allocationRevision') ?? '').trim();
  if (!raw) return undefined;
  const n = Number(raw);
  return Number.isInteger(n) && n >= 0 ? n : undefined;
}

/**
 * A `datetime-local` value carries no zone. Inventory's contract types these as
 * `date-time`, so they are sent as an explicit UTC instant rather than a bare local
 * string — an unzoned value is not a valid date-time and would be refused by request
 * validation.
 */
/**
 * A zoned instant, or `undefined` for an empty field. A NON-EMPTY value without a
 * zone is refused (TKT-302).
 *
 * The refusal is the fix. `new Date('2026-09-01T10:00')` resolves a zoneless
 * string in the SSR PROCESS's zone, so an operator's edited release time was
 * shifted by (server TZ - operator TZ) — silently, with a 303 reporting success.
 * The repo already refuses this shape one page over: events/new.astro takes RFC
 * 3339 with an offset "because a local value carries no zone".
 *
 * Refused rather than guessed, and refused rather than treated as empty: an
 * unparseable non-empty value must not be read as "clear this boundary", which
 * would remove a release gate the operator was trying to edit.
 */
function instant(channel: string, value: string): string | undefined {
  if (!value) return undefined;
  const parsed = parseZonedInstant(value);
  if (parsed === undefined) {
    throw new UnzonedReleaseTime(channel, value);
  }
  return parsed;
}

/**
 * RFC 3339 with a zone, validated COMPONENT BY COMPONENT, or undefined.
 *
 * `new Date()` is not a validator, and delegating to it was an ai-review [high]
 * (second pass). It NORMALIZES impossible dates instead of rejecting them:
 *
 *     2026-02-30T10:00:00Z -> 2026-03-02T10:00:00.000Z   (two days later)
 *     2026-04-31T10:00:00Z -> 2026-05-01T10:00:00.000Z
 *     2026-09-01T24:00:00Z -> 2026-09-02T00:00:00.000Z   (the next day)
 *
 * Each satisfies the input's `pattern`, so an operator who typed a wrong date got
 * a redirect reporting success and a release gate moved by days. The first
 * version of this check tested `2026-13-01T10:00:00Z`, which `Date` happens to
 * reject — one malformation class generalised to all of them.
 *
 * So: parse the components, bound each, check the day against the real month
 * length and the fraction width, then let `Date` refuse what those rules cannot
 * express.
 *
 * PRECISION IS A CONTRACT, and this one is MICROSECONDS — six fractional digits,
 * which is what PostgreSQL `timestamptz` stores. A finer fraction is REFUSED, and
 * the refusal says so.
 *
 * The value returned is the string assembled below, NOT `Date`'s serialisation.
 * That distinction is the whole mechanism. JavaScript Date holds milliseconds, so
 * returning `parsed.toISOString()` silently drops the rest: `.123456` would store
 * as `.123`. These boundaries are compared against `clock_timestamp()`, so that is
 * a release gate moved by up to 999µs with no warning — small, real, and exactly
 * the class of silent shift this ticket exists to remove.
 *
 * Two earlier versions each got half of this. One accepted and truncated, with a
 * test encoding the truncation as if it were the requirement (ai-review pass 4,
 * [medium]). The next refused any fraction past three digits, on the reasoning that
 * preserving microseconds meant not converting through Date at all. It does not:
 * Date stays as the final validity check, and the component string is what travels.
 * Refusing was itself the defect — the stored values carry microseconds, so an
 * operator pasting one back from a log or the database was refused a value this
 * system had produced.
 *
 * An UNTOUCHED field is unaffected either way — preservedInstant returns the stored
 * value byte for byte — and the field renders without fractions, so this only
 * matters when someone types or pastes them deliberately.
 *
 * That last step is NOT a round-trip comparison, and an earlier version of this
 * comment said it was (ai-review pass 3, [low]). Nothing here compares the
 * reconstructed components against the submitted ones — `Date` simply parses and
 * serialises. It still earns its place: a leap second passes every range check
 * above and Date refuses it, which is the reachable input that only this step
 * catches.
 */
function parseZonedInstant(value: string): string | undefined {
  // Case-insensitive `Z` and optional fractional seconds, because RFC 3339 permits
  // both and the previous implementation accepted them. Tightening a validator into
  // refusing legitimate operator input is a new defect, not a fix (ai-review pass 3):
  // the stored values carry MICROSECONDS, so a fraction is exactly what someone
  // pasting from a log or the database would type.
  const m = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2})(?::(\d{2})(\.\d+)?)?([Zz]|[+-]\d{2}:\d{2})$/.exec(
    value.trim(),
  );
  if (!m) return undefined;
  const [, y, mo, d, h, mi, sec, frac, zone] = m;
  const year = Number(y);
  const month = Number(mo);
  const day = Number(d);
  const hour = Number(h);
  const minute = Number(mi);
  const second = sec === undefined ? 0 : Number(sec);

  // Microseconds at most: six fractional digits. `frac` includes its leading dot, so
  // seven characters is six digits. See the precision contract above.
  if (frac !== undefined && frac.length > 7) return undefined;
  if (month < 1 || month > 12) return undefined;
  // Real month length, leap years included: day 0 of the NEXT month is the last
  // day of this one. Uses UTC so the host's zone cannot change the answer.
  const daysInMonth = new Date(Date.UTC(year, month, 0)).getUTCDate();
  if (day < 1 || day > daysInMonth) return undefined;
  // Hour 24 is legal in ISO 8601 for end-of-day but means the NEXT day, which is
  // not what an operator typing it into a release field intends. Refused rather
  // than normalised.
  // Second 60 is a LEAP SECOND, which RFC 3339 permits and JavaScript cannot
  // represent: `new Date('2026-12-31T23:59:60Z')` is Invalid Date, not a
  // normalisation. Refused — not because it is malformed, but because there is no
  // instant to store. Checked rather than assumed: an earlier version of this
  // comment claimed Date collapsed it into the next minute. It does not.
  //
  // Accepting it would mean choosing an instant on the operator's behalf, and a
  // release gate is not the place for that.
  if (hour > 23 || minute > 59 || second > 60) return undefined;

  // NO explicit offset-range guard, and its absence is deliberate.
  //
  // An earlier version had one, placed AFTER the Date call where it could never
  // run (ai-review pass 3, [low]). Moving it before the call did not help either:
  // enumerating every offset matching [+-]dd:dd shows there is NO value Date
  // accepts that an hours<=23 / minutes<=59 rule would reject. The guard was
  // structurally inert, not mis-ordered, so it is deleted rather than repaired —
  // AGENTS.md's rule for a mechanism whose unreachability is a property of the
  // algorithm rather than a decision someone plans to reverse.
  //
  // What refuses +99:00 is the Date parse below, and its test now says so.
  const iso = `${y}-${mo}-${d}T${h}:${mi}:${String(second).padStart(2, '0')}${frac ?? ''}${
    zone.toUpperCase() === 'Z' ? 'Z' : zone
  }`;
  // Date is the last check, not the first: it catches what the component rules
  // cannot represent — a leap second is the reachable example, since second 60
  // passes every range check above and Date still refuses it.
  const parsed = new Date(iso);
  if (Number.isNaN(parsed.getTime())) return undefined;
  // The COMPONENT string, not `parsed.toISOString()`. Date parsed it only to reject
  // what the range rules cannot express; its own serialisation is millisecond-capped
  // and would silently truncate the microseconds `iso` carries.
  return iso;
}

/**
 * Whether an RFC 3339 date-time carries a zone: a trailing `Z`, or `+HH:MM` /
 * `-HH:MM` after the time.
 *
 * Anchored to the END so the `-` separators in the DATE cannot satisfy it —
 * `2026-09-01T10:00` must not read as zoned because it contains dashes.
 */
export function hasExplicitZone(value: string): boolean {
  return /(?:Z|[+-]\d{2}:\d{2})$/.test(value.trim());
}

/**
 * How an instant is rendered back into the editable field: UTC, with an explicit
 * `Z`, to the second.
 *
 * Was minute-precision LOCAL time (`getFullYear()`/`getHours()`/...), which is
 * the other half of the same defect — the render used the server's zone too, so
 * a round trip through this screen moved every boundary by the server/operator
 * offset even when nothing was edited. Rendering in UTC makes the value
 * self-describing: whatever zone the operator's browser is in, the field says
 * which instant it means.
 *
 * Seconds are kept because these boundaries are compared against
 * `clock_timestamp()`; truncating to minutes is what preservedInstant exists to
 * avoid, and rendering at minute precision would reintroduce it for any field
 * the operator DOES touch.
 */
export function toMinuteInput(value: string | undefined): string {
  if (!value) return '';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '';
  // toISOString is always UTC with a trailing Z; drop the milliseconds, which no
  // operator types and which the round-trip comparison does not need.
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/**
 * The instant to submit for one editable timestamp, given what the SERVER currently holds.
 *
 * An untouched field keeps the server's value byte for byte, because the rendered form of an
 * instant is LOSSY and re-deriving from it would truncate. That was up to a minute when the
 * input was `datetime-local`; since TKT-302 the field renders to the SECOND, so the loss is
 * sub-second — still real, because these boundaries are compared against `clock_timestamp()`
 * and the stored values carry microseconds (ai-review pass 1, [high]).
 *
 * The comparison is against `toMinuteInput(current)`, so it tracks whatever that renders. It
 * held when both sides were server-local minutes and it holds now that both are UTC seconds;
 * what would break it is changing one side alone. The browser spec's microsecond assertions on
 * the untouched row are what prove it end to end.
 *
 * "Untouched" is decided against the CURRENT SERVER VALUE, never against a hidden input
 * echoing what was rendered (ai-review pass 2, [high]). Hidden fields are client-controlled:
 * a crafted or stale POST could otherwise present any `original` it liked and have it
 * written verbatim — including boundaries this screen does not expose for editing at all.
 * The server value is not forgeable and is re-read on every request.
 */
function preservedInstant(
  channel: string,
  submitted: string,
  clearRelease: boolean,
  current: string | undefined,
): string | undefined {
  // Explicit removal, and the ONLY way to reach it. Blank alone is refused below.
  if (clearRelease) return undefined;
  if (current && submitted === toMinuteInput(current)) {
    return current;
  }
  // A blank field where one WAS set is not a request to remove it — it is far too
  // easy to reach by accident in a free-text input, and clearing a gate is
  // destructive (ai-review pass 2, [medium]). Refuse and make the operator say so.
  // Blank where none was set is a no-op and stays legal.
  if (!submitted && current) {
    throw new BlankedReleaseTime(channel);
  }
  return instant(channel, submitted);
}

/**
 * Thrown when a submitted row names a channel inventory does not currently hold.
 *
 * The page turns this into a form-level "reload" message. It is a REFUSAL rather than a
 * fallback on purpose (ai-review pass 3): treating an unmatched row as "new" would let a
 * crafted POST invent a channel and choose its seller binding and presale gate, because
 * inventory validates neither. This screen edits existing allocations; creating one is a
 * different operation with a different authorization story.
 */
export class UnknownAllocationChannel extends Error {
  constructor(public readonly channel: string) {
    super(`no current allocation for channel ${JSON.stringify(channel)}`);
    this.name = 'UnknownAllocationChannel';
  }
}

/**
 * Thrown when a submitted set OMITS a channel inventory currently holds.
 *
 * The other half of the same rule (ai-review pass 4): checking that every submitted row
 * exists is only half a boundary, because the write is a full-set replace and omission is
 * therefore DELETION. A crafted submission could drop `channel.3`, or send nothing at all,
 * and silently destroy those allocations — their seller bindings and presale gates with
 * them — while the page redirected as though the save had succeeded.
 *
 * This screen edits existing allocations: it creates none and deletes none. Both
 * directions of that sentence need enforcing.
 */
/**
 * Thrown when a release time is present but not a zoned RFC 3339 instant.
 *
 * REFUSING is the point, and returning `undefined` was not refusing (ai-review [high]).
 * `toAllocationRequest` omits `release_at` when it is undefined, and the write is a
 * FULL-SET REPLACE — so an unparseable value took the same path as a deliberately emptied
 * field and CLEARED the release gate, then redirected as a successful save. That is
 * destructive, and worse than the timezone shift this ticket set out to fix: the operator
 * asked to change a boundary and silently removed it.
 *
 * The input's `pattern` stops the ordinary zoneless shape in the browser, which is a
 * convenience and not a boundary: it accepts syntactically valid impossibilities like
 * `2026-13-01T10:00:00Z`, and any caller that ignores the markup submits whatever it likes.
 * This is the server-side half.
 *
 * Empty stays empty: an operator clearing the field still removes the release gate, which
 * is a real thing to want. Only a NON-EMPTY unusable value throws.
 */
export class UnzonedReleaseTime extends Error {
  constructor(
    public readonly channel: string,
    public readonly submitted: string,
  ) {
    super(`release time for channel ${JSON.stringify(channel)} is not a zoned RFC 3339 instant`);
    this.name = 'UnzonedReleaseTime';
  }
}

/**
 * Thrown when a release time that WAS set is submitted blank without the explicit
 * removal checkbox (ai-review pass 2, [medium]).
 *
 * Blank-means-clear predates this ticket — the `datetime-local` input behaved the
 * same way — but the free-text field this ticket introduces makes an accidental
 * blank easier to reach, and it asked for LESS input than a malformed value, which
 * is refused outright. Removal is now an explicit act.
 */
export class BlankedReleaseTime extends Error {
  constructor(public readonly channel: string) {
    super(`release time for channel ${JSON.stringify(channel)} was blanked without confirmation`);
    this.name = 'BlankedReleaseTime';
  }
}

export class MissingAllocationChannel extends Error {
  constructor(public readonly channel: string) {
    super(`submitted set omits channel ${JSON.stringify(channel)}`);
    this.name = 'MissingAllocationChannel';
  }
}

/**
 * Build the full-set replace body.
 *
 * `current` is the allocation set as inventory reports it RIGHT NOW, and it is the ONLY
 * source for every field the form does not render — the sales window, the presale gate
 * and the seller binding. The form contributes exactly three things, all of them visible
 * to the operator: which row, its cap, and its release time.
 *
 * Every submitted row must match a current one by EXACT channel code. That check is what
 * makes the merge trustworthy: the channel is client-supplied, so without it a row naming
 * an unknown code would fall through to client-chosen values for fields this screen never
 * shows.
 *
 * `revision` is the ONE field this module deliberately takes from the client (TKT-250),
 * and it does not breach the rule above. The rule exists because a submitted value that
 * the server TRUSTS is a value the client can forge — `sold_by` decides who may sell.
 * A revision decides nothing: the server compares it against its own current value under
 * the pool lock, so every possible client choice lands in exactly one of two outcomes —
 * it matches and the save proceeds on its merits, or it does not and the save is refused.
 * There is no third branch where a chosen value grants something. A forged revision can
 * only cost its sender their own save.
 *
 * It must come from the page ORIGINALLY RENDERED, not from the fresh read taken during
 * this POST. That is the entire mechanism: the fresh read is what makes the merge safe,
 * and reusing it as the revision would compare the server's current value against itself
 * and match every time — a precondition that cannot fail is not one.
 */
export function toAllocationRequest(
  organizerId: string,
  rows: AllocationRow[],
  current: CurrentAllocation[] = [],
  revision?: number,
): ChannelAllocationSet {
  const byChannel = new Map(current.map((c) => [c.channel, c]));
  // Every current allocation must be present in the submission. Omission is DELETION on a
  // full-set replace, so a sparse or empty form would silently destroy rows — and their
  // seller bindings with them — while the page reported success (ai-review pass 4).
  const submittedChannels = new Set(rows.map((r) => r.channel));
  for (const c of current) {
    if (!submittedChannels.has(c.channel)) {
      throw new MissingAllocationChannel(c.channel);
    }
  }
  return {
    organizer_id: organizerId,
    // Omitted when undefined rather than sent as null: inventory distinguishes absent
    // (replace unconditionally) from present (compare), and the back office is the
    // caller that must always be in the second case.
    ...(revision === undefined ? {} : { allocation_revision: revision }),
    allocations: rows.map((r) => {
      const held = byChannel.get(r.channel);
      if (!held) {
        throw new UnknownAllocationChannel(r.channel);
      }
      const a: ChannelAllocation = {
        channel: r.channel,
        cap: Number(r.cap),
        // Always explicit. `false` is meaningful — it is what un-gates a channel — so it
        // must not be dropped by an `if (truthy)` the way the optional fields are.
        requires_code: Boolean(held.requires_code),
      };
      // The optional fields are OMITTED when unset rather than sent empty: the contract
      // types them as date-time/uuid, and "" fails request validation.
      const release = preservedInstant(r.channel, r.releaseAt, r.clearRelease, held.release_at);
      if (release) a.release_at = release;
      if (held.opens_at) a.opens_at = held.opens_at;
      if (held.closes_at) a.closes_at = held.closes_at;
      if (held.sold_by) a.sold_by = held.sold_by;
      return a;
    }),
  };
}

/**
 * Map a refusal — or the absence of one — onto fields.
 *
 * Pass `null` to run only the client-side checks, before the request. Those are
 * PRESENTATION ONLY: they never decide whether the write happens, and the server refuses
 * regardless. Their job is to keep a submit the server would reject anyway from coming
 * back as one opaque line.
 *
 * Note what is deliberately NOT re-derived locally: which channel is below its
 * consumption. TKT-236's channel form re-derives its bounds because those are static —
 * a code's length is the same on both sides. Consumption is live and moves between the
 * read that filled this form and the write that submits it, so a local guess can name
 * the wrong row with total confidence. Only the server knows.
 */
export function allocationErrors(
  refusal: InventoryRefusal | null,
  rows: AllocationRow[],
): AllocationFormErrors {
  const errors: AllocationFormErrors = { rows: {} };

  const seen = new Set<string>();
  for (const r of rows) {
    if (!r.channel) {
      errors.form = 'Every allocation needs a channel code.';
      continue;
    }
    if (seen.has(r.channel)) {
      errors.rows[r.channel] = `“${r.channel}” appears twice — each channel may hold one allocation.`;
      continue;
    }
    seen.add(r.channel);
    const cap = Number(r.cap);
    if (!r.cap || !Number.isInteger(cap) || cap < 1) {
      errors.rows[r.channel] = 'Cap must be a whole number of 1 or more.';
    }
  }
  if (!refusal) return errors;

  const message = refusal.error ?? 'Inventory refused the change.';
  switch (refusal.code) {
    case 'allocation_caps_exceed_capacity':
      // Names no channel on purpose: the sum is a property of the whole set, so every
      // row shares the blame and attributing it to one would point at an arbitrary field.
      errors.total = 'These caps add up to more than the slot’s capacity. Lower one or more before saving.';
      return errors;
    case 'allocation_cap_below_consumption': {
      const channel = refusal.channel ?? '';
      if (channel && rows.some((r) => r.channel === channel)) {
        errors.rows[channel] =
          'This cap is below what the channel has already sold or is holding. Raise it to at least its current consumption.';
        return errors;
      }
      // The server named a channel this form does not show. Do not drop the refusal:
      // the operator would see a rejected save with no explanation.
      errors.form = channel
        ? `Inventory refused: “${channel}” is allocated below its current consumption. Reload to see the current set.`
        : message;
      return errors;
    }
    case 'allocation_window_reversed': {
      // The one 400 in this switch: the submitted set is malformed rather than
      // unacceptable, so the remedy is to fix a field and not to change a number
      // (TKT-307).
      //
      // NOT REACHABLE FROM THIS SCREEN TODAY, and the case exists anyway. This editor
      // does not expose opens_at/closes_at — it preserves them server-side precisely
      // because a hidden input would be forgeable (see slots/[id].astro) — so an
      // operator here cannot submit a reversed window. The refusal can still arrive:
      // inventory validates every submitted row, and this form PUTs the whole set
      // including boundaries it did not render. Without this case the message reaches
      // the operator as an unattributed form-level string via `default`, which is
      // survivable and vaguer than it needs to be.
      //
      // The row message is therefore written to be useful to an operator who cannot
      // edit the field it names: it says what is wrong, not "fix this input".
      //
      // `channel` is OPTIONAL on this code even though it names a row — the server can
      // answer from a bare sentinel — so the fallback below is the normal path, not an
      // edge case, and dropping it would leave a rejected save with no explanation.
      const channel = refusal.channel ?? '';
      if (channel && rows.some((r) => r.channel === channel)) {
        errors.rows[channel] =
          'Inventory refused this channel’s sales window: it closes at or before it opens. This screen does not edit sales windows — the stored values need correcting at the source.';
        return errors;
      }
      errors.form = channel
        ? `Inventory refused: “${channel}” has a sales window that closes at or before it opens.`
        : message;
      return errors;
    }
    case 'allocation_revision_mismatch':
      // Form-level, and phrased as an instruction rather than a diagnosis: no field the
      // operator can see is wrong, so highlighting one would send them to fix a value
      // that is fine. Someone else saved while this page was open, and the only remedy
      // is to reload and re-apply — which is exactly what the message says.
      errors.form =
        'Someone else changed this slot’s allocations while this page was open, so nothing was saved. Reload to see the current set, then re-apply your change.';
      return errors;
    default:
      // An unrecognised code still has to reach the operator verbatim.
      errors.form = message;
      return errors;
  }
}
