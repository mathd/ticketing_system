import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { forceCloseDatabase } from 'fake-indexeddb'
import App from './App'
import { mintOn } from './test/page-fixture'
import { rawGet } from './test/raw-db'
import type { TerminalDecision } from './occurrence-coordinator'
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
const denialCopy: Array<{ reason: string; text: string }> = [
  { reason: " Future_Reason ", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason:  Future_Reason ." },
  { reason: "already_redeemed", text: "RejectedAlready redeemed.Do not admit. Get an operator.Reason: already_redeemed." },
  { reason: "entry_limit_reached", text: "RejectedDo not admit. No entries remain on this pass.Reason: entry_limit_reached." },
  { reason: "exit_required", text: "RejectedDo not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.Reason: exit_required." },
  { reason: "not_inside", text: "RejectedDo not admit. Access returned an Exit refusal for this Entry scan. Get an operator.Reason: not_inside." },
  { reason: "exit_not_applicable", text: "RejectedDo not admit. Access returned an Exit refusal for this Entry scan. Get an operator.Reason: exit_not_applicable." },
  { reason: "invalid_credential", text: "RejectedDo not admit. Access did not accept this credential. Get an operator.Reason: invalid_credential." },
  { reason: "revoked", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason: revoked." },
  { reason: "refunded", text: "RejectedDo not admit. This ticket was voided by a refund. Get an operator.Reason: refunded." },
  { reason: "exchanged", text: "RejectedDo not admit. This ticket was replaced by an exchange. Ask for the replacement ticket and scan it.Reason: exchanged." },
  { reason: "exit_unverified", text: "RejectedDo not admit. Access returned an Exit refusal for this Entry scan. Get an operator.Reason: exit_unverified." },
  { reason: "integrity_quarantined", text: "RejectedDo not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.Reason: integrity_quarantined." },
  { reason: "integrity_operator_controlled", text: "RejectedDo not admit. Access refused this ticket under operator-controlled integrity handling. Get an operator.Reason: integrity_operator_controlled." },
  { reason: "occurrence_required", text: "RejectedDo not admit. This scan is missing its required occurrence ID. Get an operator to check the scanner.Reason: occurrence_required." },
  { reason: "unknown_future_reason", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason: unknown_future_reason." },
  { reason: "invalid_occurrence", text: "RejectedDo not admit. This scan has invalid occurrence data. Get an operator to check the scanner.Reason: invalid_occurrence." },
  { reason: "occurrence_collision", text: "RejectedDo not admit. This scan's occurrence ID conflicts with an existing record. Get an operator.Reason: occurrence_collision." },
  { reason: "future_reason", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason: future_reason." },
  { reason: "__proto__", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason: __proto__." },
  { reason: "<b>future_reason</b>", text: "RejectedDo not admit. Access refused this scan. Get an operator.Reason: <b>future_reason</b>." },
]
const accessTime = '2026-10-18T09:00:01.456Z'
const suppliedTimeText = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date(accessTime))
const unconfirmed = 'Operator requiredScan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.'
const profiles: Array<{ name: string; status?: number; body?: unknown; error?: Error; text: string; outcome: Partial<TerminalOutcome> }> = [
  ...([409, 422] as const).flatMap(status => denialCopy.map(({ reason, text }) => ({ name: `denial ${status} ${reason}`, status,
    body: { decision: 'rejected', reason }, text, outcome: { cause: 'denied' as const, status, reason } }))),
  ...([409, 422] as const).map(status => ({ name: `denial ${status} already_redeemed supplied time`, status,
    body: { decision: 'rejected', reason: 'already_redeemed', original_scan_at: accessTime },
    text: `RejectedAlready redeemed at ${suppliedTimeText}.Do not admit. Get an operator.Reason: already_redeemed.`,
    outcome: { cause: 'denied' as const, status, reason: 'already_redeemed' } })),
  ...[409, 422].map((status) => ({ name: `unknown denial ${status}`, status, body: {}, text: 'RejectedAccess refused this scan; reason unavailable. Do not admit', outcome: { cause: 'denied' as const, status: status as 409 | 422, reason: null } })),
  { name: 'auth refusal', status: 401, text: 'Pair this deviceThis attempt was not authorized. Do not admit from this result.Pair this device and scan the ticket again.', outcome: { cause: 'auth_refused' } },
  ...[500, 501, 505, 599].map((status) => ({ name: `server ${status}`, status, text: 'Operator requiredOutcome unknown. Do not admit. Contact an operator.', outcome: { cause: 'unconfirmed_server_error' as const, status } })),
  ...[201, 301, 400, 403, 404, 429].map((status) => ({ name: `other status ${status}`, status, text: unconfirmed, outcome: { cause: 'unconfirmed_status' as const, status } })),
  { name: 'invalid accepted body', status: 200, body: { decision: 'accepted' }, text: unconfirmed, outcome: { cause: 'unconfirmed_accepted_body' } },
  ...[new TypeError('application failure'), new Error('Failed to fetch'), new DOMException('Failed to fetch', 'NetworkError')].map((error) => ({ name: error.constructor.name, error, text: 'Operator requiredLocal scanner failure. Scanner did not open. Do not admit from this result; get an operator.', outcome: { cause: 'local_failure' as const, requestMayHaveBeenSent: true } })),
]
beforeEach(async () => {
  dbName = `app-safety-${crypto.randomUUID()}`; clock = 1000
  // Keep the owner clock fixed even when the observing page sees an expired lease.
  page = createTestPage({ dbName, now: () => 1000 }); runtime.storage = page.storage; await page.ready()
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
  const NativeDate = Date
  let presentationDate = '2035-02-03T04:05:06.789Z'
  // Keep Date.now and all wait deadlines real; only claimed presentation dates are fixed.
  vi.stubGlobal('Date', class extends NativeDate {
    constructor(value?: string | number) { super(value ?? presentationDate) }
  })
  const latch = page.coordinator.latch.bind(page.coordinator)
  const decisions: TerminalDecision[] = []
  vi.spyOn(page.coordinator, 'latch').mockImplementation(proposal => {
    if (proposal.kind === 'terminal') decisions.push(structuredClone(proposal))
    const result = latch(proposal)
    presentationDate = '2035-02-03T04:05:08.456Z'
    return result
  })
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
    presentationDate = '2035-02-03T04:05:07.123Z'
    if (profile.error) throw profile.error
    return new Response(profile.body === undefined ? '<html>unreadable</html>' : JSON.stringify(profile.body), { status: profile.status })
  }
  try {
    render(<App />)
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'credential' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
    await waitFor(() => expect(document.querySelector('[aria-label="Scan result"]')).not.toBeNull())
    const assertText = () => {
      const result = screen.getByRole('alert', { name: 'Scan result' })
      expect(result.className).toBe('result rejected')
      expect(result.textContent).toBe(profile.text)
      expect(result.querySelector('b')).toBeNull()
    }
    assertText()
    await waitFor(() => expect(failures).toBeGreaterThan(0))
    await screen.findByText('The outcome write needs repair. Keep this page open and retry storage.')
    expect(scans).toHaveLength(1)
    expect(target).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    expect(scans).toStrictEqual([{ occurrence_id: target, qr_payload: 'credential', occurred_at: '2035-02-03T04:05:06.789Z', direction: 'entry' }])
    const owner = page.coordinator.ownerId
    const identity = { occurrenceId: target, qrPayload: 'credential', occurredAt: '2035-02-03T04:05:06.789Z',
      createdAt: '2035-02-03T04:05:06.789Z', ownerId: owner, direction: 'entry', recordFormat: 1, actuated: false }
    const assertPending = async () => {
      expect(await rawGet(dbName, STORE.occurrences, target)).toStrictEqual({ ...identity, leaseExpiresAt: 31000, state: 'PENDING' })
      assertText()
      expect(screen.getByText('The outcome write needs repair. Keep this page open and retry storage.')).toBeDefined()
    }
    await assertPending()
    expect(decisions).toHaveLength(1)
    const first = decisions[0]
    const expectedOutcome = profile.outcome.cause === 'denied'
      ? { family: 'terminal', ...profile.outcome, latchId: first.outcome.cause === 'denied' ? first.outcome.latchId : 'missing-denial-latch' }
      : { family: 'terminal', ...profile.outcome }
    expect(first).toStrictEqual({ kind: 'terminal', occurrenceId: target, decidedAt: '2035-02-03T04:05:07.123Z',
      outcome: profile.outcome.cause === 'denied' ? { ...expectedOutcome, reason: null } : expectedOutcome })
    if (first.outcome.cause === 'denied') expect(first.outcome.latchId).toMatch(/^[0-9a-f-]{36}$/)
    forceCloseDatabase(page.store().db as never)
    await page.waitFor('failed')
    await screen.findAllByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')
    await assertPending()
    const beforeRetry = failures
    fireEvent.click(screen.getByRole('button', { name: 'Retry storage' }))
    await page.waitFor('ready')
    await waitFor(() => expect(failures).toBeGreaterThan(beforeRetry))
    expect(page.coordinator.ownerId).toBe(owner)
    await assertPending()
    clock += 31000
    expect(clock).toBeGreaterThan(31000)
    const other = createTestPage({ dbName, now: () => clock }); await other.ready()
    expect((await other.store().queued()).entries.map(row => row.key)).toEqual([control.occurrenceId])
    await assertPending()
    if (profile.status === 401) {
      fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'token-b' } })
      fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
      await screen.findByLabelText('Ticket credential')
    }
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 1 offline scan(s).')
    expect(batches).toStrictEqual([[{ occurrence_id: control.occurrenceId, qr_payload: control.qrPayload,
      occurred_at: control.occurredAt, event_type: 'entry' }]])
    // Pairing intentionally clears a 401 result. All denial copy stays on screen.
    if (profile.status !== 401) await assertPending()
    const beforeRepair = await rawGet(dbName, STORE.occurrences, target) as Record<string, unknown>
    expect(beforeRepair).toStrictEqual({ ...identity, leaseExpiresAt: 31000, state: 'PENDING' })
    armed = false
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Retry storage' })) })
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, target)).toStrictEqual({
      ...identity, leaseExpiresAt: 31000, state: 'RESOLVED', decidedAt: first.decidedAt, localOutcome: expectedOutcome,
    }))
    if (profile.status !== 401) assertText()
    expect(scans).toHaveLength(1)
    expect(batches).toHaveLength(1)
  } finally { armed = false }
})
