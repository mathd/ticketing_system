import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { forceCloseDatabase } from 'fake-indexeddb'
import App from './App'
import { mintOn } from './test/page-fixture'
import { rawGet } from './test/raw-db'
import type { TerminalOutcome } from './occurrence-model'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => {
  const actual = await load<typeof import('./occurrence-readiness')>()
  return { ...actual, get pageStorage() { return runtime.storage! } }
})
let page: TestPage
let dbName: string

let clock: number
let target: string
let failures: number
let armed: boolean
let scans: Record<string, unknown>[]
let batches: Array<Record<string, unknown>>[]
let reply: () => Promise<Response>
const reasons = ['already_redeemed', 'entry_limit_reached', 'exit_required', 'not_inside', 'exit_not_applicable', 'invalid_credential', 'revoked', 'refunded', 'exchanged', 'exit_unverified', 'integrity_quarantined', 'integrity_operator_controlled', 'occurrence_required', 'future_reason', 'invalid_occurrence', 'occurrence_collision']
const generic = 'RejectedCredential is invalid or cannot be redeemed.'
const unconfirmed = 'Operator requiredScan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.'
const profiles: Array<{ name: string; status?: number; body?: unknown; error?: Error; text: string; outcome: Partial<TerminalOutcome> }> = [
  ...reasons.map((reason) => ({ name: `denial ${reason}`, status: 422, body: { decision: 'rejected', reason }, text: reason === 'already_redeemed' ? 'RejectedAlready redeemed.' : generic, outcome: { cause: 'denied' as const, status: 422 as const, reason } })),
  ...[409, 422].map((status) => ({ name: `unknown denial ${status}`, status, body: {}, text: 'RejectedAccess refused this scan; reason unavailable. Do not admit', outcome: { cause: 'denied' as const, status: status as 409 | 422, reason: null } })),
  { name: 'auth refusal', status: 401, text: 'Pair this deviceThis attempt was not authorized. Do not admit from this result.Pair this device and scan the ticket again.', outcome: { cause: 'auth_refused' } },
  ...[500, 501, 505, 599].map((status) => ({ name: `server ${status}`, status, text: 'Operator requiredOutcome unknown. Do not admit. Contact an operator.', outcome: { cause: 'unconfirmed_server_error' as const, status } })),
  ...[201, 301, 400, 403, 404, 429].map((status) => ({ name: `other status ${status}`, status, text: unconfirmed, outcome: { cause: 'unconfirmed_status' as const, status } })),
  { name: 'invalid accepted body', status: 200, body: { decision: 'accepted' }, text: unconfirmed, outcome: { cause: 'unconfirmed_accepted_body' } },
  ...[new TypeError('application failure'), new Error('Failed to fetch'), new DOMException('Failed to fetch', 'NetworkError')].map((error) => ({ name: error.constructor.name, error, text: 'Operator requiredLocal scanner failure. Scanner did not open. Do not admit from this result; get an operator.', outcome: { cause: 'local_failure' as const, requestMayHaveBeenSent: true } })),
]
beforeEach(async () => {
  dbName = `app-safety-${crypto.randomUUID()}`; clock = 1000
  page = createTestPage({ dbName, now: () => clock }); runtime.storage = page.storage; await page.ready()
  localStorage.setItem('scanner.device-token', 'token-a')
  target = ''; failures = 0; armed = true; scans = []; batches = []
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    if (typeof input === 'string' && input.includes('voided-tickets')) return new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
    if (typeof input === 'string') {
      const body = JSON.parse(init!.body as string); batches.push(body.occurrences)
      return new Response(JSON.stringify({ results: body.occurrences.map((row: { occurrence_id: string }) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) }), { status: 200 })
    }
    const body = await input.clone().json(); scans.push(body); target = body.occurrence_id
    return reply()
  }))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

for (const mode of ['throw', 'abort'] as const) it.each(profiles)('protects $name through a '+mode+' final write, close, expiry and foreign recovery', async (profile) => {
  const { scan: control } = await mintOn(page)
  page.coordinator.latch({ kind: 'queue', occurrenceId: control.occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-18T12:34:56.123Z' })
  await page.coordinator.persist(control.occurrenceId)
  const put = IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value: unknown, ...keys: [key?: IDBValidKey]) {
    const row = value as { occurrenceId?: string; localOutcome?: { family: string } }
    const fail = armed && this.name === STORE.occurrences && row.occurrenceId === target && row.localOutcome?.family === 'terminal'
    if (fail && mode === 'throw') { failures++; throw new DOMException('injected failure', 'QuotaExceededError') }
    const request = Reflect.apply(put, this, [value, ...keys])
    if (fail) request.addEventListener('success', () => { failures++; this.transaction.abort() })
    return request
  })
  reply = async () => {
    if (profile.error) throw profile.error
    return new Response(profile.body === undefined ? '<html>unreadable</html>' : JSON.stringify(profile.body), { status: profile.status })
  }
  render(<App />)
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'credential' } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
  await waitFor(() => expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe(profile.text))
  await waitFor(() => expect(failures).toBeGreaterThan(0))
  const first = page.store()
  const owner = page.coordinator.ownerId
  forceCloseDatabase(first.db as never)
  await page.waitFor('failed')
  await screen.findAllByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')
  fireEvent.click(screen.getByRole('button', { name: 'Retry storage' }))
  await page.waitFor('ready')
  expect(page.coordinator.ownerId).toBe(owner)
  clock += 31000
  const other = createTestPage({ dbName, now: () => clock }); await other.ready()
  expect((await other.store().queued()).entries.map((row) => row.key)).toEqual([control.occurrenceId])
  expect(await rawGet(dbName, STORE.occurrences, target)).toMatchObject({ state: 'PENDING', ownerId: owner })
  if (profile.status === 401) {
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'token-b' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
    await screen.findByLabelText('Ticket credential')
  }
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(batches.flat().map((row) => row.occurrence_id)).toEqual([control.occurrenceId])
  armed = false
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Retry storage' })) })
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, target)).toMatchObject({ state: 'RESOLVED', ownerId: owner, localOutcome: { family: 'terminal', ...profile.outcome } }))
  if (profile.status !== 401) expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe(profile.text)
})
