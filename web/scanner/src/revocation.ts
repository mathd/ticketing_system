const ticketIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i

// The decoded tid is unverified. Use it only to deny; never use it to admit.
export function decodeTicketID(payload: string): string | undefined {
  try {
    if (payload.length > 16_384) return undefined
    const parts = payload.split('.')
    if (parts.length !== 3 || !parts[1] || !/^[A-Za-z0-9_-]+$/.test(parts[1])) return undefined
    const base64 = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const claims = JSON.parse(atob(base64.padEnd(Math.ceil(base64.length / 4) * 4, '='))) as unknown
    if (!claims || typeof claims !== 'object' || !('tid' in claims)) return undefined
    const ticketID = (claims as { tid?: unknown }).tid
    return typeof ticketID === 'string' && ticketIDPattern.test(ticketID) ? ticketID.toLowerCase() : undefined
  } catch {
    return undefined
  }
}
