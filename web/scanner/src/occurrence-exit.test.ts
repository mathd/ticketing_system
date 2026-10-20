import { afterEach, expect, it } from 'vitest'
import { readCurrentRecord } from './occurrence-model'
import { STORE } from './occurrence-store'
import { planReconcileBatches } from './reconcile-batch'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { rawGet } from './test/raw-db'

afterEach(destroyAllPages)

it.each(['entry', 'exit'] as const)('keeps captured %s through queue listing and exact retirement', async (direction) => {
  const dbName = `direction-${crypto.randomUUID()}`
  const page = createTestPage({ dbName })
  const store = await page.ready()
  const { scan, record } = await mintOn(page, { direction })
  expect(record).toStrictEqual({ ...record, direction })
  expect(record).toMatchObject({ occurrenceId: scan.occurrenceId, qrPayload: scan.qrPayload,
    occurredAt: scan.occurredAt, createdAt: scan.createdAt, state: 'PENDING', direction })
  const decidedAt = '2026-10-19T12:00:01.456Z'
  page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId,
    outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt })
  await page.coordinator.persist(scan.occurrenceId)
  const queued = { ...record, direction, state: 'QUEUED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt }
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(queued)
  const plan = planReconcileBatches((await store.queued()).entries)
  expect(plan.batches.flatMap((batch) => batch.occurrences)).toStrictEqual([
    { occurrence_id: scan.occurrenceId, qr_payload: scan.qrPayload, occurred_at: scan.occurredAt, event_type: direction },
  ])
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(queued)
  await store.applyReconcileResults([scan.occurrenceId], [{ occurrence_id: scan.occurrenceId, result: 'recorded' }])
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual({ ...queued, state: 'SYNCED', result: 'recorded' })
  expect((await store.queued()).entries).toEqual([])
})

it('keeps Exit and the absent decision time through actual page-loss recovery and a second page notice', async () => {
  const dbName = `exit-recovery-${crypto.randomUUID()}`
  let now = 1_000
  const a = createTestPage({ dbName, now: () => now })
  await a.ready()
  const { scan, record } = await mintOn(a, { direction: 'exit' })
  a.destroy()
  now = 40_000
  const b = createTestPage({ dbName, now: () => now })
  const store = await b.ready()
  const recovered = { ...record, direction: 'exit', state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } }
  expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(recovered)
  const notice = { occurrenceId: scan.occurrenceId, occurredAt: scan.occurredAt, type: 'normal', direction: 'exit', cause: 'uncertain_recovery', syncState: 'QUEUED' }
  expect(await store.readNotices()).toStrictEqual([notice])
  const c = createTestPage({ dbName, now: () => now })
  await c.ready()
  expect(await c.store().readNotices()).toStrictEqual([notice])
  expect(planReconcileBatches((await store.queued()).entries).batches[0].occurrences[0].event_type).toBe('exit')
})

it.each([undefined, null, '', 'EXIT', 'other', 1])('rejects a present corrupt current direction %s without legacy fallback', async (direction) => {
  const page = createTestPage({ dbName: `exit-bad-${crypto.randomUUID()}` })
  await page.ready()
  const { record } = await mintOn(page)
  expect(readCurrentRecord({ ...record, direction }).ok).toBe(false)
  expect(readCurrentRecord({ ...record, direction: 'exit' })).toStrictEqual({ ok: true, record: { ...record, direction: 'exit' } })
})
