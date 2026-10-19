import { afterEach, expect, it, vi } from 'vitest'
import oldSource from './test/fixtures/occurrence-model.tkt558.ts?raw'
import * as model from './occurrence-model'
import { readCurrentRecord as oldReader } from './test/fixtures/occurrence-model.tkt558'
import { captured, createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { rawGet } from './test/raw-db'
import { STORE } from './occurrence-store'

afterEach(() => { vi.restoreAllMocks(); destroyAllPages() })
// Frozen from bbc919719310fb1133218389ede5def431f8bece, before TKT-559 edits.
it('pins the full TKT-558 decoder source', async () => {
  const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(oldSource))
  expect(Array.from(new Uint8Array(hash), byte => byte.toString(16).padStart(2, '0')).join('')).toBe('b3139bf9a56e3cf7a44b2f2d8b0d0ffd88fe1c97e1dc8a1875076afdedf9c85a')
})

it.each(['normal', 'refusal'] as const)('writes a stopped %s through the real store and warns in the old reader queue', async (type) => {
  const dbName = `stopped-${crypto.randomUUID()}`
  const page = createTestPage({ dbName }); await page.ready()
  const scan = captured({ type }); page.coordinator.register(scan)
  if (type === 'refusal') await page.store().mintRefusal(scan.occurrenceId)
  else await page.store().mintNormal(scan.occurrenceId)
  page.coordinator.latch({ kind: 'terminal', occurrenceId: scan.occurrenceId, decidedAt: '2026-10-18T12:00:00Z',
    outcome: { family: 'terminal', cause: 'stopped', checkpoint: 'send', reason: 'deadline', requestMayHaveBeenSent: false } })
  expect((await page.coordinator.persist(scan.occurrenceId)).durable).toBe('committed')
  const row = await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>
  expect(row).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'stopped' } })
  expect(model.readCurrentRecord(row).ok).toBe(true)
  const outcome = row.localOutcome as Record<string, unknown>
  for (const change of [{ checkpoint: 'bad' }, { reason: 'bad' }, { requestMayHaveBeenSent: 'false' },
    { checkpoint: 'send', requestMayHaveBeenSent: true }, ...(type === 'refusal' ? [{ checkpoint: 'decision' }, { cause: 'local_failure' }] : [])]) {
    expect(model.readCurrentRecord({ ...row, localOutcome: { ...outcome, ...change } }).ok).toBe(false)
  }
  const control = await mintOn(page)
  page.coordinator.latch({ kind: 'queue', occurrenceId: control.scan.occurrenceId, decidedAt: '2026-10-18T12:00:00Z',
    outcome: { family: 'queue', cause: 'transport_failure' } })
  await page.coordinator.persist(control.scan.occurrenceId)
  const oldRead = oldReader(row)
  expect(oldRead).toMatchObject({ ok: false })
  if (oldRead.ok) throw new Error('old decoder accepted a new stop')
  vi.spyOn(model, 'readCurrentRecord').mockImplementation(oldReader)
  // The queue algorithm is unchanged from TKT-558. Use its actual old decoder
  // at that production boundary, with a real eligible row proving the read ran.
  const queued = await page.store().queued()
  expect(queued.entries.map((entry) => entry.key)).toEqual([control.scan.occurrenceId])
  expect(queued.warnings).toEqual([{ key: scan.occurrenceId, problem: oldRead.problem }])
})
