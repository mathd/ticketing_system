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
