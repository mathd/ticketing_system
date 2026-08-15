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
  /** The literal string 'true' or 'false' — never a checkbox's presence. */
  requiresCode: string;
  soldBy: string;
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
    rows.push({
      channel: at('channel'),
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

/** Build the full-set replace body. Every field rides, including the unrendered ones. */
export function toAllocationRequest(
  organizerId: string,
  rows: AllocationRow[],
): ChannelAllocationSet {
  return {
    organizer_id: organizerId,
    allocations: rows.map((r) => {
      const a: ChannelAllocation = {
        channel: r.channel,
        cap: Number(r.cap),
        // Always explicit. `false` is meaningful — it is what un-gates a channel — so
        // it must not be dropped by an `if (truthy)` the way the optional fields are.
        requires_code: r.requiresCode === 'true',
      };
      // The optional fields are OMITTED when unset rather than sent empty: the contract
      // types them as date-time/uuid, and "" fails request validation.
      const release = instant(r.releaseAt);
      const opens = instant(r.opensAt);
      const closes = instant(r.closesAt);
      if (release) a.release_at = release;
      if (opens) a.opens_at = opens;
      if (closes) a.closes_at = closes;
      if (r.soldBy) a.sold_by = r.soldBy;
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
