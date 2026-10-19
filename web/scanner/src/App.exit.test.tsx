import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import type { StorageController } from './occurrence-readiness'
import { STORE } from './occurrence-store'
import { readableTime } from './scan-result'
import { createTestPage, destroyAllPages, mintOn, type TestPage } from './test/page-fixture'
import { rawGet, rawPut, rawGetAll } from './test/raw-db'

const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => ({
  ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! },
}))
let page: TestPage
let dbName: string
let requests: Record<string, unknown>[]
let reconciled: Record<string, unknown>[]
let response: () => Promise<Response>
let rejectedAtSync: boolean
let feedStatus: number
let syncStatus: number
beforeEach(async () => {
  dbName = `exit-app-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  requests = []; reconciled = []; rejectedAtSync = false; feedStatus = 200; syncStatus = 200
  localStorage.setItem('scanner.device-token', 'exit-device')
  response = async () => new Response('{}', { status: 503 })
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : input
    if (url.includes('voided-tickets')) return Response.json({ ticket_ids: [], next_cursor: null }, { status: feedStatus })
    const data = input instanceof Request ? await input.clone().json() : JSON.parse(init!.body as string)
    if (url.endsWith('/reconciliations')) {
      reconciled.push(...data.occurrences)
      if (syncStatus === 401) return new Response('unreadable', { status: 401 })
      return Response.json({ results: data.occurrences.map((row: { occurrence_id: string }) => ({
        occurrence_id: row.occurrence_id, result: rejectedAtSync ? 'rejected' : 'recorded',
      })) })
    }
    requests.push(data)
    return response()
  }))
})
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); sessionStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
async function submitExit(credential = 'exit-credential') {
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: credential } })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
}
const result = () => screen.getByRole('status', { name: 'Scan result' })

it('submits the selected Exit, commits one own mark and shows the exact neutral accepted result', async () => {
  const scannedAt = '2026-10-19T12:30:01.456Z'
  response = async () => Response.json({ decision: 'accepted', scanned_at: scannedAt, replay: true })
  render(<App />)
  expect((screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement).checked).toBe(true)
  await submitExit()
  await waitFor(() => expect(result().textContent).toBe(`Exit recordedExit recorded. The holder may leave.Exit recorded at ${readableTime(scannedAt)}.This scan was previously recorded. Exit counted once.`))
  expect(result().className).toContain('neutral')
  expect(requests[0].direction).toBe('exit')
  expect(await rawGet(dbName, STORE.occurrences, requests[0].occurrence_id as string)).toMatchObject({
    direction: 'exit', state: 'ACTUATED', actuated: true, localOutcome: { family: 'actuation', cause: 'reserved' },
  })
})

it.each(['already_redeemed', 'entry_limit_reached', 'exit_required', 'not_inside', 'exit_not_applicable',
  'invalid_credential', 'revoked', 'refunded', 'exchanged', 'exit_unverified', 'integrity_quarantined',
  'integrity_operator_controlled', 'occurrence_required', 'unknown_future_reason', 'invalid_occurrence', 'occurrence_collision'])
('keeps the exact terminal Exit reason %s and neutral leave result', async (reason) => {
  response = async () => Response.json({ decision: 'rejected', reason }, { status: 422 })
  render(<App />); await submitExit()
  const message = reason === 'invalid_credential' ? 'credential not accepted' : reason
  const voided = ['revoked', 'refunded', 'exchanged'].includes(reason) ? 'This ticket is no longer valid for entry.' : ''
  await waitFor(() => expect(result().textContent).toBe(`Exit not recordedExit not recorded; reason: ${message}. The holder may leave.${voided}`))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, requests[0].occurrence_id as string))
    .toMatchObject({ direction: 'exit', state: 'RESOLVED', localOutcome: { cause: 'denied', reason } }))
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([409, 422])('uses exact R17-X for unreadable status %s', async (status) => {
  response = async () => new Response('truncated', { status })
  render(<App />); await submitExit()
  await waitFor(() => expect(result().textContent).toBe('Exit not recordedExit not recorded; reason unavailable. The holder may leave'))
})

it('keeps a queued Exit after switching mode and retires it only as the acknowledged Exit', async () => {
  render(<App />); await submitExit()
  await waitFor(() => expect(result().textContent).toBe('Offline ExitServer response 503.Exit result unknown; the holder may leave'))
  const id = requests[0].occurrence_id as string
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ state: 'QUEUED' }))
  const before = await rawGet(dbName, STORE.occurrences, id) as Record<string, unknown>
  fireEvent.click(screen.getByRole('radio', { name: 'Entry' }))
  expect(screen.queryByRole('status', { name: 'Scan result' })).toBeNull()
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await waitFor(() => expect(reconciled).toStrictEqual([{ occurrence_id: id, qr_payload: 'exit-credential', occurred_at: before.occurredAt, event_type: 'exit' }]))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, id)).toStrictEqual({ ...before, state: 'SYNCED', result: 'recorded' }))
})

it('keeps single-policy reconciliation rejection separate from a recorded Exit', async () => {
  rejectedAtSync = true
  render(<App />); await submitExit()
  await screen.findByRole('heading', { name: 'Offline Exit' })
  const id = requests[0].occurrence_id as string
  await screen.findByRole('button', { name: 'Sync queued scans' })
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText(`Reconciliation rejected for ${id}. This result is not recorded.`)
  expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ direction: 'exit', state: 'RESOLVED', result: 'rejected' })
})

it('never mints, refuses or sends a locally listed Exit', async () => {
  const tid = '1ae64bb1-cd7c-4e69-8248-1a5601acbe98'
  await page.store().mergeRevoked([tid])
  const normal = vi.spyOn(page.store(), 'mintNormal'), refusal = vi.spyOn(page.store(), 'mintRefusal')
  render(<App />); await submitExit(`h.${btoa(JSON.stringify({ tid })).replace(/=+$/, '')}.s`)
  await waitFor(() => expect(result().textContent).toBe('Exit not recordedExit not recorded. This ticket is no longer valid for entry. The holder may leave.'))
  expect(normal).not.toHaveBeenCalled(); expect(refusal).not.toHaveBeenCalled()
  expect(requests).toEqual([]); expect(reconciled).toEqual([])
  expect((await page.store().queued()).entries).toEqual([])
})

const selected = (name: 'Entry' | 'Exit') => (screen.getByRole('radio', { name }) as HTMLInputElement).checked

it.each(['mode', 'storage', 'feed-401', 'sync-401', 'scan-401'] as const)('invalidates held Exit camera detection before %s changes its direction or pairing', async source => {
  const control = await mintOn(page)
  page.coordinator.latch({ kind: 'queue', occurrenceId: control.scan.occurrenceId,
    outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-19T12:00:00.123Z' })
  await page.coordinator.persist(control.scan.occurrenceId)
  let finish!: (codes: Array<{ rawValue: string }>) => void
  const stopped = vi.fn()
  const detect = vi.fn(() => new Promise<Array<{ rawValue: string }>>(resolve => { finish = resolve }))
  vi.stubGlobal('BarcodeDetector', class { detect = detect })
  vi.stubGlobal('MediaStream', class {})
  Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: { getUserMedia: vi.fn(async () => ({ getTracks: () => [{ stop: stopped }] })) } })
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
  render(<App />)
  await screen.findByRole('button', { name: 'Sync queued scans' })
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
  await waitFor(() => expect(detect).toHaveBeenCalledTimes(1))
  if (source === 'mode') fireEvent.click(screen.getByRole('radio', { name: 'Entry' }))
  else if (source === 'storage') {
    localStorage.setItem('scanner.device-token', 'replacement')
    fireEvent(window, new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'replacement' }))
  } else if (source === 'feed-401') {
    feedStatus = 401
    fireEvent.click(screen.getByRole('button', { name: 'Refresh revocation list' }))
  } else if (source === 'sync-401') {
    syncStatus = 401
    fireEvent.click(screen.getByRole('button', { name: 'Sync queued scans' }))
  } else {
    response = async () => new Response('unreadable', { status: 401 })
    await submitExit('auth-exit')
    await waitFor(() => expect(result().textContent).toBe('Pair this deviceThis attempt was not authorized. Do not admit from this result.Pair this device and scan the ticket again.The holder may leave.'))
  }
  await waitFor(() => expect(selected('Entry')).toBe(true))
  expect(stopped).toHaveBeenCalledTimes(1)
  if (source.endsWith('401')) {
    feedStatus = 200; syncStatus = 200
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'repaired-device' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
    await screen.findByLabelText('Ticket credential')
    expect(selected('Entry')).toBe(true)
  }
  const before = await rawGetAll(dbName, STORE.occurrences)
  const sent = requests.length
  await act(async () => { finish([{ rawValue: 'stale-camera-credential' }]); await Promise.resolve() })
  expect(requests).toHaveLength(sent)
  expect(await rawGetAll(dbName, STORE.occurrences)).toStrictEqual(before)
  expect(selected('Entry')).toBe(true)
  expect(screen.queryByText('Point the camera at a ticket QR code.')).toBeNull()
})

it('keeps the captured Exit while busy controls and a forced mode event cannot change it', async () => {
  let answer!: (value: Response) => void
  response = () => new Promise(resolve => { answer = resolve })
  render(<App />); await submitExit()
  await waitFor(() => expect(requests).toHaveLength(1))
  const entry = screen.getByRole('radio', { name: 'Entry' }) as HTMLInputElement
  expect(entry.closest('fieldset')!.disabled).toBe(true)
  fireEvent.click(entry)
  expect(selected('Exit')).toBe(true)
  await act(async () => { answer(Response.json({ decision: 'rejected', reason: 'not_inside' }, { status: 409 })) })
  await waitFor(() => expect(result().textContent).toBe('Exit not recordedExit not recorded; reason: not_inside. The holder may leave.'))
  expect(requests[0].direction).toBe('exit')
})

it('renders corrupt current-row warnings as text even when nothing can sync, then removes an obsolete warning', async () => {
  const { scan, record } = await mintOn(page, { direction: 'exit' })
  await rawPut(dbName, STORE.occurrences, { ...record, direction: '<img src=x onerror=alert(1)>' })
  render(<App />)
  const warning = `Local record ${scan.occurrenceId} cannot be read: record type is not exactly one of entry, exit or refusal. It stays on this device and will not sync.`
  await screen.findByText(warning)
  expect(screen.queryByRole('button', { name: 'Sync queued scans' })).toBeNull()
  expect(reconciled).toEqual([])
  await rawPut(dbName, STORE.occurrences, record)
  fireEvent(window, new Event('online'))
  await waitFor(() => expect(screen.queryByText(warning)).toBeNull())
  expect(reconciled).toEqual([])
})

it('shows actual recovered Exit direction on a second App and after exact sync retirement', async () => {
  const { scan, record } = await mintOn(page, { direction: 'exit' })
  page.destroy()
  const b = createTestPage({ dbName, now: () => Date.now() + 40000 })
  await b.ready(); runtime.storage = b.storage
  let view = render(<App />)
  const expected = (state: string) => `Recovered scanScan ${scan.occurrenceId} at ${record.occurredAt}. Type: exit. Recovery cause: uncertain_recovery. Sync state: ${state}.The prior result and physical passage are unknown. Do not admit from this notice.Acknowledge recovery notice`
  await waitFor(() => expect(screen.getByRole('region', { name: `Recovered scan ${scan.occurrenceId}` }).textContent).toBe(expected('QUEUED')))
  view.unmount()
  const c = createTestPage({ dbName }); await c.ready(); runtime.storage = c.storage
  view = render(<App />)
  await waitFor(() => expect(screen.getByRole('region', { name: `Recovered scan ${scan.occurrenceId}` }).textContent).toBe(expected('QUEUED')))
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await waitFor(() => expect(screen.getByRole('region', { name: `Recovered scan ${scan.occurrenceId}` }).textContent).toBe(expected('SYNCED')))
  expect(reconciled).toStrictEqual([{ occurrence_id: scan.occurrenceId, qr_payload: scan.qrPayload, occurred_at: scan.occurredAt, event_type: 'exit' }])
  expect(screen.queryByRole('status', { name: 'Scan result' })).toBeNull()
})

import { instrumentTransactions } from './test/idb-instrument'
import { forceCloseDatabase } from 'fake-indexeddb'

it('publishes captured Exit storage-unready from the camera start race without any write or request', async () => {
  render(<App />)
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  const normal = vi.spyOn(page.store(), 'mintNormal'), refusal = vi.spyOn(page.store(), 'mintRefusal')
  vi.spyOn(page.storage, 'readyStore').mockReturnValue(null)
  fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
  await waitFor(() => expect(result().textContent).toBe('Exit not recordedScanner storage is not ready. No ticket was checked.Exit not recorded; the holder may leave'))
  expect(normal).not.toHaveBeenCalled(); expect(refusal).not.toHaveBeenCalled(); expect(requests).toEqual([])
  expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
})

it.each(['throw', 'abort'] as const)('publishes exact Exit mint failure after a real %s with no request or recovery fact', async mode => {
  render(<App />)
  const instrument = instrumentTransactions()
  instrument.inject(mode, { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' })
  try {
    await submitExit()
    await waitFor(() => expect(result().textContent).toBe('Exit not recordedScanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.Exit not recorded; the holder may leave'))
    expect(requests).toEqual([])
    expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
    expect((await page.store().queued()).entries).toEqual([])
  } finally { instrument.restore() }
})

it('keeps Exit selected but disabled after actual connection loss, with no delayed presentation replay', async () => {
  render(<App />)
  fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
  forceCloseDatabase(page.store().db as never)
  await page.waitFor('failed')
  await waitFor(() => expect((screen.getByRole('group', { name: 'Scan direction' }) as HTMLFieldSetElement).disabled).toBe(true))
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'not-retained' } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
  await waitFor(() => expect(result().textContent).toBe('Exit not recordedScanner storage is not ready. No ticket was checked.Exit not recorded; the holder may leave'))
  fireEvent.click(await screen.findByRole('button', { name: 'Retry storage' }))
  await page.waitFor('ready')
  expect(selected('Exit')).toBe(true); expect(requests).toEqual([])
  expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
})
