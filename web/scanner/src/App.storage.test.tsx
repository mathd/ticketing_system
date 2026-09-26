import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { OccurrenceRecord, OccurrenceStore } from './occurrences'

const openStore = vi.hoisted(() => vi.fn())

vi.mock('./occurrences', () => ({ openOccurrenceStore: openStore }))
vi.mock('./occurrence-owner', () => ({
  createOccurrenceOwner: () => ({
    ownerId: 'current-page-owner',
    recoverOwnerId: 'previous-page-owner',
  }),
  replaceOccurrenceOwner: () => ({
    ownerId: 'replacement-page-owner',
    recoverOwnerId: 'current-page-owner',
  }),
}))

import App from './App'

function storedOccurrence(): OccurrenceRecord {
  return {
    occurrenceId: '6efae2f2-cd2d-4b9d-96cb-ec4ee048dc78',
    qrPayload: 'signed-ticket',
    occurredAt: '2026-09-03T20:00:00Z',
    state: 'PENDING',
    actuated: false,
    createdAt: '2026-09-03T20:00:00Z',
  }
}

function fakeStore() {
  const record = storedOccurrence()
  const store: OccurrenceStore = {
    mint: vi.fn().mockResolvedValue(record),
    mintRefusal: vi.fn().mockResolvedValue({ ...record, state: 'QUEUED', localDecision: 'revocation_refused' }),
    isRevoked: vi.fn().mockResolvedValue(false),
    beginRevocationPull: vi.fn().mockResolvedValue('generation'),
    mergeRevoked: vi.fn().mockResolvedValue(true),
    completeRevocationPull: vi.fn().mockResolvedValue(true),
    revocationPull: vi.fn().mockResolvedValue({ generation: 'generation' }),
    clearRevocations: vi.fn().mockResolvedValue(undefined),
    actuate: vi.fn().mockResolvedValue(true),
    markQueued: vi.fn().mockResolvedValue(undefined),
    markSynced: vi.fn().mockResolvedValue(undefined),
    queued: vi.fn().mockResolvedValue([]),
    get: vi.fn().mockResolvedValue(undefined),
    close: vi.fn(),
  }
  return { record, store }
}

function checkTicket() {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'signed-ticket' } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
}

function routedFetch(scan: () => Promise<Response>) {
  return vi.fn((url: string) => String(url).includes('voided-tickets')
    ? Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
    : scan())
}

beforeEach(() => {
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
  vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(
    new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-09-03T20:00:01Z' }), { status: 200 }),
  )))
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  openStore.mockReset()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

describe('Scanner storage failures', () => {
  it('passes the page owner and reload predecessor into the production store', async () => {
    const { store } = fakeStore()
    openStore.mockResolvedValue(store)

    const view = render(<App />)
    await waitFor(() => expect(openStore).toHaveBeenCalledOnce())
    expect(openStore).toHaveBeenCalledWith('gate-occurrences', {
      ownerId: 'current-page-owner',
      recoverOwnerId: 'previous-page-owner',
    })
    view.unmount()
    await waitFor(() => expect(store.close).toHaveBeenCalledOnce())
  })

  it('retries opening storage after startup failed', async () => {
    const { store } = fakeStore()
    openStore.mockRejectedValueOnce(new Error('IndexedDB unavailable')).mockResolvedValue(store)

    render(<App />)

    expect((await screen.findByRole('alert')).textContent).toMatch(/cannot save scans right now/i)
    checkTicket()

    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(openStore).toHaveBeenCalledTimes(2)
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(1)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('sends nothing when minting fails and enables a successful retry', async () => {
    const { record, store } = fakeStore()
    vi.mocked(store.mint).mockRejectedValueOnce(new Error('transaction failed')).mockResolvedValue(record)
    openStore.mockResolvedValue(store)

    render(<App />)
    await waitFor(() => expect(openStore).toHaveBeenCalledOnce())
    checkTicket()

    expect((await screen.findByRole('alert')).textContent).toMatch(/no ticket was checked/i)
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(0)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))

    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(1)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keeps a server rejection when saving its terminal state fails', async () => {
    const { store } = fakeStore()
    const { store: recoveredStore } = fakeStore()
    vi.mocked(store.markSynced).mockRejectedValue(new Error('transaction failed'))
    openStore.mockResolvedValueOnce(store).mockResolvedValueOnce(recoveredStore)
    vi.stubGlobal('fetch', routedFetch(() => Promise.resolve(
      new Response(JSON.stringify({
        decision: 'rejected',
        reason: 'already_redeemed',
        original_scan_at: '2026-09-03T19:59:00Z',
      }), { status: 409 }),
    )))

    render(<App />)
    await waitFor(() => expect(openStore).toHaveBeenCalledOnce())
    checkTicket()

    expect(await screen.findByRole('heading', { name: 'Rejected' })).toBeDefined()
    expect(await screen.findByText(/already redeemed at/i)).toBeDefined()
    const storageAlert = await screen.findByText(/could not save the server result/i)
    expect(storageAlert.getAttribute('role')).toBe('alert')
    expect(screen.queryByRole('heading', { name: 'Queued offline' })).toBeNull()
    expect(screen.queryByText(/admit per venue offline policy/i)).toBeNull()
    expect(store.markQueued).not.toHaveBeenCalled()

    window.dispatchEvent(new Event('online'))
    await waitFor(() => expect(openStore).toHaveBeenCalledTimes(2))
    expect(store.close).toHaveBeenCalledOnce()
    expect(openStore).toHaveBeenLastCalledWith('gate-occurrences', {
      ownerId: 'replacement-page-owner',
      recoverOwnerId: 'current-page-owner',
    })
    expect(vi.mocked(store.close).mock.invocationCallOrder[0]).toBeLessThan(
      openStore.mock.invocationCallOrder[1],
    )
  })

  it('pulls every feed page before saving the completion time', async () => {
    const { store } = fakeStore()
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((url: string) => Promise.resolve(String(url).includes('cursor=')
      ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      : new Response(JSON.stringify({ ticket_ids: ['a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'], next_cursor: 'next page' }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('voided-tickets'))).toHaveLength(2))
    expect(store.mergeRevoked).toHaveBeenCalledTimes(2)
    expect(store.completeRevocationPull).toHaveBeenCalledOnce()
    expect(screen.queryByText(/holds no revocation list yet/i)).toBeNull()
  })

  it('does not save a completion time when a later page fails', async () => {
    const { store } = fakeStore()
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((url: string) => Promise.resolve(String(url).includes('cursor=')
      ? new Response('unavailable', { status: 503 })
      : new Response(JSON.stringify({ ticket_ids: [], next_cursor: 'next page' }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('voided-tickets'))).toHaveLength(2))
    expect(store.mergeRevoked).toHaveBeenCalledOnce()
    expect(store.completeRevocationPull).not.toHaveBeenCalled()
    expect(screen.getByText(/holds no revocation list yet/i)).toBeDefined()
  })

  it('starts a full pull with the new token after the old pull finishes', async () => {
    const { store } = fakeStore()
    openStore.mockResolvedValue(store)
    let releaseFirst!: (response: Response) => void
    const firstPage = new Promise<Response>((resolve) => { releaseFirst = resolve })
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (String(url).endsWith('/scans')) return Promise.resolve(new Response(JSON.stringify({ error: 'not paired' }), { status: 401 }))
      if (String(url).includes('voided-tickets')) {
        const token = new Headers(init?.headers).get('X-Scanner-Token')
        if (token === 'paired-device-token') return firstPage
        if (token === 'new-device-token' && !String(url).includes('cursor=')) {
          return Promise.resolve(new Response(JSON.stringify({ ticket_ids: ['3f8fb96c-44f9-4a66-8c12-7d91cfae4d72'], next_cursor: 'next' }), { status: 200 }))
        }
        if (token === 'new-device-token') return Promise.resolve(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))
      }
      return Promise.resolve(new Response('{}', { status: 200 }))
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url, init]) =>
      String(url).includes('voided-tickets') && new Headers(init?.headers).get('X-Scanner-Token') === 'paired-device-token',
    )).toHaveLength(1))
    checkTicket()
    await screen.findByLabelText('Pairing token')
    fireEvent.change(screen.getByLabelText('Pairing token'), { target: { value: 'new-device-token' } })
    fireEvent.click(screen.getByRole('button', { name: 'Pair device' }))
    await waitFor(() => {
      expect(localStorage.getItem('scanner.device-token')).toBe('new-device-token')
      expect(store.clearRevocations).toHaveBeenCalledTimes(2)
    })
    // The 401 unpair binds the list to NO token. Binding it to the rejected token
    // would let a stale tab that still holds that token write into the list again.
    const [unpaired, repaired] = vi.mocked(store.clearRevocations).mock.calls.map(([fingerprint]) => fingerprint)
    expect(unpaired).not.toBe(await sha256Hex('paired-device-token'))
    expect(unpaired).not.toBe(await sha256Hex('new-device-token'))
    expect(repaired).toBe(await sha256Hex('new-device-token'))
    expect(fetchMock.mock.calls.filter(([url, init]) =>
      String(url).includes('voided-tickets') && new Headers(init?.headers).get('X-Scanner-Token') === 'new-device-token',
    )).toHaveLength(0)
    releaseFirst(new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 }))

    await waitFor(() => expect(fetchMock.mock.calls.filter(([url, init]) =>
      String(url).includes('voided-tickets') && new Headers(init?.headers).get('X-Scanner-Token') === 'new-device-token',
    )).toHaveLength(2))
    const newTokenCalls = fetchMock.mock.calls.filter(([url, init]) =>
      String(url).includes('voided-tickets') && new Headers(init?.headers).get('X-Scanner-Token') === 'new-device-token',
    )
    expect(newTokenCalls).toHaveLength(2)
    expect(fetchMock.mock.calls.filter(([url, init]) =>
      String(url).includes('voided-tickets') && new Headers(init?.headers).get('X-Scanner-Token') === 'paired-device-token',
    )).toHaveLength(1)
    expect(store.beginRevocationPull).toHaveBeenCalledTimes(2)
    expect(store.mergeRevoked).toHaveBeenCalledWith(['3f8fb96c-44f9-4a66-8c12-7d91cfae4d72'], 'generation', expect.any(String))
    expect(store.completeRevocationPull).toHaveBeenCalledWith('generation', expect.any(String), expect.any(String))
  })

  it('refuses a listed tid before scan or actuation and queues one refusal row', async () => {
    const { store } = fakeStore()
    const rows: OccurrenceRecord[] = []
    vi.mocked(store.isRevoked).mockResolvedValue(true)
    vi.mocked(store.mintRefusal).mockImplementation(async (qrPayload, occurredAt) => {
      const row = { ...storedOccurrence(), qrPayload, occurredAt, localDecision: 'revocation_refused' as const, state: 'QUEUED' as const }
      rows.push(row)
      return row
    })
    vi.mocked(store.queued).mockImplementation(async () => rows.filter((row) => row.state === 'QUEUED'))
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(String(url).endsWith('/reconciliations')
      ? new Response(JSON.stringify({ results: [{ occurrence_id: rows[0]?.occurrenceId, result: 'recorded' }] }), { status: 200 })
      : new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    const tid = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
    const claims = btoa(JSON.stringify({ tid })).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: `header.${claims}.signature` } })
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))

    expect(await screen.findByRole('heading', { name: 'Not valid for entry' })).toBeDefined()
    expect(await screen.findByText(/on the device's revocation list\. Do not admit\./i)).toBeDefined()
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(0)
    expect(store.actuate).not.toHaveBeenCalled()
    expect(store.mintRefusal).toHaveBeenCalledOnce()
    expect(store.markQueued).not.toHaveBeenCalled()
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ state: 'QUEUED', actuated: false, localDecision: 'revocation_refused' })
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/reconciliations'))).toHaveLength(1))
    const request = JSON.parse((fetchMock.mock.calls.find(([url]) => String(url).endsWith('/reconciliations'))![1] as RequestInit).body as string)
    expect(request.occurrences[0].local_decision).toBe('revocation_refused')
  })

  it('shows a listed refusal when the one-step refusal write fails', async () => {
    const { store } = fakeStore()
    vi.mocked(store.isRevoked).mockResolvedValue(true)
    vi.mocked(store.mintRefusal).mockRejectedValue(new Error('transaction failed'))
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('{}', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    const tid = 'a8e94ed1-a02b-4cb7-a47b-a607f7b3872d'
    const claims = btoa(JSON.stringify({ tid })).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: `header.${claims}.signature` } })
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))

    expect(await screen.findByRole('heading', { name: 'Not valid for entry' })).toBeDefined()
    expect((await screen.findByText(/could not save the refusal/i)).textContent).toMatch(/could not save the refusal/i)
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(0)
    expect(store.actuate).not.toHaveBeenCalled()
  })
})
