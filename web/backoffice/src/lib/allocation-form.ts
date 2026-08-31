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
function instant(value: string): string | undefined {
  if (!value) return undefined;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/** Minute-precision rendering of an instant, matching what a datetime-local input holds. */
export function toMinuteInput(value: string | undefined): string {
  if (!value) return '';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * The instant to submit for one editable timestamp, given what the SERVER currently holds.
 *
 * An untouched field keeps the server's value byte for byte: a `datetime-local` input holds
 * minutes, so re-deriving from it drops seconds and fractions, and these boundaries are
 * compared against `clock_timestamp()` — a cap edit would bring a release or a window
 * forward by up to a minute (ai-review pass 1, [high]).
 *
 * "Untouched" is decided against the CURRENT SERVER VALUE, never against a hidden input
 * echoing what was rendered (ai-review pass 2, [high]). Hidden fields are client-controlled:
 * a crafted or stale POST could otherwise present any `original` it liked and have it
 * written verbatim — including boundaries this screen does not expose for editing at all.
 * The server value is not forgeable and is re-read on every request.
 */
function preservedInstant(submitted: string, current: string | undefined): string | undefined {
  if (current && submitted === toMinuteInput(current)) {
    return current;
  }
  return instant(submitted);
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
      const release = preservedInstant(r.releaseAt, held.release_at);
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
