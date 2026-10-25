import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { TerminalDecision } from './occurrence-coordinator'
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
  page = createTestPage({ dbName, now: () => 1000 })
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

// Response injection tests client compatibility, including combinations Access does not emit.
const entryDenials: Array<{ reason: string; lines: string[] }> = [
  { reason: " Future_Reason ", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason:  Future_Reason ."] },
  { reason: "already_redeemed", lines: ["Already redeemed.", "Do not admit. Get an operator.", "Reason: already_redeemed."] },
  { reason: "entry_limit_reached", lines: ["Do not admit. No entries remain on this pass.", "Reason: entry_limit_reached."] },
  { reason: "exit_required", lines: ["Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.", "Reason: exit_required."] },
  { reason: "not_inside", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: not_inside."] },
  { reason: "exit_not_applicable", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_not_applicable."] },
  { reason: "invalid_credential", lines: ["Do not admit. Access did not accept this credential. Get an operator.", "Reason: invalid_credential."] },
  { reason: "revoked", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: revoked."] },
  { reason: "refunded", lines: ["Do not admit. This ticket was voided by a refund. Get an operator.", "Reason: refunded."] },
  { reason: "exchanged", lines: ["Do not admit. This ticket was replaced by an exchange. Ask for the replacement ticket and scan it.", "Reason: exchanged."] },
  { reason: "exit_unverified", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_unverified."] },
  { reason: "integrity_quarantined", lines: ["Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.", "Reason: integrity_quarantined."] },
  { reason: "integrity_operator_controlled", lines: ["Do not admit. Access refused this ticket under operator-controlled integrity handling. Get an operator.", "Reason: integrity_operator_controlled."] },
  { reason: "occurrence_required", lines: ["Do not admit. This scan is missing its required occurrence ID. Get an operator to check the scanner.", "Reason: occurrence_required."] },
  { reason: "unknown_future_reason", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: unknown_future_reason."] },
  { reason: "invalid_occurrence", lines: ["Do not admit. This scan has invalid occurrence data. Get an operator to check the scanner.", "Reason: invalid_occurrence."] },
  { reason: "occurrence_collision", lines: ["Do not admit. This scan's occurrence ID conflicts with an existing record. Get an operator.", "Reason: occurrence_collision."] },
  { reason: "future_reason", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: future_reason."] },
  { reason: "__proto__", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: __proto__."] },
  { reason: "<b>future_reason</b>", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: <b>future_reason</b>."] },
]
const originalScanAt = '2026-10-18T09:00:01.456Z'
const deviceDate = '2035-02-03T04:05:06.789Z'
const decisionDate = '2035-02-03T04:05:07.123Z'
function fixedPresentationDate() {
  const NativeDate = Date
  let valueNow = deviceDate
  // Date.now and timers remain real so fixture deadlines cannot freeze.
  vi.stubGlobal('Date', class extends NativeDate {
    constructor(value?: string | number) { super(value ?? valueNow) }
  })
  return (value: string) => { valueNow = value }
}
async function submittedDenial(status: 409 | 422, body: unknown, reason: string | null, lines: string[], unreadable = false) {
  const control = await queueControl()
  const setPresentationDate = fixedPresentationDate()
  const latch = page.coordinator.latch.bind(page.coordinator)
  const decisions: TerminalDecision[] = []
  vi.spyOn(page.coordinator, 'latch').mockImplementation(proposal => {
    if (proposal.kind === 'terminal') decisions.push(structuredClone(proposal))
    const result = latch(proposal)
    setPresentationDate('2035-02-03T04:05:08.456Z')
    return result
  })
  const mark = vi.spyOn(page.store(), 'reserveActuation')
  let pending: unknown
  scanReply = async () => {
    pending = await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)
    setPresentationDate(decisionDate)
    return new Response(unreadable ? '<html>not json</html>' : JSON.stringify(body), { status })
  }
  render(<App />); await scan()
  await waitFor(() => expect(document.querySelector('[aria-label="Scan result"]')).not.toBeNull())
  const result = screen.getByRole('alert', { name: 'Scan result' })
  expect(result.className).toBe('result rejected')
  expect(result.textContent).toBe(`Rejected${lines.join('')}`)
  expect([...result.querySelectorAll('p')].map(p => p.textContent)).toEqual(lines)
  expect(result.querySelector('b')).toBeNull()
  expect(scans).toHaveLength(1)
  const id = scans[0].occurrence_id as string
  expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  expect(scans).toStrictEqual([{ occurrence_id: id, occurred_at: deviceDate, qr_payload: 'credential', direction: 'entry' }])
  const identity = { occurrenceId: id, occurredAt: deviceDate, createdAt: deviceDate, qrPayload: 'credential',
    ownerId: page.coordinator.ownerId, direction: 'entry', recordFormat: 1, leaseExpiresAt: 31000, actuated: false }
  expect(pending).toStrictEqual({ ...identity, state: 'PENDING' })
  expect(decisions).toHaveLength(1)
  const first = decisions[0]
  if (first.outcome.cause !== 'denied') throw new Error('expected the original denial latch')
  const latchId = first.outcome.latchId
  expect(latchId).toMatch(/^[0-9a-f-]{36}$/)
  expect(first).toStrictEqual({ kind: 'terminal', occurrenceId: id, decidedAt: decisionDate,
    outcome: { family: 'terminal', cause: 'denied', status, latchId, reason: null } })
  const resolved = { ...identity, state: 'RESOLVED', decidedAt: first.decidedAt,
    localOutcome: { family: 'terminal', cause: 'denied', status, latchId, reason } }
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(resolved))
  expect(mark).not.toHaveBeenCalled()
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(batches).toStrictEqual([[{ occurrence_id: control.occurrenceId, qr_payload: control.qrPayload,
    occurred_at: control.occurredAt, event_type: 'entry' }]])
  expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(resolved)
  expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe(`Rejected${lines.join('')}`)
  expect(scans).toHaveLength(1)
  expect(mark).not.toHaveBeenCalled()
  expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
}

it.each(([409, 422] as const).flatMap(status => entryDenials.map(entry => ({ status, ...entry }))))(
  'TKT-565 submitted Entry $status $reason stores the exact denial and syncs only its control', async ({ status, reason, lines }) => {
    await submittedDenial(status, { decision: 'rejected', reason }, reason, lines)
  })
it.each([409, 422] as const)('TKT-565 submitted Entry %s retains supplied already_redeemed Access time', async status => {
  const localized = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date(originalScanAt))
  await submittedDenial(status, { decision: 'rejected', reason: 'already_redeemed', original_scan_at: originalScanAt }, 'already_redeemed', [
    `Already redeemed at ${localized}.`, 'Do not admit. Get an operator.', 'Reason: already_redeemed.',
  ])
})
it.each([409, 422] as const)('TKT-565 submitted Entry %s does not display the supplied time for integrity_quarantined', async status => {
  await submittedDenial(status, { decision: 'rejected', reason: 'integrity_quarantined', original_scan_at: originalScanAt }, 'integrity_quarantined', [
    'Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.', 'Reason: integrity_quarantined.',
  ])
})
const unreadableDenials = [
  { name: 'malformed time', body: { decision: 'rejected', reason: 'already_redeemed', original_scan_at: 'not-a-time' } },
  { name: 'missing reason', body: { decision: 'rejected' } },
  { name: 'wrong-type reason', body: { decision: 'rejected', reason: 7 } },
  { name: 'unreadable body', body: null, unreadable: true },
]
it.each(([409, 422] as const).flatMap(status => unreadableDenials.map(entry => ({ status, ...entry }))))(
  'TKT-565 submitted Entry $status $name retains null reason and terminal facts', async ({ status, body, unreadable }) => {
    await submittedDenial(status, body, null, ['Access refused this scan; reason unavailable. Do not admit'], unreadable)
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
