import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import { instrumentTransactions, type Instrument } from './test/idb-instrument'
import { rawGet, openRaw } from './test/raw-db'
import { ownerLockName } from './occurrence-lock'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { rawGetAll } from './test/raw-db'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => {
  const actual = await load<typeof import('./occurrence-readiness')>()
  return { ...actual, get pageStorage() { return runtime.storage! } }
})
let page: TestPage
let dbName: string

let instrument: Instrument | undefined
let scanReply: () => Promise<Response>
let feedReply: (url: string, token: string) => Promise<Response>
let scans: Record<string, unknown>[]
const TID = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
const feed = (ids: string[] = [], next: string | null = null) => new Response(JSON.stringify({ ticket_ids: ids, next_cursor: next }), { status: 200 })
const accepted = () => new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-18T12:34:56.789Z' }), { status: 200 })
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}
async function fingerprint(token: string) {
  return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token))), (b) => b.toString(16).padStart(2, '0')).join('')
}
beforeEach(async () => {
  dbName = `app-storage-${crypto.randomUUID()}`
  page = createTestPage({ dbName }); runtime.storage = page.storage; await page.ready()
  localStorage.setItem('scanner.device-token', 'token-a')
  scans = []; scanReply = async () => accepted(); feedReply = async () => feed()
  vi.stubGlobal('fetch', vi.fn(async (input: Request | string, init?: RequestInit) => {
    if (typeof input === 'string') return feedReply(input, new Headers(init?.headers).get('X-Scanner-Token')!)
    scans.push(await input.clone().json()); return scanReply()
  }))
})
afterEach(() => { cleanup(); instrument?.restore(); instrument = undefined; destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
async function scan(value = 'credential') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
}
async function pair(token: string) {
  fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: token } })
  fireEvent.submit(screen.getByRole('form', { name: 'Device pairing' }))
  await waitFor(() => expect(localStorage.getItem('scanner.device-token')).toBe(token))
}

it('keeps the page owner lock across React cleanup and remount', async () => {
  const owner = page.coordinator.ownerId
  const first = render(<App />); first.unmount()
  expect(await navigator.locks.request(ownerLockName(owner), { ifAvailable: true }, (lock) => !!lock)).toBe(false)
  render(<App />); await scan(); await screen.findByRole('heading', { name: 'Accepted' })
  expect(page.coordinator.ownerId).toBe(owner)
})

it('disables new scan and camera inputs until an explicit storage retry succeeds', async () => {
  page.destroy()
  page = createTestPage({ dbName: `${dbName}-retry` }); runtime.storage = page.storage
  const open = indexedDB.open.bind(indexedDB)
  const failure = vi.spyOn(indexedDB, 'open').mockImplementationOnce(() => { throw new DOMException('unavailable', 'InvalidStateError') }).mockImplementation(open)
  render(<App />)
  await screen.findByText('Storage is unavailable or incompatible. Keep this page open and retry storage.')
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'not-buffered' } })
  fireEvent.submit(screen.getByRole('form', { name: 'Ticket scan' }))
  expect(scans).toEqual([])
  expect(screen.getByRole('button', { name: 'Use camera' }).hasAttribute('disabled')).toBe(true)
  fireEvent.click(screen.getByRole('button', { name: 'Retry storage' }))
  await page.waitFor('ready')
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  expect(scans).toEqual([])
  await scan('fresh'); await screen.findByRole('heading', { name: 'Accepted' })
  expect(scans).toHaveLength(1); failure.mockRestore()
})

it('sends nothing on an aborted mint and accepts a fresh presentation with the same owner', async () => {
  const owner = page.coordinator.ownerId
  render(<App />)
  instrument = instrumentTransactions(); instrument.inject('abort', { mode: 'readwrite', durability: 'strict' })
  await scan()
  await screen.findByText('Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.')
  expect(scans).toEqual([]); expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
  await scan('fresh'); await screen.findByRole('heading', { name: 'Accepted' })
  expect(page.coordinator.ownerId).toBe(owner)
})

it('keeps denial copy during a failed terminal write and repairs with the same owner', async () => {
  render(<App />)
  const owner = page.coordinator.ownerId
  scanReply = async () => {
    instrument = instrumentTransactions()
    instrument.inject('abort', { mode: 'readwrite', durability: 'strict' })
    return new Response(JSON.stringify({ decision: 'rejected', reason: 'refunded' }), { status: 422 })
  }
  await scan()
  await screen.findByRole('heading', { name: 'Rejected' })
  // The status latch remains authoritative even if background refresh repairs it first.
  await waitFor(() => expect(page.coordinator.view(scans[0].occurrence_id as string)?.final?.kind).toBe('terminal'))
  await screen.findByText('The outcome write needs repair. Keep this page open and retry storage.')
  await waitFor(() => expect(instrument!.pendingRules()).toBe(0))
  fireEvent.click(screen.getByRole('button', { name: 'Retry storage' }))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'denied', reason: 'refunded' } }))
  expect(page.coordinator.ownerId).toBe(owner)
  expect(screen.getByRole('alert', { name: 'Scan result' }).textContent).toBe('RejectedCredential is invalid or cannot be redeemed.')
})

it('stores every feed page and advances the fingerprint only after the final page', async () => {
  const second = deferred<Response>()
  feedReply = async (url) => url.includes('cursor=') ? second.promise : feed([TID], 'next page')
  render(<App />)
  await waitFor(async () => expect(await page.store().isRevoked(TID)).toBe(true))
  const fp = await fingerprint('token-a')
  expect(await page.store().revocationPull(fp)).toEqual({})
  second.resolve(feed())
  await waitFor(async () => expect((await page.store().revocationPull(fp)).completedAt).toEqual(expect.any(String)))
  await waitFor(() => expect(screen.queryByText(/holds no revocation list yet/)).toBeNull())
})

it('keeps the previous completion time absent when a later feed page fails', async () => {
  feedReply = async (url) => url.includes('cursor=') ? new Response('', { status: 503 }) : feed([TID], 'next')
  render(<App />)
  await waitFor(() => expect(vi.mocked(fetch).mock.calls.filter(([input]) => typeof input === 'string' && input.includes('cursor='))).toHaveLength(1))
  expect(await page.store().revocationPull(await fingerprint('token-a'))).toEqual({})
  expect(await page.store().isRevoked(TID)).toBe(true)
  expect(screen.getByText(/holds no revocation list yet/)).toBeDefined()
})

it('stops a feed walk after unmount without closing the page store', async () => {
  const read = deferred<{ completedAt?: string }>()
  const original = page.store().revocationPull.bind(page.store())
  const spy = vi.spyOn(page.store(), 'revocationPull').mockImplementationOnce(() => read.promise).mockImplementation(original)
  const view = render(<App />)
  await waitFor(() => expect(spy).toHaveBeenCalled())
  view.unmount(); read.resolve({})
  await act(async () => { await read.promise })
  expect(fetch).not.toHaveBeenCalled()
  expect(page.storage.readyStore()).toBeTruthy()
})

it('allows token B to finish its feed while token A has a pending page', async () => {
  const old = deferred<Response>()
  feedReply = async (_, token) => token === 'token-a' ? old.promise : feed([TID])
  scanReply = async () => new Response('', { status: 401 })
  render(<App />)
  await waitFor(() => expect(fetch).toHaveBeenCalled())
  await scan(); await screen.findByLabelText('Pairing token'); await pair('token-b')
  const b = await fingerprint('token-b')
  await waitFor(async () => expect((await page.store().revocationPull(b)).completedAt).toEqual(expect.any(String)))
  expect(await page.store().isRevoked(TID)).toBe(true)
  old.resolve(new Response('', { status: 401 }))
  await act(async () => { await old.promise })
  expect(localStorage.getItem('scanner.device-token')).toBe('token-b')
  expect(screen.getByLabelText('Ticket credential')).toBeDefined()
})

it('retains the no-list notice for B until B completes its own pull', async () => {
  const pending = deferred<Response>()
  feedReply = async (_, token) => token === 'token-b' ? pending.promise : feed()
  scanReply = async () => new Response('', { status: 401 })
  render(<App />)
  await waitFor(() => expect(screen.queryByText(/holds no revocation list yet/)).toBeNull())
  await scan(); await screen.findByLabelText('Pairing token'); await pair('token-b')
  expect(screen.getByText(/holds no revocation list yet/)).toBeDefined()
  pending.resolve(feed())
  await waitFor(() => expect(screen.queryByText(/holds no revocation list yet/)).toBeNull())
})

it('cancels an old scan on an external pairing change and ignores its late 401', async () => {
  const pending = deferred<Response>(); scanReply = () => pending.promise
  render(<App />); await scan(); await waitFor(() => expect(scans).toHaveLength(1))
  await act(async () => {
    localStorage.setItem('scanner.device-token', 'token-b')
    window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'token-b' }))
  })
  pending.resolve(new Response('', { status: 401 }))
  await waitFor(async () => expect(await rawGet(dbName, STORE.occurrences, scans[0].occurrence_id as string)).toMatchObject({ state: 'RESOLVED', localOutcome: { cause: 'cancelled', requestMayHaveBeenSent: true } }))
  expect(localStorage.getItem('scanner.device-token')).toBe('token-b')
  expect(screen.getByLabelText('Ticket credential')).toBeDefined()
  expect(screen.queryByRole('heading', { name: 'Pair this device' })).toBeNull()
})

it('does not remove a newer localStorage token when the old scan gets 401', async () => {
  scanReply = async () => { localStorage.setItem('scanner.device-token', 'token-b'); return new Response('', { status: 401 }) }
  render(<App />); await scan(); await screen.findByLabelText('Pairing token')
  expect(localStorage.getItem('scanner.device-token')).toBe('token-b')
})

it('refuses a listed ticket without a live request and commits a refusal queue row', async () => {
  await page.store().mergeRevoked([TID])
  render(<App />)
  const qr = `header.${btoa(JSON.stringify({ tid: TID })).replace(/=+$/, '')}.signature`
  await scan(qr)
  expect((await screen.findByRole('alert', { name: 'Scan result' })).textContent).toBe("Not valid for entryThis ticket is on the device's revocation list. Do not admit.")
  await waitFor(async () => expect(await rawGetAll(dbName, STORE.occurrences)).toMatchObject([{ state: 'QUEUED', localDecision: 'revocation_refused', localOutcome: { cause: 'local_refusal' }, actuated: false, qrPayload: qr }]))
  expect(scans).toEqual([])
  const rows = await rawGetAll(dbName, STORE.occurrences) as Record<string, unknown>[]
  expect(Object.hasOwn(rows[0], 'direction')).toBe(false)
})

it('keeps a listed refusal when its mint aborts and creates no row', async () => {
  await page.store().mergeRevoked([TID])
  render(<App />)
  instrument = instrumentTransactions(); instrument.inject('abort', { mode: 'readwrite', durability: 'strict' })
  await scan(`header.${btoa(JSON.stringify({ tid: TID })).replace(/=+$/, '')}.signature`)
  await screen.findByRole('heading', { name: 'Not valid for entry' })
  expect(scans).toEqual([])
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))
  expect(await rawGetAll(dbName, STORE.occurrences)).toEqual([])
})

it('keeps an accepted result after version change and disables new admission controls', async () => {
  render(<App />); await scan(); await screen.findByRole('heading', { name: 'Accepted' })
  const upgrade = await openRaw(dbName, 4)
  upgrade.close()
  await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(true))
  expect(screen.getByRole('heading', { name: 'Accepted' })).toBeDefined()
  expect(screen.getByText('Scanner storage is not ready. No ticket was checked.')).toBeDefined()
  expect(scans).toHaveLength(1)
})
