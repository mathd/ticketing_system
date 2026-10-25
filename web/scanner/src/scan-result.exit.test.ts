import { expect, it } from 'vitest'
import { presentResult } from './scan-result'

it.each([false, true])('keeps the full cancelled Exit presentation for sent=%s', sent => {
  expect(presentResult({ kind: 'cancelled', requestMayHaveBeenSent: sent },
    { direction: 'exit', requestMayHaveBeenSent: sent })).toEqual({
    heading: 'Scan cancelled', tone: 'neutral', lines: [sent
      ? 'Scan cancelled. Access may have recorded this scan. The scanner did not open. Do not admit from this result.'
      : 'Scan cancelled. No request was sent. The scanner did not open. Do not admit from this result.',
    sent ? 'Exit result unknown; the holder may leave' : 'Exit not recorded; the holder may leave'],
  })
})

const exitDenials: Array<[string, string[]]> = [
  [" Future_Reason ", ["Exit not recorded; reason:  Future_Reason . The holder may leave."]],
  ["already_redeemed", ["Exit not recorded; reason: already_redeemed. The holder may leave."]],
  ["entry_limit_reached", ["Exit not recorded; reason: entry_limit_reached. The holder may leave."]],
  ["exit_required", ["Exit not recorded; reason: exit_required. The holder may leave."]],
  ["not_inside", ["Exit not recorded; reason: not_inside. The holder may leave."]],
  ["exit_not_applicable", ["Exit not recorded; reason: exit_not_applicable. The holder may leave."]],
  ["invalid_credential", ["Exit not recorded; reason: credential not accepted. The holder may leave."]],
  ["revoked", ["Exit not recorded; reason: revoked. The holder may leave.", "This ticket is no longer valid for entry."]],
  ["refunded", ["Exit not recorded; reason: refunded. The holder may leave.", "This ticket is no longer valid for entry."]],
  ["exchanged", ["Exit not recorded; reason: exchanged. The holder may leave.", "This ticket is no longer valid for entry."]],
  ["exit_unverified", ["Exit not recorded; reason: exit_unverified. The holder may leave."]],
  ["integrity_quarantined", ["Exit not recorded; reason: integrity_quarantined. The holder may leave."]],
  ["integrity_operator_controlled", ["Exit not recorded; reason: integrity_operator_controlled. The holder may leave."]],
  ["occurrence_required", ["Exit not recorded; reason: occurrence_required. The holder may leave."]],
  ["unknown_future_reason", ["Exit not recorded; reason: unknown_future_reason. The holder may leave."]],
  ["invalid_occurrence", ["Exit not recorded; reason: invalid_occurrence. The holder may leave."]],
  ["occurrence_collision", ["Exit not recorded; reason: occurrence_collision. The holder may leave."]],
  ["future_reason", ["Exit not recorded; reason: future_reason. The holder may leave."]],
  ["__proto__", ["Exit not recorded; reason: __proto__. The holder may leave."]],
  ["<b>future_reason</b>", ["Exit not recorded; reason: <b>future_reason</b>. The holder may leave."]],
 ]
it.each(exitDenials)('TKT-565 unchanged complete Exit denial %s', (reason, lines) => {
  for (const originalScanAt of [undefined, '2026-10-18T09:00:01.456Z']) {
    const time = originalScanAt === undefined ? undefined : new Intl.DateTimeFormat(undefined, {
      year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric',
    }).format(new Date(originalScanAt))
    expect(presentResult({ kind: 'rejected', reason, ...(originalScanAt === undefined ? {} : { originalScanAt }) },
      { direction: 'exit', requestMayHaveBeenSent: true })).toEqual({ heading: 'Exit not recorded', tone: 'neutral',
      lines: reason === 'already_redeemed' && time !== undefined ? [...lines, `Already redeemed at ${time}.`] : lines,
    })
  }
})
