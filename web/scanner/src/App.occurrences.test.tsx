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

it('captures a fresh id and exact committed device time for each Entry request', async () => {
  render(<App />)
  for (const value of ['first', 'second', 'header.!.signature']) {
    await scan(value)
    await screen.findByRole('heading', { name: 'Accepted' })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  }
  expect(new Set(bodies.map((body) => body.occurrence_id)).size).toBe(3)
  const rows = await rawGetAll(dbName, STORE.occurrences) as Array<Record<string, unknown>>
  for (const [index, body] of bodies.entries()) {
    expect(body.occurrence_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    const row = rows.find((item) => item.occurrenceId === body.occurrence_id)!
    expect(body).toStrictEqual({ occurrence_id: row.occurrenceId, qr_payload: ['first', 'second', 'header.!.signature'][index], occurred_at: row.occurredAt, direction: 'entry' })
    expect(row).toMatchObject({ state: 'ACTUATED', actuated: true })
  }
})

it('queues a native no-response Entry and preserves the exact Entry wire fields on a conflict', async () => {
  scanResponse = async () => { throw new TypeError('Failed to fetch') }
  render(<App />); await scan('offline-ticket')
  expect((await screen.findByRole('status', { name: 'Scan result' })).textContent).toBe('Offline decisionNo response was received.Admit per venue offline policy.')
  const button = await screen.findByRole('button', { name: 'Sync queued scans' })
  let sent: unknown
  reconcileResponse = (rows) => {
    sent = rows
    return new Response(JSON.stringify({ results: rows.map((row) => ({ occurrence_id: row.occurrence_id, result: 'conflict' })) }), { status: 200 })
  }
  fireEvent.click(button)
  await screen.findByText('Synced 0 offline scan(s). 1 conflict flagged for the operator.')
  expect(sent).toStrictEqual([{ occurrence_id: bodies[0].occurrence_id, qr_payload: 'offline-ticket', occurred_at: bodies[0].occurred_at, event_type: 'entry' }])
  expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ state: 'RESOLVED', result: 'conflict', actuated: false }])
  expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe('Offline decisionNo response was received.Admit per venue offline policy.')
})
