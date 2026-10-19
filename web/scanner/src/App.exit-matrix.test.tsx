import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { forceCloseDatabase } from 'fake-indexeddb'
import App from './App'
import type { StorageController } from './occurrence-readiness'
import { STORE } from './occurrence-store'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import { rawGet, rawGetAll } from './test/raw-db'
import { ACCEPT_INSTRUCTION, OFFLINE_INSTRUCTION } from './scan-result'

const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async load => ({ ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! } }))
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
const reasons = ['already_redeemed', 'entry_limit_reached', 'exit_required', 'not_inside', 'exit_not_applicable',
  'invalid_credential', 'revoked', 'refunded', 'exchanged', 'exit_unverified', 'integrity_quarantined',
  'integrity_operator_controlled', 'occurrence_required', 'unknown_future_reason', 'invalid_occurrence', 'occurrence_collision']
const families = [...reasons, '401', '409-unreadable', '422-invalid', '200-invalid', '500', '501', '505', '599',
  'status', 'plain-error', 'dom-error', 'type-error', 'mark-failed']
const columns = ['S', 'F-throw', 'F-abort', 'C', 'T', 'V', 'P-committed', 'P-uncommitted'] as const
const cases = families.flatMap(family => columns.map(column => ({ family, column })))

it.each(cases)('Exit $family / $column preserves the exact terminal fact or explicit page-loss uncertainty', async ({ family, column }) => {
  const dbName = `exit-cell-${crypto.randomUUID()}`
  let now = 1000
  const a = createTestPage({ dbName, now: () => now, renewalMs: 3600000 })
  await a.ready(); runtime.storage = a.storage
  const control = await mintOn(a, { direction: 'entry' })
  a.coordinator.latch({ kind: 'queue', occurrenceId: control.scan.occurrenceId,
    outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-19T12:00:00.123Z' })
  await a.coordinator.persist(control.scan.occurrenceId)
  localStorage.setItem('scanner.device-token', 'paired')
  let id = '', initial: unknown, blocked = !['S', 'P-committed'].includes(column), failures = 0
  const mint = a.store().mintNormal.bind(a.store())
  vi.spyOn(a.store(), 'mintNormal').mockImplementation(async key => {
    const row = await mint(key); id = key; initial = row; return row
  })
  const put = IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value: unknown, ...keys: [key?: IDBValidKey]) {
    const row = value as { occurrenceId?: string; state?: string }
    const target = blocked && row.occurrenceId === id && row.state === 'RESOLVED'
    if (target && column !== 'F-abort') { failures++; throw new DOMException('terminal write blocked', 'QuotaExceededError') }
    const request = Reflect.apply(put, this, [value, ...keys])
    if (target) request.addEventListener('success', () => { failures++; this.transaction.abort() })
    return request
  })
  if (family === 'mark-failed') vi.spyOn(a.store(), 'reserveActuation').mockRejectedValue(new Error('mark failed'))
  const batches: Array<Record<string, unknown>> = []
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.url
    if (url.includes('voided-tickets')) return Response.json({ ticket_ids: [], next_cursor: null })
    const body = typeof input === 'string' ? JSON.parse(init!.body as string) : await input.clone().json()
    if (url.endsWith('/reconciliations')) {
      batches.push(...body.occurrences)
      return Response.json({ results: body.occurrences.map((row: { occurrence_id: string }) => ({ occurrence_id: row.occurrence_id, result: 'recorded' })) })
    }
    expect(body.direction).toBe('exit')
    if (family === 'plain-error') throw new Error('local failure')
    if (family === 'dom-error') throw new DOMException('local failure', 'InvalidStateError')
    if (family === 'type-error') throw new TypeError('local failure')
    if (family === '401') return new Response('unreadable', { status: 401 })
    if (family === '409-unreadable') return new Response('truncated', { status: 409 })
    if (family === '422-invalid') return Response.json({ decision: 'accepted' }, { status: 422 })
    if (family === '200-invalid') return Response.json({ decision: 'rejected' })
    if (family === 'mark-failed') return Response.json({ decision: 'accepted', scanned_at: '2026-10-19T12:00:01.456Z' })
    if (family === 'status') return new Response('{}', { status: 418 })
    if (['500', '501', '505', '599'].includes(family)) return new Response('{}', { status: Number(family) })
    return Response.json({ decision: 'rejected', reason: family }, { status: 422 })
  }))
  let view = render(<App />)
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'captured-exit' } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
  await waitFor(() => expect(id).not.toBe(''))
  await waitFor(() => expect(a.coordinator.latched(id)?.kind).toBe('terminal'))
  const first = a.coordinator.latched(id)!
  if (first.kind !== 'terminal') throw new Error('terminal required')
  await waitFor(() => expect(screen.getByRole('status', { name: 'Scan result' })).toBeDefined())
  const expected = reasons.includes(family)
    ? `Exit not recordedExit not recorded; reason: ${family === 'invalid_credential' ? 'credential not accepted' : family}. The holder may leave.${['revoked', 'refunded', 'exchanged'].includes(family) ? 'This ticket is no longer valid for entry.' : ''}`
    : family === '401' ? 'Pair this deviceThis attempt was not authorized. Do not admit from this result.Pair this device and scan the ticket again.The holder may leave.'
    : family === '409-unreadable' || family === '422-invalid' ? 'Exit not recordedExit not recorded; reason unavailable. The holder may leave'
    : family === 'mark-failed' ? 'Operator requiredAccess recorded this scan; the scanner did not open; get an operator.The holder may leave.'
    : ['plain-error', 'dom-error', 'type-error'].includes(family) ? 'Operator requiredLocal scanner failure. Scanner did not open. Do not admit from this result; get an operator.Exit result unknown; the holder may leave'
    : ['500', '501', '505', '599'].includes(family) ? 'Operator requiredExit result unknown; the holder may leave'
    : 'Operator requiredScan response unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.Exit result unknown; the holder may leave'
  await waitFor(() => expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe(expected))
  expect(screen.queryByText(ACCEPT_INSTRUCTION)).toBeNull(); expect(screen.queryByText(OFFLINE_INSTRUCTION)).toBeNull()
  const exactTerminal = { ...(initial as object), direction: 'exit', state: 'RESOLVED', localOutcome: first.outcome, decidedAt: first.decidedAt }
  if (reasons.includes(family)) exactTerminal.localOutcome = { ...first.outcome, reason: family } as typeof first.outcome
  if (blocked) {
    await waitFor(() => expect(failures).toBeGreaterThan(0))
    expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(initial)
  } else await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(exactTerminal))
  if (column === 'C') {
    const owner = a.coordinator.ownerId
    forceCloseDatabase(a.store().db as never); await a.waitFor('failed')
    await act(async () => a.storage.retry()); await a.waitFor('ready')
    expect(a.coordinator.ownerId).toBe(owner)
    expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(initial)
    expect(a.coordinator.latched(id)).toBe(first)
  }
  if (column === 'V') {
    blocked = false
    await act(async () => a.store().db.dispatchEvent(new IDBVersionChangeEvent('versionchange', { oldVersion: 3, newVersion: 4 })))
    await a.waitFor('closed')
    expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(exactTerminal)
    await act(async () => a.storage.retry()); await a.waitFor('ready')
  }
  const lost = column === 'P-uncommitted'
  if (column === 'T' || column.startsWith('P-')) {
    view.unmount()
    if (column.startsWith('P-')) a.destroy()
    now = 40000
    const b = createTestPage({ dbName, now: () => now }); await b.ready(); runtime.storage = b.storage
    view = render(<App />)
    if (lost) {
      const recovered = { ...(initial as object), state: 'QUEUED', localOutcome: { family: 'queue', cause: 'uncertain_recovery' } }
      expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(recovered)
      const notice = await screen.findByRole('region', { name: `Recovered scan ${id}` })
      expect(notice.textContent).toContain(`Scan ${id} at ${(initial as { occurredAt: string }).occurredAt}. Type: exit. Recovery cause: uncertain_recovery. Sync state: QUEUED.`)
      expect(notice.textContent).toContain('The prior result and physical passage are unknown. Do not admit from this notice.')
      expect(screen.queryByRole('status', { name: 'Scan result' })).toBeNull()
    } else expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(column === 'T' ? initial : exactTerminal)
  }
  if (!localStorage.getItem('scanner.device-token')) {
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'new-pairing' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
  }
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await waitFor(() => expect(batches.length).toBe(lost ? 2 : 1))
  expect(batches).toContainEqual({ occurrence_id: control.scan.occurrenceId, qr_payload: control.scan.qrPayload, occurred_at: control.scan.occurredAt, event_type: 'entry' })
  if (lost) expect(batches).toContainEqual({ occurrence_id: id, qr_payload: 'captured-exit', occurred_at: (initial as { occurredAt: string }).occurredAt, event_type: 'exit' })
  else expect(batches.map(row => row.occurrence_id)).not.toContain(id)
  blocked = false
  if (!a.destroyed) {
    await waitFor(async () => {
      await act(async () => { await a.coordinator.repairAll() })
      expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual(exactTerminal)
    })
    expect(a.coordinator.latched(id)).toBe(first)
  }
  const rows = await rawGetAll(dbName, STORE.occurrences)
  expect(rows).toHaveLength(2)
  if (lost) await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ state: 'SYNCED', direction: 'exit', result: 'recorded' }))
  view.unmount()
})
