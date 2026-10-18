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


import { instrumentTransactions } from './test/idb-instrument'
import { forceCloseDatabase } from 'fake-indexeddb'

it.each(['commit', 'abort'] as const)('does not send before mint completion, then observes %s', async (finish) => {
  render(<App />)
  const instrument = instrumentTransactions()
  const held = instrument.hold({ mode: 'readwrite', durability: 'strict' })
  try {
    await scan()
    const call = await held.started
    await waitFor(() => expect(call.events).toContain('write-success'))
    expect(scans).toEqual([])
    expect(screen.getByText('Checking...')).toBeDefined()
    expect(screen.queryByRole('region', { name: 'Scan result' })).toBeNull()
    held.release(finish)
    if (finish === 'commit') await screen.findByRole('heading', { name: 'Operator required' })
    else await screen.findByText('Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.')
    expect(scans).toHaveLength(finish === 'commit' ? 1 : 0)
  } finally { held.release(); instrument.restore() }
})

it.each(['ordinary', 'versionchange', 'unexpected close'] as const)('opens only after the actual mark completes through %s', async (event) => {
  render(<App />)
  const instrument = instrumentTransactions()
  const holding: ReturnType<typeof instrument.hold>[] = []
  scanReply = async () => {
    holding.push(instrument.hold({ mode: 'readwrite', durability: 'strict' }))
    return new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T09:00:01.456Z' }), { status: 200 })
  }
  try {
    await scan(); await waitFor(() => expect(holding).toHaveLength(1))
    const call = await holding[0].started
    await waitFor(() => expect(call.events).toContain('write-success'))
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    const db = page.store().db
    if (event === 'versionchange') db.dispatchEvent(new IDBVersionChangeEvent('versionchange', { oldVersion: 3, newVersion: 4 }))
    if (event === 'unexpected close') forceCloseDatabase(db as never)
    holding[0].release()
    await screen.findByRole('heading', { name: 'Accepted' })
    expect(call.events).toContain('complete')
    expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'ACTUATED', actuated: true })
    expect(screen.getAllByText('Entry accepted. Admit.')).toHaveLength(1)
  } finally { holding.forEach((held) => held.release()); instrument.restore() }
})

it.each(['commit', 'abort'] as const)('settles pairing cancellation against a held mark %s without a second instruction', async (finish) => {
  render(<App />)
  const instrument = instrumentTransactions()
  const holding: ReturnType<typeof instrument.hold>[] = []
  scanReply = async () => {
    holding.push(instrument.hold({ mode: 'readwrite', durability: 'strict' }))
    return new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T09:00:01.456Z' }), { status: 200 })
  }
  let terminalWrites = 0
  const put = IDBObjectStore.prototype.put
  const spy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value: unknown, ...keys: [key?: IDBValidKey]) {
    if ((value as { localOutcome?: { family: string } }).localOutcome?.family === 'terminal') terminalWrites++
    return Reflect.apply(put, this, [value, ...keys])
  })
  try {
    await scan(); await waitFor(() => expect(holding).toHaveLength(1))
    const call = await holding[0].started
    await waitFor(() => expect(call.events).toContain('write-success'))
    await act(async () => {
      localStorage.setItem('scanner.device-token', 'token-b')
      window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'token-b' }))
    })
    expect(terminalWrites).toBe(0)
    holding[0].release(finish)
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: finish === 'commit' ? 'ACTUATED' : 'RESOLVED' }))
    await act(async () => { await page.coordinator.repairAll() })
    expect(page.coordinator.view(scans[0].occurrence_id as string)?.durable).toBe(finish === 'commit' ? 'settled_as_actuation' : 'committed')
    expect(terminalWrites).toBe(finish === 'commit' ? 0 : 1)
    expect(screen.queryByRole('heading', { name: 'Accepted' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Scan cancelled' })).toBeNull()
    expect(screen.queryByText('The outcome write needs repair. Keep this page open and retry storage.')).toBeNull()
  } finally { holding.forEach((held) => held.release()); spy.mockRestore(); instrument.restore() }
})

it.each([409, 422])('reads denial %s body and publishes its memory result before TERM completes', async (status) => {
  render(<App />)
  const instrument = instrumentTransactions()
  const holding: ReturnType<typeof instrument.hold>[] = []
  scanReply = async () => {
    holding.push(instrument.hold({ mode: 'readwrite', durability: 'strict' }))
    return new Response(JSON.stringify({ decision: 'rejected', reason: 'refunded' }), { status })
  }
  try {
    await scan(); await waitFor(() => expect(holding).toHaveLength(1))
    const call = await holding[0].started
    await screen.findByRole('heading', { name: 'Rejected' })
    expect(call.events).not.toContain('complete')
    expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe('RejectedCredential is invalid or cannot be redeemed.')
    holding[0].release()
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'denied', reason: 'refunded' } }))
  } finally { holding.forEach((held) => held.release()); instrument.restore() }
})

it.each([409, 422])('commits denial %s while its body is stalled and never sends it on remount sync', async (status) => {
  const control = await queueControl()
  let finish!: (body: unknown) => void
  const r = new Response('', { status })
  vi.spyOn(r, 'json').mockImplementation(() => new Promise((resolve) => { finish = resolve }))
  scanReply = async () => r
  const view = render(<App />); await scan()
  await waitFor(() => expect(scans).toHaveLength(1))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'denied', reason: null } }))
  expect(screen.getByText('Checking...')).toBeDefined()
  view.unmount(); render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
  await screen.findByText('Synced 1 offline scan(s).')
  expect(batches.flat().map((row) => row.occurrence_id)).toEqual([control.occurrenceId])
  await act(async () => { finish({ decision: 'rejected', reason: 'refunded' }) })
  expect(screen.queryByRole('heading', { name: 'Rejected' })).toBeNull()
})

it('queue-latch-no-storage-api at the actual App classification boundary', async () => {
  render(<App />)
  const events: string[] = []
  let armed = false
  scanReply = async () => { armed = true; return new Response('', { status: 503 }) }
  const latch = page.coordinator.latch.bind(page.coordinator)
  vi.spyOn(page.coordinator, 'latch').mockImplementation((proposal) => { if (armed) events.push('latch'); return latch(proposal) })
  const api=page.store() as unknown as Record<string,(...args:unknown[])=>unknown>
  for(const [name,method] of Object.entries(api)){
    if(typeof method!=='function')continue
    vi.spyOn(api,name).mockImplementation((...args)=>{if(armed)events.push(name);return Reflect.apply(method,api,args)})
  }
  const factory=indexedDB as unknown as Record<string,(...args:unknown[])=>unknown>
  for(const name of ['open','deleteDatabase','databases']){
    if(typeof factory[name]!=='function')continue
    const method=factory[name]
    vi.spyOn(factory,name).mockImplementation((...args)=>{if(armed)events.push(name);return Reflect.apply(method,indexedDB,args)})
  }
  const tx = IDBDatabase.prototype.transaction
  vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementation(function (this: IDBDatabase, ...args: Parameters<typeof tx>) {
    if (armed) events.push('transaction')
    return Reflect.apply(tx, this, args)
  })
  const get = IDBObjectStore.prototype.get
  vi.spyOn(IDBObjectStore.prototype, 'get').mockImplementation(function (this: IDBObjectStore, ...args: Parameters<typeof get>) {
    if (armed) events.push('get')
    return Reflect.apply(get, this, args)
  })
  await scan(); await screen.findByRole('heading', { name: 'Offline decision' })
  expect(events[0]).toBe('latch')
})
