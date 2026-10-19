import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
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

let bodies: Record<string, unknown>[]
let scanResponse: () => Promise<Response>
let reconcileResponse: (rows: Array<{ occurrence_id: string }>) => Response
beforeEach(async () => {
  dbName = `app-contract-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  localStorage.setItem('scanner.device-token', 'token')
  bodies = []
  scanResponse = async () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T12:34:56.789Z' }), { status: 200 })
  reconcileResponse = (rows) => new Response(JSON.stringify({ results: rows.map((row) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) }), { status: 200 })
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    if (typeof input === 'string') {
      if (input.includes('voided-tickets')) return new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      return reconcileResponse(JSON.parse(init!.body as string).occurrences)
    }
    bodies.push(await input.clone().json())
    return scanResponse()
  }))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
async function scan(value = 'credential') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
}

// ADR-085 U02 is terminal even when the accepted-looking body lacks its time.
it.each([{ decision: 'accepted' }, { decision: 'accepted', scanned_at: 'bad' }, { decision: 'rejected', reason: 'refunded' }])('resolves an invalid 200 without admission or reconciliation: %s', async (body) => {
  scanResponse = async () => new Response(JSON.stringify(body), { status: 200 })
  render(<App />); await scan()
  expect((await screen.findByRole('alert', { name: 'Scan result' })).textContent).toBe('Operator requiredScan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.')
  await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ state: 'RESOLVED', actuated: false, localOutcome: { cause: 'unconfirmed_accepted_body' } }]))
  expect((await page.store().queued()).entries).toEqual([])
})

it('does not retire a queued occurrence for an unknown reconciliation result', async () => {
  scanResponse = async () => new Response('', { status: 503 })
  render(<App />); await scan()
  const button = await screen.findByRole('button', { name: 'Sync queued scans' })
  const before = await rawGetAll(dbName, STORE.occurrences)
  reconcileResponse = (rows) => new Response(JSON.stringify({ results: rows.map((row) => ({ occurrence_id: row.occurrence_id, result: 'done' })) }), { status: 200 })
  fireEvent.click(button)
  await screen.findByText(/^Sync failed: The server answered/)
  expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before)
  expect(screen.getByText(/1 queued offline scan awaiting sync/)).toBeDefined()
})
