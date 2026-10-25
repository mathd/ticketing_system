import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { StorageController } from './occurrence-readiness'
import App from './App'
import { createTestPage, destroyAllPages, mintOn, type TestPage } from './test/page-fixture'
import { rawGet, rawPut, openRaw } from './test/raw-db'
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
async function scan(direction: 'entry' | 'exit' = 'entry') {
  fireEvent.click(screen.getByRole('radio', { name: direction === 'entry' ? 'Entry' : 'Exit' }))
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

describe.each(['entry', 'exit'] as const)('%s actuation and replay guards', direction => {
const cases = ['own ACT', 'foreign ACT', 'foreign PEND', 'missing', 'QUEUED', 'RESOLVED', 'ACT without reservation'] as const
it.each(cases.flatMap(state => [true, false, undefined].map(replay => ({ state, replay }))))('checks replay=$replay against actual $state before displaying a result', async ({ state, replay }) => {
  let before: unknown
  scanReply = async () => {
    const id = scans[0].occurrence_id as string
    const original = await rawGet(dbName, STORE.occurrences, id) as Record<string, unknown>
    if (state === 'missing') {
      const db = await openRaw(dbName)
      await new Promise<void>((resolve, reject) => { const tx = db.transaction(STORE.occurrences, 'readwrite'); tx.objectStore(STORE.occurrences).delete(id); tx.oncomplete = () => resolve(); tx.onabort = () => reject(tx.error) })
      db.close()
    } else {
      const row = { ...original }
      if (state.includes('ACT')) Object.assign(row, { state: 'ACTUATED', actuated: state !== 'ACT without reservation', decidedAt: '2026-10-18T08:00:00.123Z', localOutcome: { family: 'actuation', cause: 'reserved' } })
      if (state.startsWith('foreign')) row.ownerId = 'foreign-owner'
      if (state === 'QUEUED') Object.assign(row, { state: 'QUEUED', localOutcome: { family: 'queue', cause: 'transport_failure' }, decidedAt: '2026-10-18T08:00:00.123Z' })
      if (state === 'RESOLVED') Object.assign(row, { state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'unconfirmed_server_error', status: 500 }, decidedAt: '2026-10-18T08:00:00.123Z' })
      await rawPut(dbName, STORE.occurrences, row)
    }
    before = await rawGet(dbName, STORE.occurrences, id)
    return new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T09:00:01.456Z', ...(replay === undefined ? {} : { replay }) }), { status: 200 })
  }
  render(<App />); await scan(direction)
  const duplicate = state === 'own ACT' && replay === true
  await screen.findByRole('heading', { name: duplicate ? 'Already processed' : 'Operator required' })
  expect(screen.getByRole(direction === 'entry' ? 'alert' : 'status', { name: 'Scan result' }).textContent).toBe(duplicate
    ? direction === 'entry' ? 'Already processedThis scan was already handled on this device. No second entry.' : 'Already processedThis scan was already handled on this device. No second Exit action. The holder may leave.'
    : `Operator requiredAccess recorded this scan; the scanner did not open; get an operator.${direction === 'exit' ? 'The holder may leave.' : ''}`)
  await act(async () => { await page.coordinator.repairAll() })
  expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toStrictEqual(before)
})

it.each([true, false, undefined])('admits a fresh durable reservation with replay=%s', async replay => {
  scanReply = async () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T09:00:01.456Z', ...(replay === undefined ? {} : { replay }) }), { status: 200 })
  render(<App />); await scan(direction)
  await screen.findByRole('heading', { name: direction === 'entry' ? 'Accepted' : 'Exit recorded' })
  const row = await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)
  expect(row).toMatchObject({ state: 'ACTUATED', actuated: true, ownerId: page.coordinator.ownerId, direction, localOutcome: { family: 'actuation', cause: 'reserved' } })
})

})

import { instrumentTransactions } from './test/idb-instrument'
it('retains a failed queue latch through pairing cancellation and repairs only that queue', async () => {
  render(<App />)
  const instrument = instrumentTransactions()
  let holding: ReturnType<typeof instrument.hold> | undefined
  scanReply = async () => { holding = instrument.hold({ mode: 'readwrite', durability: 'strict' }); return new Response('', { status: 503 }) }
  try {
    await scan(); await screen.findByRole('heading', { name: 'Offline decision' })
    expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe('Offline decisionServer response 503.Admit per venue offline policy.')
    await waitFor(() => expect(holding).toBeDefined())
    const call = await holding!.started
    await waitFor(() => expect(call.events).toContain('write-success'))
    const first = page.coordinator.latched(scans[0].occurrence_id as string)
    await act(async () => { localStorage.setItem('scanner.device-token', 'token-b'); window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'token-b' })) })
    expect(page.coordinator.latched(scans[0].occurrence_id as string)).toBe(first)
    const write = page.store().persistDecision.bind(page.store())
    let failArmed = true
    vi.spyOn(page.store(), 'persistDecision').mockImplementation(async decision => {
      if (failArmed) instrument.inject('throw', { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' })
      return write(decision)
    })
    holding!.release('abort')
    await waitFor(() => expect(page.coordinator.repairWarnings()).toHaveLength(1))
    failArmed = false
    fireEvent.click(await screen.findByRole('button', { name: 'Retry storage' }))
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'QUEUED', localOutcome: { family: 'queue', cause: 'server_unavailable', status: 503 } }))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 1 offline scan(s).')
    expect(batches.flat().map(row => row.occurrence_id)).toEqual([scans[0].occurrence_id])
    expect(screen.queryByRole('heading', { name: 'Scan cancelled' })).toBeNull()
  } finally { holding?.release(); instrument.restore() }
})

it.each(['throw', 'abort'] as const)('retains failed cancellation after %s through a late denial and sends only a queued control', async (failure) => {
  const control = await queueControl()
  let respond!: (response: Response) => void
  scanReply = () => new Promise(resolve => { respond = resolve })
  render(<App />)
  const instrument = instrumentTransactions()
  let failArmed=true
  const write=page.store().persistDecision.bind(page.store())
  vi.spyOn(page.store(),'persistDecision').mockImplementation(async decision=>{if(failArmed)instrument.inject(failure,{stores:[STORE.occurrences],mode:'readwrite',durability:'strict'});return write(decision)})
  try {
    await scan(); await waitFor(() => expect(scans).toHaveLength(1))
    await act(async () => { localStorage.setItem('scanner.device-token', 'token-b'); window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'token-b' })) })
    await waitFor(() => expect(page.coordinator.repairWarnings()).toHaveLength(1))
    const first = page.coordinator.latched(scans[0].occurrence_id as string)
    await act(async () => { respond(new Response(JSON.stringify({ decision: 'rejected', reason: 'refunded' }), { status: 409 })) })
    expect(page.coordinator.latched(scans[0].occurrence_id as string)).toBe(first)
    failArmed=false
    fireEvent.click(await screen.findByRole('button', { name: 'Retry storage' }))
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: true } }))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 1 offline scan(s).')
    expect(batches.flat().map(row => row.occurrence_id)).toEqual([control.occurrenceId])
    expect(screen.queryByRole('heading', { name: 'Rejected' })).toBeNull()
  } finally { failArmed=false;instrument.restore() }
})

it.each(['throw', 'abort'] as const)('keeps O02 after an actual %s failure before pairing cancellation settles the scan', async (failure) => {
  const instrument = instrumentTransactions()
  let failed!: () => void, release!: () => void
  const failureReached = new Promise<void>(resolve => { failed = resolve })
  const continuation = new Promise<void>(resolve => { release = resolve })
  const write = page.store().persistDecision.bind(page.store())
  let once = false, failArmed = true
  vi.spyOn(page.store(), 'persistDecision').mockImplementation(async decision => {
    if(failArmed)instrument.inject(failure,{stores:[STORE.occurrences],mode:'readwrite',durability:'strict'})
    try { return await write(decision) } catch (error) {
      if (!once) { once = true; failed(); await continuation }
      throw error
    }
  })
  scanReply = async () => new Response('', {status:503})
  try {
    render(<App />); await scan(); await failureReached
    const first = page.coordinator.latched(scans[0].occurrence_id as string)
    await screen.findByRole('status', { name: 'Scan result' })
    expect(screen.getByRole('status', { name: 'Scan result' }).textContent).toBe('Offline decisionServer response 503.Admit per venue offline policy.')
    await act(async () => { localStorage.setItem('scanner.device-token', 'token-b'); window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'token-b' })) })
    expect(page.coordinator.latched(scans[0].occurrence_id as string)).toBe(first)
    await act(async () => { release() })
    await waitFor(() => expect(page.coordinator.repairWarnings()).toHaveLength(1))
    failArmed=false
    fireEvent.click(await screen.findByRole('button', { name: 'Retry storage' }))
    await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'QUEUED', localOutcome: { family: 'queue', cause: 'server_unavailable', status: 503 }, decidedAt: first!.decidedAt }))
    fireEvent.click(await screen.findByRole('button', { name: 'Sync queued scans' }))
    await screen.findByText('Synced 1 offline scan(s).')
    expect(batches.flat().map(row => row.occurrence_id)).toEqual([scans[0].occurrence_id])
  } finally { failArmed=false;release(); instrument.restore() }
})

it.each([409, 422].flatMap(status => ['held', 'throw', 'abort'].map(mode => ({status, mode}))))('keeps denial $status across late body and $mode persistence', async ({status, mode}) => {
  const instrument = instrumentTransactions()
  let body!: (value: unknown) => void
  let holding: ReturnType<typeof instrument.hold> | undefined
  const store = page.store(), persist = store.persistDecision.bind(store)
  let attempted!: () => void
  const reached = new Promise<void>(resolve => { attempted = resolve })
  vi.spyOn(store, 'persistDecision').mockImplementation(async decision => {
    try { return await persist(decision) } finally { attempted() }
  })
  scanReply = async () => {
    if (mode === 'held') holding = instrument.hold({stores:[STORE.occurrences], mode:'readwrite', durability:'strict'})
    else instrument.inject(mode as 'throw' | 'abort', {stores:[STORE.occurrences], mode:'readwrite', durability:'strict'})
    return {status, json: () => new Promise(resolve => { body = resolve })} as Response
  }
  try {
    render(<App />); await scan(); await waitFor(() => expect(body).toBeDefined())
    const id = scans[0].occurrence_id as string, first = page.coordinator.latched(id)!
    if(first.kind==='actuation')throw new Error('expected denial')
    expect(first).toMatchObject({kind:'terminal',outcome:{cause:'denied',status,reason:null}})
    if (mode === 'held') await holding!.started
    else await reached
    await act(async () => { body({decision:'rejected',reason:'refunded'}) })
    await screen.findByRole('heading',{name:'Rejected'})
    expect(screen.getByRole('alert',{name:'Scan result'}).textContent).toBe('RejectedDo not admit. This ticket was voided by a refund. Get an operator.Reason: refunded.')
    expect(page.coordinator.latched(id)).toBe(first)
    holding?.release()
    await act(async () => { await page.coordinator.repairAll() })
    await waitFor(async () => {
      await page.coordinator.repairAll()
      expect(await rawGet(dbName,STORE.occurrences,id)).toMatchObject({state:'RESOLVED',decidedAt:first.decidedAt,localOutcome:{...first.outcome,reason:'refunded'}})
    })
  } finally { holding?.release(); instrument.restore() }
})

it.each([409,422].flatMap(status => ['held','throw','abort'].map(mode => ({status,mode}))))('publishes denial $status before $mode reason enrichment settles', async ({status,mode}) => {
  const instrument=instrumentTransactions(),store=page.store(),enrich=store.enrichDenial.bind(store)
  let original: Record<string,unknown> | undefined,holding:ReturnType<typeof instrument.hold>|undefined,once=false,attempted!:()=>void
  const reached=new Promise<void>(resolve=>{attempted=resolve})
  vi.spyOn(store,'enrichDenial').mockImplementation(async(decision,reason)=>{
    if(!once){
      once=true;original=await rawGet(dbName,STORE.occurrences,decision.occurrenceId) as Record<string,unknown>
      if(mode==='held')holding=instrument.hold({stores:[STORE.occurrences],mode:'readwrite',durability:'strict'})
      else instrument.inject(mode as 'throw'|'abort',{stores:[STORE.occurrences],mode:'readwrite',durability:'strict'})
    }
    try{return await enrich(decision,reason)}finally{attempted()}
  })
  scanReply=async()=>new Response(JSON.stringify({decision:'rejected',reason:'refunded'}),{status})
  try{
    render(<App/>);await scan();await screen.findByRole('heading',{name:'Rejected'})
    await waitFor(()=>expect(original).toBeDefined())
    const id=scans[0].occurrence_id as string,first=page.coordinator.latched(id)
    expect(original).toMatchObject({state:'RESOLVED',localOutcome:{cause:'denied',reason:null,status}})
    expect(screen.getByRole('alert',{name:'Scan result'}).textContent).toBe('RejectedDo not admit. This ticket was voided by a refund. Get an operator.Reason: refunded.')
    if(mode==='held'){const call=await holding!.started;await waitFor(()=>expect(call.events).toContain('write-success'));expect(call.events).not.toContain('complete')}
    else{await reached;expect(await rawGet(dbName,STORE.occurrences,id)).toStrictEqual(original)}
    holding?.release()
    await waitFor(async()=>{await page.coordinator.repairAll();expect(await rawGet(dbName,STORE.occurrences,id)).toStrictEqual({...original,localOutcome:{...original!.localOutcome as object,reason:'refunded'}})})
    expect(page.coordinator.latched(id)).toBe(first)
  }finally{holding?.release();instrument.restore()}
})
