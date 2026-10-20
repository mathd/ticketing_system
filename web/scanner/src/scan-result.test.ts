import { expect, it } from 'vitest'
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
  [{ kind: 'rejected', reason: 'already_redeemed' }, 'Rejected', ['Already redeemed.']],
]
it.each(cases)('renders the complete %j result', (result, heading, lines) => {
  expect(presentResult(result, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({ heading, lines })
})
it.each(['entry_limit_reached', 'exit_required', 'not_inside', 'exit_not_applicable', 'invalid_credential',
  'revoked', 'refunded', 'exchanged', 'exit_unverified', 'integrity_quarantined', 'integrity_operator_controlled',
  'occurrence_required', 'unknown_future_reason', 'invalid_occurrence', 'occurrence_collision'])('keeps the scoped generic Entry copy for %s', (reason) => {
  expect(presentResult({ kind: 'rejected', reason }, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({ heading: 'Rejected', lines: ['Credential is invalid or cannot be redeemed.'] })
})
it('uses validated server times without replacing them with current device time', () => {
  const scannedAt = '2026-10-18T09:00:01.456Z'
  const localized = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date(scannedAt))
  expect(presentResult({ kind: 'accepted', scannedAt, replay: true }, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({ heading: 'Accepted', lines: [
    'Entry accepted. Admit.', `Entry recorded at ${localized}.`, 'This scan was previously recorded. Entry counted once.',
  ] })
  expect(presentResult({ kind: 'rejected', reason: 'already_redeemed', originalScanAt: scannedAt }, { direction: 'entry', requestMayHaveBeenSent: true })).toMatchObject({
    heading: 'Rejected', lines: [`Already redeemed at ${localized}.`],
  })
})
