import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { OccurrenceRecord, OccurrenceStore } from './occurrences'

const openStore = vi.hoisted(() => vi.fn())
vi.mock('./occurrences', () => ({ openOccurrenceStore: openStore }))

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
    mint: vi.fn().mockResolvedValue({ ...record, state: 'PENDING' }),
    actuate: vi.fn().mockResolvedValue(true),
    markQueued: vi.fn().mockResolvedValue(undefined),
    markSynced: vi.fn().mockResolvedValue(undefined),
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
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ decision: 'accepted' }), { status: 200 }),
    ))

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
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify({
        results: [{ occurrence_id: record.occurrenceId, result: 'done' }],
      }), { status: 200 }),
    ))

    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /sync/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalledOnce())
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
    expect(store.markSynced).not.toHaveBeenCalled()
    expect(screen.getByText(/1 queued offline scan/i)).toBeDefined()
  })
})
