import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, screen } from '@testing-library/react'
import App from './App'
import { renderWithQueue } from './test/render-queued'
import { mintOn } from './test/page-fixture'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { rawGetAll } from './test/raw-db'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => {
  const actual = await load<typeof import('./occurrence-readiness')>()
  return { ...actual, get pageStorage() { return runtime.storage! } }
})
let page: TestPage
let dbName: string

let keys: string[]
let answer: (rows: Array<{ occurrence_id: string }>) => Response
const recorded = (rows: Array<{ occurrence_id: string }>) => ({ results: rows.map((row) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) })
beforeEach(async () => {
  dbName = `app-reconcile-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  keys = []
  for (const qrPayload of ['first', 'second']) {
    const { scan } = await mintOn(page, { qrPayload })
    keys.push(scan.occurrenceId)
    page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-18T12:34:56.789Z' })
    await page.coordinator.persist(scan.occurrenceId)
  }
  keys.sort()
  localStorage.setItem('scanner.device-token', 'token')
  answer = (rows) => new Response(JSON.stringify(recorded(rows)), { status: 200 })
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => input.includes('voided-tickets')
    ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }) : answer(JSON.parse(init!.body as string).occurrences)))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
const malformed: Array<[string, (rows: Array<{ occurrence_id: string }>) => unknown]> = [
  ['null', () => null], ['array', () => []], ['missing results', () => ({})], ['results type', () => ({ results: {} })],
  ['missing id', (rows) => ({ results: [recorded(rows).results[0], { result: 'recorded' }] })],
  ['missing result', (rows) => ({ results: [recorded(rows).results[0], { occurrence_id: rows[1].occurrence_id }] })],
  ['wrong result type', (rows) => ({ results: [recorded(rows).results[0], { occurrence_id: rows[1].occurrence_id, result: 1 }] })],
  ['unknown result', (rows) => ({ results: [recorded(rows).results[0], { occurrence_id: rows[1].occurrence_id, result: 'done' }] })],
  ['foreign id', (rows) => ({ results: [recorded(rows).results[0], { occurrence_id: crypto.randomUUID(), result: 'recorded' }] })],
  ['duplicate id', (rows) => ({ results: [recorded(rows).results[0], recorded(rows).results[0]] })],
  ['omitted id', (rows) => ({ results: [recorded(rows).results[0]] })],
  ['invalid optional time', (rows) => ({ results: [recorded(rows).results[0], { ...recorded(rows).results[1], occurred_at: 'not a time' }] })],
  ['invalid optional skew', (rows) => ({ results: [recorded(rows).results[0], { ...recorded(rows).results[1], skew_flagged: 'false' }] })],
]
it.each(malformed)('keeps both complete rows on malformed reconciliation %s and retires both on a valid retry', async (_, build) => {
  const before = await rawGetAll(dbName, STORE.occurrences)
  answer = (rows) => new Response(JSON.stringify(build(rows)), { status: 200 })
  await renderWithQueue(<App />, page)
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText(/^Sync failed: The server answered/)
  expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before)
  answer = (rows) => new Response(JSON.stringify(recorded(rows)), { status: 200 })
  fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 2 offline scan(s).')
  expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ state: 'SYNCED', result: 'recorded' }, { state: 'SYNCED', result: 'recorded' }])
})

it('rolls back the valid first retirement when the second retirement write aborts', async () => {
  const before = await rawGetAll(dbName, STORE.occurrences)
  const put = IDBObjectStore.prototype.put
  let failed = false
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value: unknown, ...keysArg: [key?: IDBValidKey]) {
    const request = Reflect.apply(put, this, [value, ...keysArg])
    const row = value as { occurrenceId?: string; state?: string }
    if (!failed && this.name === STORE.occurrences && row.occurrenceId === keys[1] && row.state === 'SYNCED') {
      failed = true; request.addEventListener('success', () => this.transaction.abort())
    }
    return request
  })
  await renderWithQueue(<App />, page); fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')
  expect(failed).toBe(true)
  expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before)
  expect(screen.queryByText(/^Synced /)).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 2 offline scan(s).')
})

it('reports a rejected result as not recorded beside a recorded sibling', async () => {
  answer = (rows) => new Response(JSON.stringify({ results: [{ occurrence_id: rows[0].occurrence_id, result: 'recorded' }, { occurrence_id: rows[1].occurrence_id, result: 'rejected' }] }), { status: 200 })
  await renderWithQueue(<App />, page); fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(screen.getByText(`Reconciliation rejected for ${keys[1]}. This result is not recorded.`)).toBeDefined()
  expect(await rawGetAll(dbName, STORE.occurrences)).toEqual(expect.arrayContaining([expect.objectContaining({ occurrenceId: keys[0], state: 'SYNCED', result: 'recorded' }), expect.objectContaining({ occurrenceId: keys[1], state: 'RESOLVED', result: 'rejected' })]))
})
