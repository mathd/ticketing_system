import { describe, expect, it } from 'vitest'
import {
  decodeAccessError,
  decodeReconcileResponse,
  decodeScanResponse,
} from './access-contract'

const OCCURRENCE = '6efae2f2-cd2d-4b9d-96cb-ec4ee048dc78'
const OTHER_OCCURRENCE = '9552ce84-c3aa-4a67-b7e6-65ef783c1c95'

describe('Access response contracts', () => {
  it('decodes the generated accepted and rejected scan variants', () => {
    expect(decodeScanResponse(200, {
      decision: 'accepted',
      scanned_at: '2026-09-03T20:00:01.123Z',
      replay: true,
    })).toEqual({
      decision: 'accepted',
      scanned_at: '2026-09-03T20:00:01.123Z',
      replay: true,
    })
    expect(decodeScanResponse(409, {
      decision: 'rejected',
      reason: 'already_redeemed',
      original_scan_at: '2026-09-03T16:00:01-04:00',
    })).toEqual({
      decision: 'rejected',
      reason: 'already_redeemed',
      original_scan_at: '2026-09-03T16:00:01-04:00',
    })
  })

  it.each([
    ['an acceptance without its decision', 200, { scanned_at: '2026-09-03T20:00:01Z' }],
    ['the wrong decision for the status', 200, { decision: 'rejected', scanned_at: '2026-09-03T20:00:01Z' }],
    ['an acceptance without its time', 200, { decision: 'accepted' }],
    ['an impossible calendar date', 200, { decision: 'accepted', scanned_at: '2026-02-30T20:00:01Z' }],
    ['a rejection without its reason', 422, { decision: 'rejected' }],
    ['an error status passed off as a scan result', 500, { decision: 'rejected', reason: 'failed' }],
  ])('refuses %s', (_name, status, body) => {
    expect(() => decodeScanResponse(status, body)).toThrow()
  })

  it('requires the generated error body used by the pairing path', () => {
    expect(decodeAccessError({ error: 'scanner device is not enrolled' })).toEqual({
      error: 'scanner device is not enrolled',
    })
    expect(() => decodeAccessError({})).toThrow()
  })

  it('decodes reconciliation results for occurrences in the submitted queue', () => {
    expect(decodeReconcileResponse({
      results: [{
        occurrence_id: OCCURRENCE,
        result: 'conflict',
        occurred_at: '2026-09-03t20:00:01z',
        skew_flagged: true,
      }],
    }, new Set([OCCURRENCE]))).toEqual({
      results: [{
        occurrence_id: OCCURRENCE,
        result: 'conflict',
        occurred_at: '2026-09-03t20:00:01z',
        skew_flagged: true,
      }],
    })
  })

  it.each([
    ['a missing results list', {}, new Set([OCCURRENCE])],
    ['an unknown result', { results: [{ occurrence_id: OCCURRENCE, result: 'done' }] }, new Set([OCCURRENCE])],
    ['a foreign occurrence', { results: [{ occurrence_id: OTHER_OCCURRENCE, result: 'synced' }] }, new Set([OCCURRENCE])],
    ['a repeated occurrence', { results: [
      { occurrence_id: OCCURRENCE, result: 'recorded' },
      { occurrence_id: OCCURRENCE, result: 'synced' },
    ] }, new Set([OCCURRENCE])],
    ['an omitted occurrence', { results: [
      { occurrence_id: OCCURRENCE, result: 'recorded' },
    ] }, new Set([OCCURRENCE, OTHER_OCCURRENCE])],
    ['an invalid optional time', { results: [{
      occurrence_id: OCCURRENCE,
      result: 'recorded',
      occurred_at: 'yesterday',
    }] }, new Set([OCCURRENCE])],
  ])('refuses reconciliation with %s', (_name, body, expected) => {
    expect(() => decodeReconcileResponse(body, expected)).toThrow()
  })
})
