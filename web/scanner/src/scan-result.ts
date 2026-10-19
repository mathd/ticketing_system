import type { EntryResult } from './entry-scan'

export const ACCEPT_INSTRUCTION = 'Entry accepted. Admit.'
export const OFFLINE_INSTRUCTION = 'Admit per venue offline policy.'
export const STORAGE_UNREADY = 'Scanner storage is not ready. No ticket was checked.'
export const STORAGE_UNAVAILABLE = 'Storage is unavailable or incompatible. Keep this page open and retry storage.'
export const REPAIR_NEEDED = 'The outcome write needs repair. Keep this page open and retry storage.'
export const CANNOT_REPAIR = 'The saved scan could not be repaired. Get an operator.'

export type ResultPresentation = { heading: string | undefined; lines: string[]; tone: 'accepted' | 'queued' | 'rejected' }
export const readableTime = (value: string) => new Date(value).toLocaleString()
const unconfirmed = 'Scan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.'

/** Core Entry instructions. Ordinary reason-specific copy remains in TKT-552. */
export function presentResult(result: EntryResult): ResultPresentation {
  const refused = (heading: string | undefined, ...lines: string[]): ResultPresentation => ({ heading, lines, tone: 'rejected' })
  switch (result.kind) {
    case 'accepted': return { heading: 'Accepted', tone: 'accepted', lines: [ACCEPT_INSTRUCTION,
      `Entry recorded at ${readableTime(result.scannedAt)}.`,
      ...(result.replay ? ['This scan was previously recorded. Entry counted once.'] : [])] }
    case 'rejected': return refused('Rejected', result.reason === 'already_redeemed'
      ? result.originalScanAt ? `Already redeemed at ${readableTime(result.originalScanAt)}.` : 'Already redeemed.'
      : 'Credential is invalid or cannot be redeemed.')
    case 'queued': return { heading: 'Offline decision', tone: 'queued', lines: [
      result.status === undefined ? 'No response was received.' : `Server response ${result.status}.`, OFFLINE_INSTRUCTION] }
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
    case 'revocation-refused': return refused('Not valid for entry', "This ticket is on the device's revocation list. Do not admit.")
    case 'storage-unready': return refused(undefined, STORAGE_UNREADY)
    case 'mint-unconfirmed': return refused(undefined, 'Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.')
  }
}
