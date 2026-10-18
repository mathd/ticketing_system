// Captured scan fixtures. Each call returns a new id and distinct sub-second times.

import type { CapturedOccurrence } from '../occurrence-coordinator'

let sequence = 0
export function captured(overrides: Partial<CapturedOccurrence> = {}): CapturedOccurrence {
  sequence += 1
  return {
    occurrenceId: crypto.randomUUID(),
    qrPayload: `qr-${sequence}`,
    occurredAt: `2026-10-18T09:00:${String(sequence % 60).padStart(2, '0')}.123Z`,
    createdAt: `2026-10-18T09:00:${String(sequence % 60).padStart(2, '0')}.124Z`,
    type: 'normal',
    ...overrides,
  }
}
