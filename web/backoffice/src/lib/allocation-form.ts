// The channel-allocation editor's pure half (TKT-244): form → request, refusal → field.
//
// Separate from the page so the mapping is unit-testable without a server, exactly as
// channel-form.ts is for the channel registry (TKT-236). A banner reading "invalid
// allocation" satisfies a naive assertion and fails this ticket's COS, which asks for
// the message beside the field the operator has to fix.
//
// TWO THINGS THIS MODULE EXISTS TO GET RIGHT:
//
// 1. The write is a FULL-SET ATOMIC REPLACE under the pool lock (ADR-024): inventory
//    DELETEs every allocation row and re-INSERTs from what was submitted. So a field the
//    form does not carry is a field the save DESTROYS. `sold_by` is the dangerous one —
//    TKT-246 judges it in the claim paths under the pool row lock, so dropping it turns a
//    reseller's bound stock back into public stock. That is an authorization regression,
//    not a display bug, and it is invisible in a screenshot.
//
// 2. `requires_code` rides as a HIDDEN input, and a hidden input always submits. So
//    "the key is present" means nothing here and `value=""` must read as FALSE. Reading
//    it with checkbox semantics is how renaming a disabled channel silently re-enabled it
//    in TKT-236 (AGENTS.md: "a hidden input is not a checkbox").

import type { components } from './inventory-api-types.gen';

export type ChannelAllocation = components['schemas']['ChannelAllocation'];
export type ChannelAllocationSet = components['schemas']['ChannelAllocationSet'];

/** One editor row: every value as the form carries it, i.e. as strings. */
export interface AllocationRow {
  channel: string;
  cap: string;
  releaseAt: string;
  opensAt: string;
  closesAt: string;
  /**
   * The literal string 'true' or 'false' — never a checkbox's presence.
   *
   * Only load-bearing for a row inventory does not already hold; for an existing row the
   * server's value wins (ai-review pass 2). Kept because the parser must still read the
   * field by VALUE rather than by key presence — a hidden input always submits.
   */
  requiresCode: string;
  soldBy: string;
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
      opensAt: at('opensAt'),
      closesAt: at('closesAt'),
      // The VALUE decides, not the key's presence — see the header.
      requiresCode: at('requiresCode') === 'true' ? 'true' : 'false',
      soldBy: at('soldBy'),
    });
  }
  return rows;
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
 * Build the full-set replace body.
 *
 * `current` is the allocation set as inventory reports it RIGHT NOW. It supplies every
 * field the form does not render — the sales window, the presale gate and the seller
 * binding — so those survive a save without ever round-tripping through the client. The
 * form contributes only what an operator can actually see and change: the channel, its
 * cap, and its release time.
 */
export function toAllocationRequest(
  organizerId: string,
  rows: AllocationRow[],
  current: CurrentAllocation[] = [],
): ChannelAllocationSet {
  const byChannel = new Map(current.map((c) => [c.channel, c]));
  return {
    organizer_id: organizerId,
    allocations: rows.map((r) => {
      const held = byChannel.get(r.channel);
      const a: ChannelAllocation = {
        channel: r.channel,
        cap: Number(r.cap),
        // Always explicit. `false` is meaningful — it is what un-gates a channel — so it
        // must not be dropped by an `if (truthy)` the way the optional fields are. Taken
        // from the server for an existing row: this screen does not edit it, so the form's
        // copy can only be stale or forged.
        requires_code: held ? Boolean(held.requires_code) : r.requiresCode === 'true',
      };
      // The optional fields are OMITTED when unset rather than sent empty: the contract
      // types them as date-time/uuid, and "" fails request validation.
      const release = preservedInstant(r.releaseAt, held?.release_at);
      if (release) a.release_at = release;
      // Never rendered as an input, so they come from the server unchanged or not at all.
      if (held?.opens_at) a.opens_at = held.opens_at;
      if (held?.closes_at) a.closes_at = held.closes_at;
      const soldBy = held ? held.sold_by : r.soldBy || undefined;
      if (soldBy) a.sold_by = soldBy;
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
    default:
      // An unrecognised code still has to reach the operator verbatim.
      errors.form = message;
      return errors;
  }
}
