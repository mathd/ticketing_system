import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { StorageController } from './occurrence-readiness'
import App from './App'
import { createTestPage, destroyAllPages, mintOn, type TestPage } from './test/page-fixture'
import { rawGet } from './test/raw-db'
import { STORE } from './occurrence-store'

const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => {
  const actual = await load<typeof import('./occurrence-readiness')>()
  return { ...actual, get pageStorage() { return runtime.storage! } }
})
let page: TestPage
let dbName: string
let scans: Record<string, unknown>[]
let batches: Record<string, unknown>[][]
let scanReply: () => Promise<Response>
beforeEach(async () => {
  dbName = `app-entry-${crypto.randomUUID()}`
  page = createTestPage({ dbName })
  runtime.storage = page.storage
  await page.ready()
  scans = []; batches = []
  localStorage.setItem('scanner.device-token', 'token-a')
  scanReply = async () => new Response('{}', { status: 500 })
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.url
    if (url.includes('voided-tickets')) return new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
    const body = typeof input === 'string' ? JSON.parse(init!.body as string) : await input.clone().json()
    if (url.endsWith('/reconciliations')) {
      batches.push(body.occurrences)
      return new Response(JSON.stringify({ results: body.occurrences.map((row: { occurrence_id: string }) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) }), { status: 200 })
    }
    scans.push(body)
    return scanReply()
  }))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); sessionStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
async function scan() {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'credential' } })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
}
async function queueControl() {
  const { scan } = await mintOn(page)
  page.coordinator.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-18T09:00:00.123Z' })
  await page.coordinator.persist(scan.occurrenceId)
  return scan
}

it.each([401, 500, 501, 505, 599, 201, 301, 400, 403, 404, 429])('keeps live status %s out of real App reconciliation while its queue control syncs', async (status) => {
  const control = await queueControl()
  const reply = new Response('<html>unreadable</html>', { status })
  const json = vi.spyOn(reply, 'json')
  scanReply = async () => reply
  render(<App />)
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'credential' } })
  await scan()
  const expected = status === 401
    ? 'Pair this deviceThis attempt was not authorized. Do not admit from this result.Pair this device and scan the ticket again.'
    : status >= 500 ? 'Operator requiredOutcome unknown. Do not admit. Contact an operator.'
      : 'Operator requiredScan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.'
  await waitFor(() => expect(screen.getByRole(status === 401 ? 'alert' : 'alert', { name: 'Scan result' }).textContent).toBe(expected))
  expect(json).not.toHaveBeenCalled()
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED' }))
  if (status === 401) {
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'token-b' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
  }
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await waitFor(() => expect(batches.flat().map((r) => r.occurrence_id)).toEqual([control.occurrenceId]))
  expect(batches[0][0]).toStrictEqual({ occurrence_id: control.occurrenceId, qr_payload: control.qrPayload, occurred_at: control.occurredAt, event_type: 'entry' })
  expect(scans[0].direction).toBe('entry')
})

it('publishes an accepted result through the real mark and keeps it stable on refresh and sync', async () => {
  scanReply = async () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T09:00:01.456Z', replay: true }), { status: 200 })
  render(<App />)
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'credential' } })
  await scan()
  const localized = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date('2026-10-18T09:00:01.456Z'))
  const text = `AcceptedEntry accepted. Admit.Entry recorded at ${localized}.This scan was previously recorded. Entry counted once.`
  await waitFor(() => expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe(text))
  expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'ACTUATED', actuated: true })
  await act(async () => { window.dispatchEvent(new Event('online')) })
  expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe(text)
  expect(batches).toEqual([])
})

it.each(['already_redeemed', 'entry_limit_reached', 'exit_required', 'not_inside', 'exit_not_applicable', 'invalid_credential', 'revoked', 'refunded', 'exchanged', 'exit_unverified', 'integrity_quarantined', 'integrity_operator_controlled', 'occurrence_required', 'unknown_future_reason', 'invalid_occurrence', 'occurrence_collision'])('keeps the exact denial reason %s and the complete current Entry instruction', async (reason) => {
  scanReply = async () => new Response(JSON.stringify({ decision: 'rejected', reason }), { status: 422 })
  render(<App />); await scan()
  const expected = reason === 'already_redeemed' ? 'RejectedAlready redeemed.'
    : reason === 'exit_required' ? 'RejectedDo not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.Reason: exit_required.'
      : reason === 'entry_limit_reached' ? 'RejectedDo not admit. No entries remain on this pass.Reason: entry_limit_reached.'
        : 'RejectedCredential is invalid or cannot be redeemed.'
  await waitFor(() => expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe(expected))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status: 422, reason } }))
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([409, 422])('retains an unknown denial at status %s when the body is unreadable', async (status) => {
  scanReply = async () => new Response('<html>not json</html>', { status })
  render(<App />); await scan()
  await waitFor(() => expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe('RejectedAccess refused this scan; reason unavailable. Do not admit'))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'denied', reason: null } }))
})

it.each([502, 503, 504])('publishes the exact offline instruction for %s and retains it through reconciliation', async (status) => {
  scanReply = async () => new Response('<html>unreadable</html>', { status })
  render(<App />); await scan()
  const text = `Offline decisionServer response ${status}.Admit per venue offline policy.`
  await waitFor(() => expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe(text))
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe(text)
  expect(batches[0]).toHaveLength(1)
})

it('publishes the local-failure instruction for a non-network TypeError', async () => {
  scanReply = async () => { throw new TypeError('application failed') }
  render(<App />); await scan()
  await waitFor(() => expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe('Operator requiredLocal scanner failure. Scanner did not open. Do not admit from this result; get an operator.'))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'local_failure' } }))
})

it('automatically reconciles a listed refusal as refusal-only beside an explicit Entry control', async () => {
  const control = await queueControl()
  const tid = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
  await page.store().mergeRevoked([tid])
  render(<App />)
  const qr = `header.${btoa(JSON.stringify({ tid })).replace(/=+$/, '')}.signature`
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: qr } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
  await screen.findByRole('heading', { name: 'Not valid for entry' })
  await screen.findByText('Synced 2 offline scan(s).')
  expect(scans).toEqual([])
  const rows = batches.flat()
  expect(rows).toHaveLength(2)
  expect(rows.find((row) => row.occurrence_id === control.occurrenceId)).toStrictEqual({ occurrence_id: control.occurrenceId, qr_payload: control.qrPayload, occurred_at: control.occurredAt, event_type: 'entry' })
  const refused = rows.find((row) => row.occurrence_id !== control.occurrenceId)!
  const stored = await rawGet(dbName, STORE.occurrences, refused.occurrence_id as string) as Record<string, unknown>
  expect(refused).toStrictEqual({ occurrence_id: stored.occurrenceId, qr_payload: qr, occurred_at: stored.occurredAt, local_decision: 'revocation_refused' })
})

it.each([
  'Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0',
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15',
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
])('keeps 503 offline authority independent of browser family: %s', async (userAgent) => {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(userAgent)
  const prior = Object.getOwnPropertyDescriptor(navigator, 'userAgentData')
  Object.defineProperty(navigator, 'userAgentData', { configurable: true, value: undefined })
  try {
    scanReply = async () => new Response('', { status: 503 })
    render(<App />); await scan()
    await waitFor(() => expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe('Offline decisionServer response 503.Admit per venue offline policy.'))
  } finally {
    if (prior) Object.defineProperty(navigator, 'userAgentData', prior)
    else Reflect.deleteProperty(navigator, 'userAgentData')
  }
})
