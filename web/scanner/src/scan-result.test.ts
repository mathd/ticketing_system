import { expect, it, vi } from 'vitest'
import { ACCEPT_INSTRUCTION, OFFLINE_INSTRUCTION, presentResult } from './scan-result'
import type { EntryResult } from './entry-scan'

it('fixes the admission instructions independently of the renderer', () => {
  expect(ACCEPT_INSTRUCTION).toBe('Entry accepted. Admit.')
  expect(OFFLINE_INSTRUCTION).toBe('Admit per venue offline policy.')
})
const cases: Array<[EntryResult, string | undefined, string[]]> = [
  [{ kind: 'stopped-before-send' }, 'Scan stopped', ['Scan took too long or its clock could not be checked. No request was sent. Do not admit from this result. Scan again.']],
  [{ kind: 'sent-unconfirmed' }, 'Operator required', ['Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.']],
  [{ kind: 'accepted-too-late' }, 'Operator required', ['Access recorded this scan, but the scanner could not finish in time. The scanner did not open. Get an operator; do not admit from this result.']],
  [{ kind: 'revocation-audit-not-saved' }, 'Not valid for entry', ["This ticket is on the device's revocation list. Do not admit. The refusal audit was not saved. Get an operator."]],
  [{ kind: 'duplicate' }, 'Already processed', ['This scan was already handled on this device. No second entry.']],
  [{ kind: 'mark-failed' }, 'Operator required', ['Access recorded this scan; the scanner did not open; get an operator.']],
  [{ kind: 'auth-refused' }, 'Pair this device', ['This attempt was not authorized. Do not admit from this result.', 'Pair this device and scan the ticket again.']],
  [{ kind: 'denial-unreadable' }, 'Rejected', ['Access refused this scan; reason unavailable. Do not admit']],
  [{ kind: 'queued' }, 'Offline decision', ['No response was received.', 'Admit per venue offline policy.']],
  [{ kind: 'queued', status: 503 }, 'Offline decision', ['Server response 503.', 'Admit per venue offline policy.']],
  [{ kind: 'storage-unready' }, undefined, ['Scanner storage is not ready. No ticket was checked.']],
  [{ kind: 'mint-unconfirmed' }, undefined, ['Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.']],
  [{ kind: 'cancelled', requestMayHaveBeenSent: false }, 'Scan cancelled', ['Scan cancelled. No request was sent. The scanner did not open. Do not admit from this result.']],
  [{ kind: 'cancelled', requestMayHaveBeenSent: true }, 'Scan cancelled', ['Scan cancelled. Access may have recorded this scan. The scanner did not open. Do not admit from this result.']],
  [{ kind: 'body-unconfirmed' }, 'Operator required', ['Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.']],
  [{ kind: 'status-unconfirmed' }, 'Operator required', ['Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.']],
  [{ kind: 'server-error' }, 'Operator required', ['Outcome unknown. Do not admit. Contact an operator.']],
  [{ kind: 'local-failure' }, 'Operator required', ['Local scanner failure. Scanner did not open. Do not admit from this result; get an operator.']],
  [{ kind: 'revocation-refused' }, 'Not valid for entry', ["This ticket is on the device's revocation list. Do not admit."]],
  [{ kind: 'rejected', reason: 'already_redeemed' }, 'Rejected', ['Already redeemed.', 'Do not admit. Get an operator.', 'Reason: already_redeemed.']],
]
it.each(cases)('renders the complete %j result', (result, heading, lines) => {
  expect(presentResult(result, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({ heading, lines })
})
const entryDenials: Array<[string, string[]]> = [
  [" Future_Reason ", ["Do not admit. Access refused this scan. Get an operator.", "Reason:  Future_Reason ."]],
  ["already_redeemed", ["Already redeemed.", "Do not admit. Get an operator.", "Reason: already_redeemed."]],
  ["entry_limit_reached", ["Do not admit. No entries remain on this pass.", "Reason: entry_limit_reached."]],
  ["exit_required", ["Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.", "Reason: exit_required."]],
  ["not_inside", ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: not_inside."]],
  ["exit_not_applicable", ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_not_applicable."]],
  ["invalid_credential", ["Do not admit. Access did not accept this credential. Get an operator.", "Reason: invalid_credential."]],
  ["revoked", ["Do not admit. Access refused this scan. Get an operator.", "Reason: revoked."]],
  ["refunded", ["Do not admit. This ticket was voided by a refund. Get an operator.", "Reason: refunded."]],
  ["exchanged", ["Do not admit. This ticket was replaced by an exchange. Ask for the replacement ticket and scan it.", "Reason: exchanged."]],
  ["exit_unverified", ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_unverified."]],
  ["integrity_quarantined", ["Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.", "Reason: integrity_quarantined."]],
  ["integrity_operator_controlled", ["Do not admit. Access refused this ticket under operator-controlled integrity handling. Get an operator.", "Reason: integrity_operator_controlled."]],
  ["occurrence_required", ["Do not admit. This scan is missing its required occurrence ID. Get an operator to check the scanner.", "Reason: occurrence_required."]],
  ["unknown_future_reason", ["Do not admit. Access refused this scan. Get an operator.", "Reason: unknown_future_reason."]],
  ["invalid_occurrence", ["Do not admit. This scan has invalid occurrence data. Get an operator to check the scanner.", "Reason: invalid_occurrence."]],
  ["occurrence_collision", ["Do not admit. This scan's occurrence ID conflicts with an existing record. Get an operator.", "Reason: occurrence_collision."]],
  ["future_reason", ["Do not admit. Access refused this scan. Get an operator.", "Reason: future_reason."]],
  ["__proto__", ["Do not admit. Access refused this scan. Get an operator.", "Reason: __proto__."]],
  ["<b>future_reason</b>", ["Do not admit. Access refused this scan. Get an operator.", "Reason: <b>future_reason</b>."]],
]
it.each(entryDenials)('TKT-565 complete Entry denial %s', (reason, lines) => {
  expect(presentResult({ kind: 'rejected', reason }, { direction: 'entry', requestMayHaveBeenSent: true })).toEqual({ heading: 'Rejected', tone: 'rejected', lines })
})
it.each(entryDenials.filter(([reason]) => reason !== 'already_redeemed'))('TKT-565 ignores supplied admission time for Entry %s', (reason, lines) => {
  expect(presentResult({ kind: 'rejected', reason, originalScanAt: '2026-10-18T09:00:01.456Z' },
    { direction: 'entry', requestMayHaveBeenSent: true })).toEqual({ heading: 'Rejected', tone: 'rejected', lines })
})
it('uses validated server times without replacing them with current device time', () => {
  const scannedAt = '2026-10-18T09:00:01.456Z'
  const localized = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date(scannedAt))
  expect(presentResult({ kind: 'accepted', scannedAt, replay: true }, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({ heading: 'Accepted', lines: [
    'Entry accepted. Admit.', `Entry recorded at ${localized}.`, 'This scan was previously recorded. Entry counted once.',
  ] })
  expect(presentResult({ kind: 'rejected', reason: 'already_redeemed', originalScanAt: scannedAt }, { direction: 'entry', requestMayHaveBeenSent: true })).toEqual({
    heading: 'Rejected', tone: 'rejected', lines: [`Already redeemed at ${localized}.`, 'Do not admit. Get an operator.', 'Reason: already_redeemed.'],
  })
})

it('TKT-565 never substitutes the device date for an absent Access time', () => {
  const NativeDate = Date
  vi.stubGlobal('Date', class extends NativeDate {
    constructor(value?: string | number) { super(value ?? '2035-02-03T04:05:06.789Z') }
  })
  try {
    expect(new Date().toISOString()).toBe('2035-02-03T04:05:06.789Z')
    expect(presentResult({ kind: 'rejected', reason: 'already_redeemed' },
      { direction: 'entry', requestMayHaveBeenSent: true })).toEqual({ heading: 'Rejected', tone: 'rejected', lines: [
      'Already redeemed.', 'Do not admit. Get an operator.', 'Reason: already_redeemed.',
    ] })
    const originalScanAt = '2026-10-18T09:00:01.456Z'
    const time = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new NativeDate(originalScanAt))
    expect(presentResult({ kind: 'rejected', reason: 'already_redeemed', originalScanAt },
      { direction: 'entry', requestMayHaveBeenSent: true })).toEqual({ heading: 'Rejected', tone: 'rejected', lines: [
      `Already redeemed at ${time}.`, 'Do not admit. Get an operator.', 'Reason: already_redeemed.',
    ] })
  } finally { vi.unstubAllGlobals() }
})
