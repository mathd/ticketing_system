import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { createEntryScanner, type EntryResult } from './entry-scan'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { bounded, createTestPage, destroyAllPages, eventually, settle, type TestPage } from './test/page-fixture'
import { rawGet } from './test/raw-db'

let page: TestPage
let dbName: string
let wall: number
let mono: number
let clockThrows: boolean
let results: EntryResult[]
let busy: boolean
let scanner: ReturnType<typeof createEntryScanner>
let timers: Map<number, () => void>
let nextTimer: number
let instrument: Instrument | undefined
let requests: Request[]
let token: string
const accepted = () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T12:00:00Z' }))
const reply = (status: number) => new Response('{}', { status })
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const advance = (ms: number) => { wall = 100000 + ms; mono = 100 + ms }
const wake = () => { const callbacks = [...timers.values()]; timers.clear(); callbacks.forEach((callback) => callback()) }
beforeEach(async () => {
  dbName = `timing-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); await page.ready()
  wall = 100000; mono = 100; clockThrows = false; results = []; busy = false
  timers = new Map(); nextTimer = 0; requests = []; token = 'paired'
  scanner = createEntryScanner({ storage: page.storage, pairing: () => ({ token, generation: 1 }),
    publish: (result) => { results.push(result) }, busy: (value) => { busy = value }, diagnostic: () => undefined,
    authRefused: () => { token = '' },
    clock: { wall: () => { if (clockThrows) throw new Error('clock'); return wall }, monotonic: () => mono },
    scheduler: { set: (callback) => { const id = ++nextTimer; timers.set(id, callback); return id }, clear: (id) => { timers.delete(id as number) } },
  })
})
afterEach(() => { instrument?.restore(); instrument = undefined; destroyAllPages(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
function transport(answer: (request: Request) => Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn((request: Request) => { requests.push(request); return answer(request) }))
}
async function record() {
  const id = (await requests[0].clone().json()).occurrence_id as string
  await page.coordinator.persist(id)
  return rawGet(dbName, STORE.occurrences, id) as Promise<Record<string, unknown>>
}

it.each([2999, 3000])('checks mint completion at %s with timers withheld', async (elapsed) => {
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  transport(async () => reply(503))
  const scan = scanner.submit('credential'); await hold.started
  advance(elapsed); hold.release(); await bounded(scan, 'mint continuation')
  expect(results).toEqual(elapsed === 2999 ? [{ kind: 'queued', status: 503 }] : [{ kind: 'stopped-before-send' }])
  expect(requests).toHaveLength(elapsed === 2999 ? 1 : 0)
})

it.each([502, 503, 504, 'native'] as const)('checks offline %s at each response boundary without timers', async (status) => {
  for (const elapsed of [29999, 30000]) {
    const waiting = deferred<Response>()
    transport(() => waiting.promise)
    const scan = scanner.submit('credential'); await eventually(() => requests.length === 1, 'headers')
    advance(elapsed)
    if (status === 'native') waiting.reject(new TypeError('Failed to fetch'))
    else waiting.resolve(reply(status))
    await bounded(scan, 'offline continuation')
    expect(results.at(-1)).toEqual(elapsed === 29999
      ? status === 'native' ? { kind: 'queued' } : { kind: 'queued', status }
      : { kind: 'sent-unconfirmed' })
    expect((await record()).state).toBe(elapsed === 29999 ? 'QUEUED' : 'RESOLVED')
    requests = []; advance(0)
  }
})

it.each([409, 422, 200, 'headers'] as const)('bounds stalled %s and ignores its late answer', async (status) => {
  const waiting = deferred<Response>(); const body = deferred<unknown>()
  const r = reply(typeof status === 'number' ? status : 200)
  vi.spyOn(r, 'json').mockReturnValue(body.promise)
  transport(() => status === 'headers' ? waiting.promise : Promise.resolve(r))
  const scan = scanner.submit('credential'); await eventually(() => requests.length === 1, 'sent')
  if (status !== 'headers') await eventually(() => vi.mocked(r.json).mock.calls.length === 1, 'body started')
  advance(30000); wake(); await bounded(scan, 'deadline releases submission')
  expect(results).toEqual([{ kind: status === 409 || status === 422 ? 'denial-unreadable' : status === 200 ? 'body-unconfirmed' : 'sent-unconfirmed' }])
  expect(busy).toBe(false)
  waiting.resolve(reply(401)); body.resolve({ decision: 'accepted', scanned_at: '2026-10-18T12:00:00Z' })
  await settle()
  expect(token).toBe('paired'); expect(results).toHaveLength(1)
  expect((await record()).state).toBe('RESOLVED')
  expect((await page.store().queued()).entries).toEqual([])
})

it.each(['deadline', 'cancel'] as const)('keeps the first %s stop while headers never finish', async (first) => {
  transport(() => new Promise(() => undefined))
  const scan = scanner.submit('credential'); await eventually(() => requests.length === 1, 'sent')
  if (first === 'deadline') { advance(30000); wake(); scanner.cancel() }
  else { scanner.cancel(); advance(30000); wake() }
  await bounded(scan, 'stopped request')
  expect(results).toEqual([first === 'deadline' ? { kind: 'sent-unconfirmed' } : { kind: 'cancelled', requestMayHaveBeenSent: true }])
  expect((await record()).localOutcome).toMatchObject({ cause: first === 'deadline' ? 'stopped' : 'cancelled' })
  expect(timers.size).toBe(0)
})

it.each(['commit', 'abort'] as const)('publishes a late mark immediately then keeps actual %s evidence', async (mode) => {
  instrument = instrumentTransactions(); let hold: ReturnType<Instrument['hold']> | undefined
  transport(async () => { hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' }); return accepted() })
  const scan = scanner.submit('credential'); await eventually(() => !!hold, 'mark hold')
  const call = await hold!.started
  await eventually(() => call.events.includes('write-success'), 'mark write succeeds')
  advance(30000); wake(); await bounded(scan, 'mark deadline')
  expect(results).toEqual([{ kind: 'accepted-too-late' }]); expect(busy).toBe(false)
  expect(call.events).not.toContain('complete')
  hold!.release(mode)
  expect((await record()).state).toBe(mode === 'commit' ? 'ACTUATED' : 'RESOLVED')
  expect(results).toHaveLength(1)
})

it('publishes an in-time queue and permits another scan while the first save is held', async () => {
  instrument = instrumentTransactions(); let hold: ReturnType<Instrument['hold']> | undefined
  transport(async () => { if (!hold) hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' }); return reply(503) })
  await bounded(scanner.submit('credential'), 'submission ends before save')
  await hold!.started
  expect(results).toEqual([{ kind: 'queued', status: 503 }]); expect(busy).toBe(false)
  advance(31000)
  const second = scanner.submit('second'); expect(busy).toBe(true)
  hold!.release(); await bounded(second, 'next scan')
  expect(results).toEqual([{ kind: 'queued', status: 503 }, { kind: 'queued', status: 503 }])
  expect(requests).toHaveLength(2)
  expect((await record()).state).toBe('QUEUED')
})

it('re-arms an early wake and catches a clock exception inside the next wake', async () => {
  transport(() => new Promise(() => undefined))
  const scan = scanner.submit('credential'); await eventually(() => requests.length === 1, 'sent')
  advance(100); wake(); expect(results).toEqual([]); expect(timers.size).toBe(1)
  clockThrows = true; wake(); await bounded(scan, 'throwing wake')
  expect(results).toEqual([{ kind: 'local-failure' }]); expect((await record()).state).toBe('RESOLVED')
})

it.each([true, false])('orders a late listed lookup and its deadline wake, listed first %s', async (listedFirst) => {
  const lookup = deferred<boolean>(); vi.spyOn(page.store(), 'isRevoked').mockReturnValue(lookup.promise)
  const qr = `e30.${btoa(JSON.stringify({ tid: '10000000-0000-4000-8000-000000000001' })).replace(/=/g, '')}.signature`
  const scan = scanner.submit(qr); advance(3000)
  if (!listedFirst) wake()
  lookup.resolve(true); await bounded(scan, 'lookup stops'); await settle()
  expect(results).toEqual([listedFirst ? { kind: 'revocation-audit-not-saved' } : { kind: 'stopped-before-send' }])
  expect((await page.store().queued()).entries).toEqual([])
})

it.each(['start', 'lookup', 'mint', 'headers', 'denial', 'body', 'mark'] as const)('catches clock reader failure at %s', async (phase) => {
  let hold: ReturnType<Instrument['hold']> | undefined
  const lookup = deferred<boolean>()
  const body = deferred<unknown>()
  let bodyStarted = false
  const r = phase === 'denial' ? reply(409) : accepted()
  if (phase === 'denial' || phase === 'body') vi.spyOn(r, 'json').mockImplementation(() => { bodyStarted = true; return body.promise })
  if (phase === 'mint' || phase === 'mark') instrument = instrumentTransactions()
  if (phase === 'mint') hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  if (phase === 'lookup') vi.spyOn(page.store(), 'isRevoked').mockReturnValue(lookup.promise)
  transport(async () => {
    if (phase === 'headers') return new Promise(() => undefined)
    if (phase === 'mark') hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
    return r
  })
  if (phase === 'start') clockThrows = true
  const qr = `e30.${btoa(JSON.stringify({ tid: '10000000-0000-4000-8000-000000000001' })).replace(/=/g, '')}.signature`
  const scan = scanner.submit(phase === 'lookup' ? qr : 'credential')
  if (phase !== 'start') {
    if (phase === 'lookup') await eventually(() => timers.size > 0, 'lookup wait')
    else if (phase === 'mint' || phase === 'mark') {
      await eventually(() => !!hold, 'transaction hold')
      await hold!.started
    } else if (phase === 'denial' || phase === 'body') await eventually(() => bodyStarted, 'body wait')
    else await eventually(() => requests.length === 1, 'header wait')
    clockThrows = true; wake()
  }
  await bounded(scan, 'reader failure ends submission')
  expect(results).toEqual([{ kind: phase === 'denial' ? 'denial-unreadable' : phase === 'body' ? 'body-unconfirmed' : phase === 'mark' ? 'mark-failed' : 'local-failure' }])
  hold?.release('abort'); lookup.resolve(false); body.resolve({})
  if (requests.length) expect((await record()).state).toBe('RESOLVED')
})

it.each(['clock', 'invalid', 'late'] as const)('preserves known revocation through a %s after mint', async (fault) => {
  const ticket = '10000000-0000-4000-8000-000000000001'
  const qr = `e30.${btoa(JSON.stringify({ tid: ticket })).replace(/=/g, '')}.signature`
  await page.store().mergeRevoked([ticket])
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  const scan = scanner.submit(qr); await hold.started
  if (fault === 'clock') clockThrows = true
  else if (fault === 'invalid') wall = NaN
  else advance(3000)
  wake(); await bounded(scan, 'refusal deadline')
  expect(results).toEqual([{ kind: 'revocation-audit-not-saved' }])
  hold.release()
  await page.coordinator.repairAll()
  const ids = page.coordinator.pendingTargets()
  expect(ids).toEqual([])
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([29999, 30000])('checks a valid accepted body at %s before starting the mark', async (elapsed) => {
  const mark = vi.spyOn(page.store(), 'reserveActuation')
  const body = deferred<unknown>(); const r = accepted()
  vi.spyOn(r, 'json').mockReturnValue(body.promise)
  transport(async () => r)
  const scan = scanner.submit('credential'); await eventually(() => vi.mocked(r.json).mock.calls.length === 1, 'accepted body')
  advance(elapsed); body.resolve({ decision: 'accepted', scanned_at: '2026-10-18T12:00:00Z' })
  await bounded(scan, 'accepted boundary')
  expect(mark).toHaveBeenCalledTimes(elapsed === 29999 ? 1 : 0)
  expect(results[0].kind).toBe(elapsed === 29999 ? 'accepted' : 'accepted-too-late')
  expect((await record()).state).toBe(elapsed === 29999 ? 'ACTUATED' : 'RESOLVED')
})

it('refuses a mark that completes at the limit even when no timer wakes', async () => {
  instrument = instrumentTransactions(); let hold: ReturnType<Instrument['hold']> | undefined
  transport(async () => { hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' }); return accepted() })
  const scan = scanner.submit('credential'); await eventually(() => !!hold, 'mark barrier')
  const call = await hold!.started
  await eventually(() => call.events.includes('write-success'), 'ACT write succeeds')
  advance(30000); hold!.release(); await bounded(scan, 'late mark continuation')
  expect(results).toEqual([{ kind: 'accepted-too-late' }])
  expect((await record()).state).toBe('ACTUATED')
})


it('checks a late unlisted lookup before registration and mint', async () => {
  const lookup = deferred<boolean>()
  vi.spyOn(page.store(), 'isRevoked').mockReturnValue(lookup.promise)
  const register = vi.spyOn(page.coordinator, 'register')
  const mint = vi.spyOn(page.store(), 'mintNormal')
  const qr = `e30.${btoa(JSON.stringify({ tid: '10000000-0000-4000-8000-000000000001' })).replace(/=/g, '')}.signature`
  const scan = scanner.submit(qr)
  advance(3000); lookup.resolve(false); await bounded(scan, 'late lookup continuation')
  expect(register).not.toHaveBeenCalled(); expect(mint).not.toHaveBeenCalled()
  expect(results).toEqual([{ kind: 'stopped-before-send' }])
})

it.each(['commit', 'abort'] as const)('repairs a mint that settles %s after the deadline and version change', async (mode) => {
  const register = vi.spyOn(page.coordinator, 'register')
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  const scan = scanner.submit('credential'); const call = await hold.started
  await eventually(() => call.events.includes('write-success'), 'mint write succeeds')
  const id = register.mock.calls[0][0].occurrenceId
  advance(3000); wake(); await bounded(scan, 'mint stopped')
  const db = page.store().db
  db.dispatchEvent(new IDBVersionChangeEvent('versionchange', { oldVersion: 3, newVersion: 4 }))
  hold.release(mode); await page.waitFor('closed')
  page.storage.retry(); await page.waitFor('ready')
  await page.coordinator.repairAll()
  const row = await rawGet(dbName, STORE.occurrences, id)
  if (mode === 'commit') expect(row).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'stopped' } })
  else expect(row).toBeUndefined()
  expect(results).toEqual([{ kind: 'stopped-before-send' }]); expect(requests).toEqual([])
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([502, 'native'] as const)('refuses a clock exception immediately before offline %s authority', async (status) => {
  const pending = deferred<Response>()
  transport(() => pending.promise)
  const scan = scanner.submit('credential'); await eventually(() => requests.length === 1, 'fetch wait')
  clockThrows = true
  if (status === 'native') pending.reject(new TypeError('Failed to fetch'))
  else pending.resolve(reply(status))
  await bounded(scan, 'clock fault in continuation')
  expect(results).toEqual([{ kind: 'local-failure' }])
  expect((await record()).state).toBe('RESOLVED')
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([2999, 3000])('checks a refusal mint at %s even with its timer withheld', async (elapsed) => {
  const ticket = '10000000-0000-4000-8000-000000000001'
  const qr = `e30.${btoa(JSON.stringify({ tid: ticket })).replace(/=/g, '')}.signature`
  await page.store().mergeRevoked([ticket])
  const register = vi.spyOn(page.coordinator, 'register')
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  const scan = scanner.submit(qr); await hold.started
  advance(elapsed); hold.release(); await bounded(scan, 'refusal mint boundary')
  const id = register.mock.calls[0][0].occurrenceId
  await page.coordinator.persist(id)
  expect(results).toEqual([{ kind: elapsed === 2999 ? 'revocation-refused' : 'revocation-audit-not-saved' }])
  expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ state: elapsed === 2999 ? 'QUEUED' : 'RESOLVED' })
  expect((await page.store().queued()).entries).toHaveLength(elapsed === 2999 ? 1 : 0)
})

import { forceCloseDatabase } from 'fake-indexeddb'

it.each(['normal', 'refusal'] as const)('keeps a late %s mint terminal through failed repair and connection replacement', async (type) => {
  const ticket = '10000000-0000-4000-8000-000000000001'
  const qr = type === 'normal' ? 'credential' : `e30.${btoa(JSON.stringify({ tid: ticket })).replace(/=/g, '')}.signature`
  if (type === 'refusal') await page.store().mergeRevoked([ticket])
  const register = vi.spyOn(page.coordinator, 'register')
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  const scan = scanner.submit(qr); const call = await hold.started
  await eventually(() => call.events.includes('write-success'), 'late mint write')
  const id = register.mock.calls[0][0].occurrenceId
  advance(3000); wake(); await bounded(scan, 'late mint stops')
  instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
  hold.release(); expect((await page.coordinator.persist(id)).durable).toBe('failed')
  expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ state: 'PENDING' })
  expect((await page.store().queued()).entries).toEqual([])
  forceCloseDatabase(page.store().db as never); await page.waitFor('failed')
  page.storage.retry(); await page.waitFor('ready')
  expect(await rawGet(dbName, STORE.occurrences, id)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'stopped' } })
  expect((await page.store().queued()).entries).toEqual([])
  expect(results).toEqual([{ kind: type === 'refusal' ? 'revocation-audit-not-saved' : 'stopped-before-send' }])
})
