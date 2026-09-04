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

beforeEach(() => {
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
    new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-09-03T20:00:01Z' }), { status: 200 }),
  ))
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  openStore.mockReset()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

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
    expect(fetch).toHaveBeenCalledTimes(1)
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
    expect(fetch).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check ticket' }).hasAttribute('disabled')).toBe(false))

    fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keeps a server rejection when saving its terminal state fails', async () => {
    const { store } = fakeStore()
    const { store: recoveredStore } = fakeStore()
    vi.mocked(store.markSynced).mockRejectedValue(new Error('transaction failed'))
    openStore.mockResolvedValueOnce(store).mockResolvedValueOnce(recoveredStore)
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify({
        decision: 'rejected',
        reason: 'already_redeemed',
        original_scan_at: '2026-09-03T19:59:00Z',
      }), { status: 409 }),
    ))

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
})
