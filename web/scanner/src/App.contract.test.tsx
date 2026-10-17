import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { ScanDirection } from './access-contract'
import type { OccurrenceRecord, OccurrenceStore } from './occurrences'

const openStore = vi.hoisted(() => vi.fn())
vi.mock('./occurrences', async (importOriginal) => ({
  ...await importOriginal<typeof import('./occurrences')>(),
  openOccurrenceStore: openStore,
}))

import App from './App'

const record: OccurrenceRecord = {
  occurrenceId: '6efae2f2-cd2d-4b9d-96cb-ec4ee048dc78',
  qrPayload: 'signed-ticket',
  occurredAt: '2026-09-03T20:00:00Z',
  state: 'QUEUED',
  actuated: false,
  createdAt: '2026-09-03T20:00:00Z',
}

function fakeStore(queued: OccurrenceRecord[] = []) {
  const store: OccurrenceStore = {
    // The fake keeps the direction it was given: a fake that supplied its own would
    // hide a submit path that forgets the mode.
    mint: vi.fn(async (_qrPayload: string, _occurredAt: string, direction: ScanDirection) => ({ ...record, state: 'PENDING' as const, direction })),
    mintRefusal: vi.fn().mockResolvedValue({ ...record, state: 'PENDING', localDecision: 'revocation_refused' }),
    isRevoked: vi.fn().mockResolvedValue(false),
    mergeRevoked: vi.fn().mockResolvedValue(undefined),
    completeRevocationPull: vi.fn().mockResolvedValue(undefined),
    revocationPull: vi.fn().mockResolvedValue({}),
    actuate: vi.fn().mockResolvedValue(true),
    markQueued: vi.fn().mockResolvedValue(undefined),
    markSynced: vi.fn().mockResolvedValue(undefined),
    closeLate: vi.fn().mockResolvedValue(undefined),
    queued: vi.fn().mockResolvedValue(queued),
    get: vi.fn().mockResolvedValue(undefined),
    close: vi.fn(),
  }
  return store
}

beforeEach(() => {
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  openStore.mockReset()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('Scanner runtime response contracts', () => {
  it('does not actuate an accepted response that lacks its required time', async () => {
    const store = fakeStore()
    openStore.mockResolvedValue(store)
    vi.stubGlobal('fetch', (url: string) => Promise.resolve(String(url).includes('voided-tickets')
      ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      : new Response(JSON.stringify({ decision: 'accepted' }), { status: 200 })))

    render(<App />)
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'signed-ticket' } })
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/server answered but the reply could not be read/i)).toBeDefined()
    expect(store.actuate).not.toHaveBeenCalled()
    expect(store.markQueued).toHaveBeenCalledWith(record.occurrenceId)
  })

  it('does not retire a queued occurrence for an unknown reconciliation result', async () => {
    const store = fakeStore([record])
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((url: string) => Promise.resolve(String(url).includes('voided-tickets')
      ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      : new Response(JSON.stringify({ results: [{ occurrence_id: record.occurrenceId, result: 'done' }] }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /sync/i }))

    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/reconciliations'))).toHaveLength(1))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    expect(store.markSynced).not.toHaveBeenCalled()
    expect(screen.getByText(/1 queued offline scan/i)).toBeDefined()
  })

  // An exit goes through the same decoders as an entry. Each malformation class
  // must leave the exit queued: it must not open the gate, and it must not retire
  // the row.
  it.each([
    ['syntax: a 2xx that is not JSON', 200, 'not json'],
    ['syntax: an HTML error page', 502, '<html><body>502 Bad Gateway</body></html>'],
    ['type: a numeric reason', 409, JSON.stringify({ decision: 'rejected', reason: 7 })],
    ['type: a replay marker that is not a boolean', 200, JSON.stringify({ decision: 'accepted', scanned_at: '2026-10-16T19:00:00Z', replay: 'yes' })],
    ['absence: an acceptance without its time', 200, JSON.stringify({ decision: 'accepted' })],
    ['absence: a rejection without its reason', 409, JSON.stringify({ decision: 'rejected' })],
    ['identity: a rejection sent with the acceptance status', 200, JSON.stringify({ decision: 'rejected', reason: 'not_inside' })],
    ['range: an impossible acceptance time', 200, JSON.stringify({ decision: 'accepted', scanned_at: '2026-02-30T19:00:00Z' })],
  ])('keeps an exit queued for a malformed scan reply — %s', async (_name, status, body) => {
    const store = fakeStore()
    openStore.mockResolvedValue(store)
    vi.stubGlobal('fetch', (url: string) => Promise.resolve(String(url).includes('voided-tickets')
      ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      : new Response(body, { status })))

    render(<App />)
    fireEvent.click(screen.getByRole('radio', { name: 'Exit' }))
    fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value: 'signed-ticket' } })
    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))

    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText('The server answered but the reply could not be read — the exit is saved on this device and will sync when back online. The holder may leave.')).toBeDefined()
    expect(store.mint).toHaveBeenCalledWith('signed-ticket', expect.any(String), 'exit')
    expect(store.actuate).not.toHaveBeenCalled()
    expect(store.markSynced).not.toHaveBeenCalled()
    expect(store.markQueued).toHaveBeenCalledWith(record.occurrenceId)
  })

  const other = '9552ce84-c3aa-4a67-b7e6-65ef783c1c95'
  it.each([
    ['syntax: a body that is not JSON', 'not json'],
    ['type: results that are not a list', JSON.stringify({ results: { occurrence_id: record.occurrenceId, result: 'recorded' } })],
    ['absence: a result without its outcome', JSON.stringify({ results: [{ occurrence_id: record.occurrenceId }] })],
    ['identity: a result for another occurrence', JSON.stringify({ results: [{ occurrence_id: other, result: 'recorded' }] })],
    ['identity: a repeated occurrence', JSON.stringify({ results: [
      { occurrence_id: record.occurrenceId, result: 'recorded' },
      { occurrence_id: record.occurrenceId, result: 'recorded' },
    ] })],
    ['identity: an omitted occurrence', JSON.stringify({ results: [] })],
    ['range: an unknown outcome', JSON.stringify({ results: [{ occurrence_id: record.occurrenceId, result: 'exited' }] })],
    ['range: an impossible recorded time', JSON.stringify({ results: [{ occurrence_id: record.occurrenceId, result: 'recorded', occurred_at: '2026-13-01T00:00:00Z' }] })],
  ])('keeps an exit queued for a malformed reconciliation reply — %s', async (_name, body) => {
    const store = fakeStore([{ ...record, direction: 'exit' }])
    openStore.mockResolvedValue(store)
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(String(url).includes('voided-tickets')
      ? new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })
      : new Response(body, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /sync/i }))

    expect(await screen.findByText(/^Sync failed: The server answered but the reply could not be read\./)).toBeDefined()
    const reconcileCalls = fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/reconciliations'))
    expect(reconcileCalls).toHaveLength(1)
    expect(JSON.parse((reconcileCalls[0][1] as RequestInit).body as string).occurrences).toEqual([
      { qr_payload: record.qrPayload, occurrence_id: record.occurrenceId, occurred_at: record.occurredAt, event_type: 'exit' },
    ])
    expect(store.markSynced).not.toHaveBeenCalled()
    expect(screen.getByText(/1 queued offline scan/i)).toBeDefined()
  })
})
