import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { createEntryScanner, type EntryResult, type Pairing } from './entry-scan'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { createTestPage, destroyAllPages, eventually, type TestPage } from './test/page-fixture'
import { rawGet, rawPut } from './test/raw-db'

let page: TestPage
let pairing: Pairing
let results: EntryResult[]
let scanner: ReturnType<typeof createEntryScanner>
let instrument: Instrument | undefined
let dbName: string
let sent: Record<string, unknown>[]
let onPublish: () => void
const TIME = '2026-10-18T09:00:00.123Z'
const ACCEPTED = '2026-10-18T09:00:01.456Z'
beforeEach(async () => {
  dbName = `entry-${crypto.randomUUID()}`
  page = createTestPage({ dbName })
  await page.ready()
  pairing = { token: 'token-a', generation: 1 }
  results = []
  sent = []
  onPublish = () => undefined
  scanner = createEntryScanner({ storage: page.storage, pairing: () => pairing,
    publish: (result) => { onPublish(); results.push(result) }, busy: () => undefined, diagnostic: () => undefined,
    authRefused: () => { pairing = { token: '', generation: pairing.generation + 1 } },
  })
})
afterEach(() => { instrument?.restore(); instrument = undefined; destroyAllPages(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
function answer(response: Response | (() => Promise<Response>), onResponse: () => void = () => undefined) {
  return vi.stubGlobal('fetch', vi.fn(async (request: Request) => {
    expect(request.redirect).toBe('manual')
    expect(request.headers.get('X-Scanner-Token')).toBe('token-a')
    const body = await request.clone().json()
    sent.push(body)
    const row = await rawGet(dbName, STORE.occurrences, body.occurrence_id) as Record<string, unknown>
    expect(row.state).toBe('PENDING')
    expect(body).toStrictEqual({ qr_payload: 'credential', occurrence_id: body.occurrence_id,
      occurred_at: row.occurredAt, direction: 'entry' })
    onResponse()
    return typeof response === 'function' ? response() : response
  }))
}
const response = (status: number, body: unknown = {}) => new Response(JSON.stringify(body), { status })
async function decisionWriteSettled() {
  await eventually(() => {
    const durable = page.coordinator.view(sent[0].occurrence_id as string)?.durable
    return durable === 'committed' || durable === 'settled_as_actuation'
  }, 'production decision write settled')
}
async function row() {
  expect(sent).toHaveLength(1)
  return rawGet(dbName, STORE.occurrences, sent[0].occurrence_id as string) as Promise<Record<string, unknown>>
}

it.each([401, 500, 501, 505, 599, 201, 204, 301, 400, 403, 404, 429])('keeps status %s terminal without trusting its body', async (status) => {
  const r = status === 204 ? new Response(null, { status }) : response(status)
  const json = vi.spyOn(r, 'json').mockRejectedValue(new Error('unreadable'))
  answer(r)
  await scanner.submit(' credential ')
  expect(json).not.toHaveBeenCalled()
  expect(results).toHaveLength(1)
  expect(results[0].kind).toBe(status === 401 ? 'auth-refused' : status >= 500 ? 'server-error' : 'status-unconfirmed')
  await decisionWriteSettled()
  expect((await row()).state).toBe('RESOLVED')
  expect((await page.store().queued()).entries).toEqual([])
  expect(pairing.token).toBe(status === 401 ? '' : 'token-a')
})

it.each([502, 503, 504])('publishes status %s offline permission before storage calls', async (status) => {
  const r = response(status)
  const json = vi.spyOn(r, 'json').mockRejectedValue(new Error('not a permission source'))
  let armed = false
  answer(r, () => { armed = true })
  const events: string[] = []
  const latch = page.coordinator.latch.bind(page.coordinator)
  vi.spyOn(page.coordinator, 'latch').mockImplementation((proposal) => { events.push('latch'); return latch(proposal) })
  for (const method of ['get', 'put', 'add'] as const) {
    const original = IDBObjectStore.prototype[method]
    vi.spyOn(IDBObjectStore.prototype, method).mockImplementation(function (this: IDBObjectStore, ...args: unknown[]) {
      if (armed) events.push(method)
      return Reflect.apply(original, this, args)
    })
  }
  for (const [target, method] of [[IDBFactory.prototype, 'open'], [IDBFactory.prototype, 'deleteDatabase'],
    [IDBDatabase.prototype, 'transaction']] as const) {
    const original = Reflect.get(target, method)
    const spyOn = vi.spyOn as unknown as (object: object, method: string) => { mockImplementation(fn: (...args: unknown[]) => unknown): unknown }
    spyOn(target, method).mockImplementation(function (this: unknown, ...args: unknown[]) {
      if (armed) events.push(method)
      return Reflect.apply(original, this, args)
    })
  }
  onPublish = () => { events.push('publish') }
  await scanner.submit('credential')
  expect(events.slice(0, 2)).toEqual(['latch', 'publish'])
  expect(results).toEqual([{ kind: 'queued', status }])
  expect(json).not.toHaveBeenCalled()
  expect((await row()).state).toBe('QUEUED')
})

it.each([new TypeError('Failed to fetch'), new TypeError('network down'), new Error('Failed to fetch'), new DOMException('Failed to fetch', 'NetworkError')])('limits no-response queue authority to the native shape: %s', async (error) => {
  answer(() => Promise.reject(error))
  await scanner.submit('credential')
  const offline = error instanceof TypeError && error.message === 'Failed to fetch'
  expect(results).toEqual([offline ? { kind: 'queued' } : { kind: 'local-failure' }])
  expect((await row()).state).toBe(offline ? 'QUEUED' : 'RESOLVED')
})

it('treats a synchronous fetch setup throw as a local failure', async () => {
  vi.stubGlobal('fetch', vi.fn(() => { throw new TypeError('Failed to fetch') }))
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: 'local-failure' }])
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([409, 422])('commits status %s as an unknown denial while its body is stalled', async (status) => {
  const r = response(status)
  let finish!: (value: unknown) => void
  vi.spyOn(r, 'json').mockImplementation(() => new Promise((resolve) => { finish = resolve }))
  answer(r)
  const scanning = scanner.submit('credential')
  await eventually(() => !!sent.length && !!page.coordinator.latched(sent[0].occurrence_id as string), 'denial latched')
  await eventually(() => page.coordinator.view(sent[0].occurrence_id as string)?.durable === 'committed', 'production denial write completes')
  expect(await row()).toMatchObject({ state: 'RESOLVED', localOutcome: { family: 'terminal', cause: 'denied', status, reason: null } })
  expect(results).toEqual([])
  finish({ decision: 'rejected', reason: 'refunded' })
  await scanning
  expect(results).toEqual([{ kind: 'rejected', reason: 'refunded' }])
  await eventually(() => page.coordinator.view(sent[0].occurrence_id as string)?.enrichment === 'committed', 'production reason enrichment completes')
  expect(await row()).toMatchObject({ state: 'RESOLVED', localOutcome: { reason: 'refunded' } })
})

it.each([409, 422])('keeps unreadable status %s as denial rather than queue', async (status) => {
  answer(new Response('{broken', { status }))
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: 'denial-unreadable' }])
  await decisionWriteSettled()
  expect((await row()).state).toBe('RESOLVED')
})

it.each([true, false, undefined])('requires a completed own mark for acceptance with replay %s', async (replay) => {
  answer(response(200, { decision: 'accepted', scanned_at: ACCEPTED, ...(replay === undefined ? {} : { replay }) }))
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: 'accepted', scannedAt: ACCEPTED, replay: replay === true }])
  expect(await row()).toMatchObject({ state: 'ACTUATED', actuated: true, direction: 'entry' })
  expect((await page.store().queued()).entries).toEqual([])
})

it.each([{}, { decision: 'accepted', scanned_at: 'not-a-time' }, { decision: 'rejected', reason: 'refunded' }])('refuses malformed 200 acceptance %s', async (body) => {
  answer(response(200, body))
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: 'body-unconfirmed' }])
  await decisionWriteSettled()
  expect((await row()).state).toBe('RESOLVED')
})

it('never sends when mint aborts', async () => {
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  instrument = instrumentTransactions()
  instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
  await scanner.submit('credential')
  expect(fetch).not.toHaveBeenCalled()
  expect(results).toEqual([{ kind: 'mint-unconfirmed' }])
  expect((await page.store().queued()).entries).toEqual([])
})

it('stops unready input without retaining it for retry', async () => {
  page.storage.stop()
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: 'storage-unready' }])
  page.storage.retry()
  await page.waitFor('ready')
  expect(fetch).not.toHaveBeenCalled()
  expect((await page.store().queued()).entries).toEqual([])
})

it('keeps the captured pairing and claimed time while a request waits', async () => {
  let finish!: (r: Response) => void
  answer(() => new Promise((resolve) => { finish = resolve }))
  const scanning = scanner.submit('credential')
  await eventually(() => !!finish, 'request sent')
  scanner.cancel()
  pairing = { token: 'token-b', generation: 2 }
  finish(response(200, { decision: 'accepted', scanned_at: TIME }))
  await scanning
  expect(results.some((r) => r.kind === 'accepted')).toBe(false)
  await decisionWriteSettled()
  expect(await row()).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'cancelled', requestMayHaveBeenSent: true } })
})

it.each([true, false, undefined])('handles completed own existing ACT with replay %s without a new opening', async (replay) => {
  const decidedAt = '2026-10-18T08:55:01.234Z'
  answer(async () => {
    const original = await row()
    await rawPut(dbName, STORE.occurrences, { ...original, state: 'ACTUATED', actuated: true,
      localOutcome: { family: 'actuation', cause: 'reserved' }, decidedAt })
    return response(200, { decision: 'accepted', scanned_at: ACCEPTED, ...(replay === undefined ? {} : { replay }) })
  })
  await scanner.submit('credential')
  expect(results).toEqual([{ kind: replay === true ? 'duplicate' : 'mark-failed' }])
  expect(page.coordinator.latched(sent[0].occurrence_id as string)).toEqual({
    kind: 'actuation', occurrenceId: sent[0].occurrence_id, decidedAt,
  })
  expect((await row()).state).toBe('ACTUATED')
  expect(page.coordinator.repairWarnings()).toEqual([])
})

it.each(['commit', 'mint-abort', 'queue-abort'] as const)('keeps a known local refusal typed and never sends it, %s', async (schedule) => {
  const ticketId = '10000000-0000-4000-8000-000000000001'
  const qr = `e30.${btoa(JSON.stringify({ tid: ticketId })).replace(/=/g, '')}.signature`
  await page.store().mergeRevoked([ticketId])
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  if (schedule !== 'commit') {
    instrument = instrumentTransactions()
    if (schedule === 'mint-abort') instrument.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
    else {
      const mint = page.store().mintRefusal.bind(page.store())
      vi.spyOn(page.store(), 'mintRefusal').mockImplementation(async (id) => {
        const minted = await mint(id)
        instrument!.inject('abort', { stores: [STORE.occurrences], mode: 'readwrite' })
        return minted
      })
    }
  }
  await scanner.submit(qr)
  expect(results).toEqual([{ kind: schedule === 'mint-abort' ? 'revocation-audit-not-saved' : 'revocation-refused' }])
  expect(fetch).not.toHaveBeenCalled()
  await Promise.all(page.coordinator.pendingTargets().map((id) => page.coordinator.persist(id)))
  await page.coordinator.repairAll()
  const queue = (await page.store().queued()).entries
  if (schedule === 'mint-abort') expect(queue).toEqual([])
  else {
    expect(queue).toHaveLength(1)
    expect(queue[0]).toMatchObject({ format: 1, record: { state: 'QUEUED', localDecision: 'revocation_refused',
      localOutcome: { family: 'queue', cause: 'local_refusal' } } })
    expect(Object.hasOwn((queue[0] as { record: object }).record, 'direction')).toBe(false)
  }
})

it('waits for mint complete before invoking fetch', async () => {
  answer(response(503))
  const fetch = vi.mocked(globalThis.fetch)
  instrument = instrumentTransactions()
  const hold = instrument.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  const scanning = scanner.submit('credential')
  const call = await hold.started
  try {
    expect(call.events).not.toContain('complete')
    expect(fetch).not.toHaveBeenCalled()
    expect(results).toEqual([])
  } finally { hold.release(); await scanning }
  expect(call.events).toContain('complete')
  expect(fetch).toHaveBeenCalledTimes(1)
  expect(results).toEqual([{ kind: 'queued', status: 503 }])
})

it.each(['commit', 'abort'] as const)('publishes acceptance only after the mark completes, %s', async (schedule) => {
  let hold: ReturnType<Instrument['hold']> | undefined
  instrument = instrumentTransactions()
  answer(response(200, { decision: 'accepted', scanned_at: ACCEPTED }), () => {
    hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  })
  const scanning = scanner.submit('credential')
  await eventually(() => !!hold, 'mark barrier installed')
  const call = await hold!.started
  try {
    expect(call.events).not.toContain('complete')
    expect(results).toEqual([])
    onPublish = () => { if (schedule === 'commit') expect(call.events).toContain('complete') }
  } finally { hold!.release(schedule); await scanning }
  expect(results).toEqual(schedule === 'commit'
    ? [{ kind: 'accepted', scannedAt: ACCEPTED, replay: false }]
    : [{ kind: 'mark-failed' }])
  expect((await row()).state).toBe(schedule === 'commit' ? 'ACTUATED' : 'RESOLVED')
})

it.each(['commit', 'abort'] as const)('settles cancellation against an in-flight mark, %s', async (schedule) => {
  let hold: ReturnType<Instrument['hold']> | undefined
  instrument = instrumentTransactions()
  answer(response(200, { decision: 'accepted', scanned_at: ACCEPTED }), () => {
    hold = instrument!.hold({ stores: [STORE.occurrences], mode: 'readwrite' })
  })
  const scanning = scanner.submit('credential')
  await eventually(() => !!hold, 'mark barrier installed')
  const call = await hold!.started
  try {
    await eventually(() => call.events.includes('write-success'), 'mark write request succeeds')
    scanner.cancel()
    pairing = { token: 'token-b', generation: 2 }
  } finally { hold!.release(schedule); await scanning }
  await decisionWriteSettled()
  expect(results).toEqual([{ kind: 'cancelled', requestMayHaveBeenSent: true }])
  expect((await row()).state).toBe(schedule === 'commit' ? 'ACTUATED' : 'RESOLVED')
  expect(page.coordinator.repairWarnings()).toEqual([])
  expect(page.coordinator.pendingTargets()).toEqual([])
})

it('rejects an invalid header before fetch without offline permission', async () => {
  pairing = { token: 'bad\nheader', generation: 1 }
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  await scanner.submit('credential')
  expect(fetch).not.toHaveBeenCalled()
  expect(results).toEqual([{ kind: 'local-failure' }])
  expect((await page.store().queued()).entries).toEqual([])
})
