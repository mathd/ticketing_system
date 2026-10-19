/** Only a rejection of the native fetch promise can supply no-response evidence. */
export type ScanTransport =
  | { kind: 'response'; response: Response }
  | { kind: 'no-response' }
  | { kind: 'cancelled' }

export async function sendScan(request: Request): Promise<ScanTransport> {
  // Construction and synchronous fetch throws stay outside this classifier.
  const pending = fetch(request)
  try {
    return { kind: 'response', response: await pending }
  } catch (error) {
    if (request.signal.aborted) return { kind: 'cancelled' }
    if (error instanceof TypeError && error.name === 'TypeError'
      && error.constructor === TypeError && error.message === 'Failed to fetch') return { kind: 'no-response' }
    throw error
  }
}
