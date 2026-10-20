import type { EntryResult, ResultContext } from './entry-scan'

export const ACCEPT_INSTRUCTION = 'Entry accepted. Admit.'
export const OFFLINE_INSTRUCTION = 'Admit per venue offline policy.'
export const STORAGE_UNREADY = 'Scanner storage is not ready. No ticket was checked.'
export const STORAGE_UNAVAILABLE = 'Storage is unavailable or incompatible. Keep this page open and retry storage.'
export const REPAIR_NEEDED = 'The outcome write needs repair. Keep this page open and retry storage.'
export const CANNOT_REPAIR = 'The saved scan could not be repaired. Get an operator.'

export type ResultPresentation = { heading: string | undefined; lines: string[]; tone: 'accepted' | 'queued' | 'rejected' | 'neutral' }
export const readableTime = (value: string) => new Date(value).toLocaleString()
const unconfirmed = 'Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.'

/** Presentation uses the submission context, independent of the current controls. */
export function presentResult(result: EntryResult, context: ResultContext): ResultPresentation {
  if (context.direction === 'exit') return presentExit(result, context)
  return presentEntry(result)
}

/**
 * Core Entry instructions. exit_required and entry_limit_reached have their own copy (TKT-552).
 * already_redeemed keeps its own text. Every other readable reason, including an unknown one, gets
 * the generic fallback. The fallback is open-ended; TKT-565 tracks the remaining reason copy.
 */
function presentEntry(result: EntryResult): ResultPresentation {
  const refused = (heading: string | undefined, ...lines: string[]): ResultPresentation => ({ heading, lines, tone: 'rejected' })
  switch (result.kind) {
    case 'accepted': return { heading: 'Accepted', tone: 'accepted', lines: [ACCEPT_INSTRUCTION,
      `Entry recorded at ${readableTime(result.scannedAt)}.`,
      ...(result.replay ? ['This scan was previously recorded. Entry counted once.'] : [])] }
    case 'rejected':
      if (result.reason === 'exit_required') return refused('Rejected',
        'Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.',
        'Reason: exit_required.')
      if (result.reason === 'entry_limit_reached') return refused('Rejected', 'Do not admit. No entries remain on this pass.', 'Reason: entry_limit_reached.')
      return refused('Rejected', result.reason === 'already_redeemed'
      ? result.originalScanAt ? `Already redeemed at ${readableTime(result.originalScanAt)}.` : 'Already redeemed.'
      : 'Credential is invalid or cannot be redeemed.')
    case 'queued': return { heading: 'Offline decision', tone: 'queued', lines: [
      result.status === undefined ? 'No response was received.' : `Server response ${result.status}.`, OFFLINE_INSTRUCTION] }
    case 'stopped-before-send': return refused('Scan stopped', 'Scan took too long or its clock could not be checked. No request was sent. Do not admit from this result. Scan again.')
    case 'sent-unconfirmed': return refused('Operator required', unconfirmed)
    case 'accepted-too-late': return refused('Operator required', 'Access recorded this scan, but the scanner could not finish in time. The scanner did not open. Get an operator; do not admit from this result.')
    case 'revocation-audit-not-saved': return refused('Not valid for entry', "This ticket is on the device's revocation list. Do not admit. The refusal audit was not saved. Get an operator.")
    case 'duplicate': return refused('Already processed', 'This scan was already handled on this device. No second entry.')
    case 'mark-failed': return refused('Operator required', 'Access recorded this scan; the scanner did not open; get an operator.')
    case 'auth-refused': return refused('Pair this device', 'This attempt was not authorized. Do not admit from this result.', 'Pair this device and scan the ticket again.')
    case 'denial-unreadable': return refused('Rejected', 'Access refused this scan; reason unavailable. Do not admit')
    case 'server-error': return refused('Operator required', 'Outcome unknown. Do not admit. Contact an operator.')
    case 'status-unconfirmed':
    case 'body-unconfirmed': return refused('Operator required', unconfirmed)
    case 'local-failure': return refused('Operator required', 'Local scanner failure. Scanner did not open. Do not admit from this result; get an operator.')
    case 'cancelled': return refused('Scan cancelled', result.requestMayHaveBeenSent
      ? 'Scan cancelled. Access may have recorded this scan. The scanner did not open. Do not admit from this result.'
      : 'Scan cancelled. No request was sent. The scanner did not open. Do not admit from this result.')
    case 'revocation-exit':
    case 'revocation-refused': return refused('Not valid for entry', "This ticket is on the device's revocation list. Do not admit.")
    case 'storage-unready': return refused(undefined, STORAGE_UNREADY)
    case 'mint-unconfirmed': return refused(undefined, 'Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.')
  }
}

function presentExit(result: EntryResult, context: ResultContext): ResultPresentation {
  const neutral = (heading: string | undefined, ...lines: string[]): ResultPresentation => ({ heading, lines, tone: 'neutral' })
  const unknown = 'Exit result unknown; the holder may leave'
  const notRecorded = 'Exit not recorded; the holder may leave'
  switch (result.kind) {
    case 'accepted': return neutral('Exit recorded', 'Exit recorded. The holder may leave.',
      `Exit recorded at ${readableTime(result.scannedAt)}.`,
      ...(result.replay ? ['This scan was previously recorded. Exit counted once.'] : []))
    case 'rejected': return neutral('Exit not recorded',
      `Exit not recorded; reason: ${result.reason === 'invalid_credential' ? 'credential not accepted' : result.reason}. The holder may leave.`,
      ...(['revoked', 'refunded', 'exchanged'].includes(result.reason) ? ['This ticket is no longer valid for entry.'] : []),
      ...(result.reason !== 'already_redeemed' || result.originalScanAt === undefined ? [] : [`Already redeemed at ${readableTime(result.originalScanAt)}.`]))
    case 'denial-unreadable': return neutral('Exit not recorded', 'Exit not recorded; reason unavailable. The holder may leave')
    case 'queued': return neutral('Offline Exit',
      result.status === undefined ? 'No response was received.' : `Server response ${result.status}.`, unknown)
    case 'duplicate': return neutral('Already processed', 'This scan was already handled on this device. No second Exit action. The holder may leave.')
    case 'accepted-too-late': return neutral('Operator required',
      'Access recorded this Exit, but the scanner could not finish in time. The scanner did not open. Get an operator; do not admit from this result.', 'The holder may leave.')
    case 'mark-failed': return neutral('Operator required', 'Access recorded this scan; the scanner did not open; get an operator.', 'The holder may leave.')
    case 'auth-refused': return neutral('Pair this device', 'This attempt was not authorized. Do not admit from this result.',
      'Pair this device and scan the ticket again.', 'The holder may leave.')
    case 'sent-unconfirmed': return neutral('Operator required',
      'Scan sent, answer unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.', unknown)
    case 'status-unconfirmed':
    case 'body-unconfirmed': return neutral('Operator required', unconfirmed, unknown)
    case 'server-error': return neutral('Operator required', unknown)
    case 'local-failure': return neutral('Operator required',
      'Local scanner failure. Scanner did not open. Do not admit from this result; get an operator.',
      context.requestMayHaveBeenSent ? unknown : notRecorded)
    case 'cancelled': return neutral('Scan cancelled', context.requestMayHaveBeenSent
      ? 'Scan cancelled. Access may have recorded this scan. The scanner did not open. Do not admit from this result.'
      : 'Scan cancelled. No request was sent. The scanner did not open. Do not admit from this result.',
      context.requestMayHaveBeenSent ? unknown : notRecorded)
    case 'stopped-before-send': return neutral('Scan stopped',
      'Scan took too long or its clock could not be checked. No request was sent. Do not admit from this result. Scan again.', notRecorded)
    case 'storage-unready': return neutral('Exit not recorded', STORAGE_UNREADY, notRecorded)
    case 'mint-unconfirmed': return neutral('Exit not recorded',
      'Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.', notRecorded)
    case 'revocation-exit':
    case 'revocation-refused':
    case 'revocation-audit-not-saved': return neutral('Exit not recorded',
      'Exit not recorded. This ticket is no longer valid for entry. The holder may leave.')
  }
}
