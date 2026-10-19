import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import type { StorageController } from './occurrence-readiness'
import { productionScanClock, productionScanScheduler } from './scan-clock'
import { readableTime } from './scan-result'
import { STORE } from './occurrence-store'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { rawGetAll } from './test/raw-db'

const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async load => ({ ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! } }))
let page: TestPage
let dbName: string
let elapsed: number
let calls: Array<Record<string, unknown>>
let response: () => Promise<Response>
const credential = `header.${btoa(JSON.stringify({ tid: '11111111-1111-4111-8111-111111111111' })).replace(/=/g, '')}.signature`
beforeEach(async () => {
  dbName = `exit-results-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); await page.ready(); runtime.storage = page.storage
  localStorage.setItem('scanner.device-token', 'paired')
  elapsed = 0; calls = []
  vi.spyOn(productionScanClock, 'wall').mockImplementation(() => 100000 + elapsed)
  vi.spyOn(productionScanClock, 'monotonic').mockImplementation(() => 100 + elapsed)
  vi.spyOn(productionScanScheduler, 'set').mockReturnValue(0)
  vi.spyOn(productionScanScheduler, 'clear').mockImplementation(() => undefined)
  response = async () => Response.json({ decision: 'accepted', scanned_at: '2026-10-19T12:00:00.123Z' })
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string) => {
    if (!(input instanceof Request)) return Response.json({ ticket_ids: [], next_cursor: null })
    calls.push(await input.clone().json()); return response()
  }))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); sessionStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
function submit() {
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: credential } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
}
const region = () => screen.getByRole('status', { name: 'Scan result' })

it.each(['lookup', 'mint'] as const)('renders the full no-send Exit at the literal 3000 ms %s continuation', async phase => {
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  let entered = false
  const store = page.store()
  if (phase === 'lookup') {
    const lookup = store.isRevoked.bind(store)
    vi.spyOn(store, 'isRevoked').mockImplementation(async id => { const value = await lookup(id); entered = true; await held; return value })
  } else {
    const mint = store.mintNormal.bind(store)
    vi.spyOn(store, 'mintNormal').mockImplementation(async id => { const row = await mint(id); entered = true; await held; return row })
  }
  render(<App />); submit()
  try {
    await waitFor(() => expect(entered).toBe(true))
    await act(async () => { elapsed = 3000; release() })
    await waitFor(() => expect(region().textContent).toBe('Scan stoppedScan took too long or its clock could not be checked. No request was sent. Do not admit from this result. Scan again.Exit not recorded; the holder may leave'))
    expect(region().className).toContain('neutral'); expect(calls).toEqual([])
    if (phase === 'lookup') expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
    else await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ direction: 'exit', state: 'RESOLVED' }]))
  } finally { release() }
})

it.each(['body', 'mark'] as const)('renders the full known accepted Exit at the literal 30000 ms %s continuation', async phase => {
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  let entered = false
  if (phase === 'body') response = async () => {
    const answer = Response.json({ decision: 'accepted', scanned_at: '2026-10-19T12:00:00.123Z' })
    const json = answer.json.bind(answer)
    vi.spyOn(answer, 'json').mockImplementation(async () => { const body = await json(); entered = true; await held; return body })
    return answer
  }
  else {
    const reserve = page.store().reserveActuation.bind(page.store())
    vi.spyOn(page.store(), 'reserveActuation').mockImplementation(async id => { const mark = await reserve(id); entered = true; await held; return mark })
  }
  render(<App />); submit()
  try {
    await waitFor(() => expect(entered).toBe(true))
    await act(async () => { elapsed = 30000; release() })
    await waitFor(() => expect(region().textContent).toBe('Operator requiredAccess recorded this Exit, but the scanner could not finish in time. The scanner did not open. Get an operator; do not admit from this result.The holder may leave.'))
    expect(region().className).toContain('neutral'); expect(calls).toHaveLength(1); expect(calls[0].direction).toBe('exit')
    await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ direction: 'exit', state: phase === 'body' ? 'RESOLVED' : 'ACTUATED' }]))
  } finally { release() }
})

it.each(['clock', 'header'] as const)('renders full pre-send local Exit failure from %s without a request', async fault => {
  if (fault === 'clock') vi.spyOn(productionScanClock, 'wall').mockImplementation(() => { throw new Error('clock unavailable') })
  else localStorage.setItem('scanner.device-token', 'invalid\nheader')
  render(<App />); submit()
  await waitFor(() => expect(region().textContent).toBe('Operator requiredLocal scanner failure. Scanner did not open. Do not admit from this result; get an operator.Exit not recorded; the holder may leave'))
  expect(region().className).toContain('neutral'); expect(calls).toEqual([])
  if (fault === 'clock') expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
  else await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ direction: 'exit', state: 'RESOLVED', localOutcome: { cause: 'local_failure', requestMayHaveBeenSent: false } }]))
})

it.each(['already_redeemed', 'integrity_quarantined'])('uses a supplied original time only for the correct Exit reason %s', async reason => {
  const time = '2026-10-19T11:59:00.456Z'
  response = async () => Response.json({ decision: 'rejected', reason, original_scan_at: time }, { status: 422 })
  render(<App />); submit()
  await waitFor(() => expect(region().textContent).toBe(`Exit not recordedExit not recorded; reason: ${reason}. The holder may leave.${reason === 'already_redeemed' ? `Already redeemed at ${readableTime(time)}.` : ''}`))
})

it('preserves the Entry introduction and varies only the Exit introduction', () => {
  render(<App />)
  expect(screen.getByText('Scan a ticket QR code or paste its credential to validate entry.')).toBeDefined()
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  expect(screen.getByText('Scan a ticket QR code or paste its credential to record Exit.')).toBeDefined()
})
