import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import { seedDatabase, V2_STORES, rawGet } from './test/raw-db'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
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

let instrument: Instrument | undefined
let downloaded: Blob[]
const original = { occurrenceId: 'held-a', state: 'PENDING', qrPayload: 'private-credential', occurredAt: '2026-10-18T12:34:56.789Z', actuated: false, extra: { key: 'preserved' } }
const other = { occurrenceId: 'held-b', state: 'unknown', qrPayload: 'another-credential' }
const legacyQueue = { occurrenceId: 'legacy-q', state: 'QUEUED', qrPayload: 'queue-credential', occurredAt: '2026-10-18T12:34:56.123Z', actuated: false }
beforeEach(async () => {
  dbName = `app-records-${crypto.randomUUID()}`
  await seedDatabase(dbName, 2, V2_STORES, { occurrences: [original, other, legacyQueue] })
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  localStorage.clear(); downloaded = []
  vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => { downloaded.push(blob as Blob); return 'blob:test-export' })
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined)
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    if (typeof input === 'string' && input.includes('voided-tickets')) return new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
    const rows = JSON.parse(init!.body as string).occurrences
    return new Response(JSON.stringify({ results: rows.map((row: { occurrence_id: string }) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) }), { status: 200 })
  }))
})
afterEach(() => { cleanup(); instrument?.restore(); instrument = undefined; destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
async function exportRecords() {
  fireEvent.click(await screen.findByRole('button', { name: 'Export local records' }))
  await screen.findByText('Export requested. The browser cannot confirm that the file was saved.')
}
const ack = () => screen.getByRole('button', { name: 'Acknowledge selected held records' })

it.each([false, true])('shows hold and M02 controls in paired=%s with selected-only acknowledgement', async (paired) => {
  if (paired) localStorage.setItem('scanner.device-token', 'token')
  render(<App />)
  await screen.findByText('2 held record(s) need operator review. These records will not sync.')
  expect(screen.getByText('1 pre-upgrade queued record(s) are listed for review. Their original cause is unknown.')).toBeDefined()
  expect(screen.queryByLabelText('Held record legacy-q')).toBeNull()
  fireEvent.click(screen.getByLabelText('Held record held-a'))
  expect(ack().hasAttribute('disabled')).toBe(true)
  await exportRecords()
  expect(ack().hasAttribute('disabled')).toBe(false)
  fireEvent.submit(screen.getByRole('form', { name: 'Held record acknowledgement' }))
  await screen.findByText('Selected held records acknowledged. They remain stored and will not sync.')
  await screen.findByText('1 held record(s) need operator review. These records will not sync.')
  expect(await rawGet(dbName, STORE.occurrences, 'held-a')).toStrictEqual(original)
  expect(await rawGet(dbName, STORE.occurrences, 'held-b')).toStrictEqual(other)
  expect(await rawGet(dbName, STORE.heldDisposals, 'held-a')).toMatchObject({ disposition: 'operator_acknowledged_without_sync' })
  expect(await rawGet(dbName, STORE.heldDisposals, 'held-b')).toBeUndefined()
  expect(downloaded).toHaveLength(1)
  expect(await rawGetAll(dbName, STORE.exportReceipts)).toHaveLength(1)
})

it('does not enable acknowledgement when export dispatch throws', async () => {
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => { throw new Error('dispatch failed') })
  render(<App />)
  fireEvent.click(await screen.findByLabelText('Held record held-a'))
  fireEvent.click(screen.getByRole('button', { name: 'Export local records' }))
  await screen.findByText('Export was not confirmed. Held records remain unresolved. Request another export before acknowledgement.')
  expect(ack().hasAttribute('disabled')).toBe(true)
  expect(await rawGetAll(dbName, STORE.exportReceipts)).toEqual([])
  expect(await rawGetAll(dbName, STORE.heldDisposals)).toEqual([])
})

it('waits for receipt transaction completion before enabling acknowledgement', async () => {
  render(<App />); fireEvent.click(await screen.findByLabelText('Held record held-a'))
  instrument = instrumentTransactions()
  const held = instrument.hold({ stores: [STORE.exportReceipts], mode: 'readwrite', durability: 'strict' })
  try {
    fireEvent.click(screen.getByRole('button', { name: 'Export local records' }))
    await held.started
    expect(ack().hasAttribute('disabled')).toBe(true)
    expect(screen.queryByText('Export requested. The browser cannot confirm that the file was saved.')).toBeNull()
    await act(async () => { held.release() })
    await screen.findByText('Export requested. The browser cannot confirm that the file was saved.')
    expect(ack().hasAttribute('disabled')).toBe(false)
  } finally { held.release() }
})

it('keeps held rows unresolved when the export receipt aborts', async () => {
  render(<App />); fireEvent.click(await screen.findByLabelText('Held record held-a'))
  instrument = instrumentTransactions(); instrument.inject('abort', { stores: [STORE.exportReceipts], mode: 'readwrite' })
  fireEvent.click(screen.getByRole('button', { name: 'Export local records' }))
  await screen.findByText('Export was not confirmed. Held records remain unresolved. Request another export before acknowledgement.')
  expect(ack().hasAttribute('disabled')).toBe(true)
  expect(await rawGetAll(dbName, STORE.exportReceipts)).toEqual([])
})

it('reports an aborted selected acknowledgement without changing either held row', async () => {
  render(<App />); fireEvent.click(await screen.findByLabelText('Held record held-a')); fireEvent.click(screen.getByLabelText('Held record held-b'))
  await exportRecords()
  instrument = instrumentTransactions(); instrument.inject('abort', { mode: 'readwrite', when: (call) => call.storeNames.includes(STORE.heldDisposals) })
  fireEvent.submit(screen.getByRole('form', { name: 'Held record acknowledgement' }))
  await screen.findByText('Acknowledgement failed. Held records remain unresolved.')
  expect(await rawGetAll(dbName, STORE.heldDisposals)).toEqual([])
  expect(screen.getByText('2 held record(s) need operator review. These records will not sync.')).toBeDefined()
  expect(await rawGet(dbName, STORE.occurrences, 'held-a')).toStrictEqual(original)
})

it('keeps M02 export available at held zero after successful reconciliation', async () => {
  localStorage.setItem('scanner.device-token', 'token')
  render(<App />)
  await exportRecords()
  fireEvent.click(screen.getByLabelText('Held record held-a')); fireEvent.click(screen.getByLabelText('Held record held-b'))
  fireEvent.submit(screen.getByRole('form', { name: 'Held record acknowledgement' }))
  await screen.findByText('Selected held records acknowledged. They remain stored and will not sync.')
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(screen.queryByText(/held record\(s\) need operator review/)).toBeNull()
  expect(screen.getByText('1 pre-upgrade queued record(s) are listed for review. Their original cause is unknown.')).toBeDefined()
  expect(screen.getByRole('button', { name: 'Export local records' })).toBeDefined()
  expect(await rawGet(dbName, STORE.occurrences, 'legacy-q')).toMatchObject({ state: 'SYNCED', result: 'recorded' })
})

it('shows a durable recovery notice after retirement and acknowledges only the notice', async () => {
  page.destroy()
  let clock = 1000
  const prior = createTestPage({ dbName: `${dbName}-notice`, now: () => clock }); await prior.ready()
  const { scan } = await mintOn(prior)
  clock += 31000; prior.destroy()
  page = createTestPage({ dbName: `${dbName}-notice`, now: () => clock }); runtime.storage = page.storage; await page.ready()
  localStorage.setItem('scanner.device-token', 'token')
  const view = render(<App />)
  const expected = `Recovered scanScan ${scan.occurrenceId} at ${scan.occurredAt}. Type: entry. Recovery cause: uncertain_recovery. Sync state: QUEUED.The prior result and physical passage are unknown. Do not admit from this notice.Acknowledge recovery notice`
  await waitFor(() => expect(screen.getByRole('region', { name: `Recovered scan ${scan.occurrenceId}` }).textContent).toBe(expected))
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  await waitFor(() => expect(screen.getByRole('region', { name: `Recovered scan ${scan.occurrenceId}` }).textContent).toContain('Sync state: SYNCED.'))
  const before = await rawGet(`${dbName}-notice`, STORE.occurrences, scan.occurrenceId)
  instrument = instrumentTransactions(); instrument.inject('abort', { stores: [STORE.recoveryNotices], mode: 'readwrite' })
  fireEvent.click(screen.getByRole('button', { name: 'Acknowledge recovery notice' }))
  await screen.findByText('Notice acknowledgement failed. The notice remains.')
  fireEvent.click(screen.getByRole('button', { name: 'Acknowledge recovery notice' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: `Recovered scan ${scan.occurrenceId}` })).toBeNull())
  expect(await rawGet(`${dbName}-notice`, STORE.occurrences, scan.occurrenceId)).toStrictEqual(before)
  view.unmount(); render(<App />)
  await waitFor(() => expect(page.storage.readyStore()).toBeTruthy())
  expect(screen.queryByRole('heading', { name: 'Recovered scan' })).toBeNull()
})
