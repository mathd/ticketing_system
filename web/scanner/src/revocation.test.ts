import { describe, expect, it } from 'vitest'
import { decodeTicketID } from './revocation'

const ticketID = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
const encode = (value: string) => btoa(value).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')
const credential = (claims: string) => `header.${encode(claims)}.signature`

describe('ticket id decoder', () => {
  it('reads a UUID tid from the claims part', () => {
    expect(decodeTicketID(credential(JSON.stringify({ tid: ticketID })))).toBe(ticketID)
  })

  it.each([
    ['syntax', 'header.!.signature'],
    ['JSON type', credential('{"tid":4}')],
    ['missing tid', credential('{}')],
    ['non-UUID tid', credential('{"tid":"ticket-1"}')],
  ])('returns no id for %s without throwing', (_name, value) => {
    expect(() => decodeTicketID(value)).not.toThrow()
    expect(decodeTicketID(value)).toBeUndefined()
  })
})
