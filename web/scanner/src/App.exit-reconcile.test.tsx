import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { rawGetAll } from './test/raw-db'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async load => ({ ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! } }))
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
const failures = ['transport', '401', 'syntax', 'null', 'missing', 'type', 'foreign', 'duplicate', 'omitted', 'domain', 'time', 'skew']
it.each(failures)('keeps exact mixed Entry, Exit and refusal rows after %s and retires only the exact valid reply', async failure => {
  const dbName = `mixed-exit-${crypto.randomUUID()}`, page = createTestPage({ dbName })
  await page.ready(); runtime.storage = page.storage; localStorage.setItem('scanner.device-token', 'paired')
  for (const kind of ['entry', 'exit', 'refusal'] as const) {
    const { scan } = await mintOn(page, kind === 'refusal' ? { type: 'refusal' } : { direction: kind })
    page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: kind === 'refusal' ? 'local_refusal' : 'transport_failure' }, decidedAt: '2026-10-19T12:00:01.123Z' })
    await page.coordinator.persist(scan.occurrenceId)
  }
  const before = await rawGetAll(dbName, STORE.occurrences) as Array<Record<string, unknown>>
  let broken = true
  const batches: Array<Array<Record<string, unknown>>> = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    if (input.includes('voided-tickets')) return Response.json({ ticket_ids: [], next_cursor: null })
    const rows = JSON.parse(init!.body as string).occurrences as Array<Record<string, unknown>>; batches.push(rows)
    const results = rows.map(row => ({ occurrence_id: row.occurrence_id, result: 'recorded' }))
    if (!broken) return Response.json({ results })
    switch (failure) {
      case 'transport': throw new TypeError('Failed to fetch')
      case '401': return new Response('unreadable', { status: 401 })
      case 'syntax': return new Response('{truncated')
      case 'null': return Response.json(null)
      case 'missing': return Response.json({})
      case 'type': return Response.json({ results: {} })
      case 'foreign': results[1].occurrence_id = crypto.randomUUID(); break
      case 'duplicate': results[1].occurrence_id = results[0].occurrence_id; break
      case 'omitted': results.pop(); break
      case 'domain': results[1].result = 'done'; break
      case 'time': Object.assign(results[1], { occurred_at: 'invalid' }); break
      case 'skew': Object.assign(results[1], { skew_flagged: 'false' }); break
    }
    return Response.json({ results })
  }))
  render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  if (failure === '401') await screen.findByLabelText('Pairing token')
  else await screen.findByText(/^Sync failed:/)
  expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before)
  const expected = before.map(row => ({ occurrence_id: row.occurrenceId, qr_payload: row.qrPayload, occurred_at: row.occurredAt,
    ...(row.localDecision === undefined ? { event_type: row.direction } : { local_decision: row.localDecision }) }))
  expect(batches[0]).toStrictEqual(expected)
  broken = false
  if (failure === '401') {
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'repaired' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
    await screen.findByLabelText('Ticket credential')
  }
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 3 offline scan(s).')
  await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before.map(row => ({ ...row, state: 'SYNCED', result: 'recorded' }))))
  expect(batches[1]).toStrictEqual(expected)
})
