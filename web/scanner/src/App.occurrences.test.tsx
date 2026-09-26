import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'

// The occurrence protocol on the scan flow (ADR-025 §D3/§D6): mint-and-commit
// before the request, replay surfaced distinctly, offline scans queue and
// reconcile, conflicts go to the operator — never the gate.

// Every scan and reconciliation needs an enrolled device (ai-review S1). The
// suites below are about SCAN behaviour, so they pair once here — otherwise each
// of them would be re-asserting the pairing screen and its own subject not at
// all. The pairing screen has its own tests, in App.test.tsx.
beforeEach(() => {
  sessionStorage.clear()
  localStorage.setItem('scanner.device-token', 'paired-device-token')
})

afterEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function pasteCredential(value = 'signed-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
}

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
const feedResponse = () => new Response(JSON.stringify({ ticket_ids: [], next_cursor: null }), { status: 200 })

describe('App occurrence protocol', () => {
  it('sends a minted occurrence id and device time with every scan', async () => {
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(String(url).includes('voided-tickets')
      ? feedResponse()
      : new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-17T09:00:00Z' }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(<App />)
    pasteCredential()
    await screen.findByRole('heading', { name: 'Accepted' })
    const scanCalls = fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))
    const body = JSON.parse((scanCalls[0][1] as RequestInit).body as string)
    expect(body.qr_payload).toBe('signed-ticket')
    expect(body.occurrence_id).toMatch(UUID_V4)
    expect(typeof body.occurred_at).toBe('string')
  })

  it('reuses nothing across scans — each decision mints a fresh id', async () => {
    // A fresh Response per call: a Response body is single-read, and a reused
    // one would throw on the second scan and silently queue it.
    const fetchMock = vi.fn().mockImplementation((url: string, _init?: RequestInit) =>
      Promise.resolve(String(url).includes('voided-tickets') ? feedResponse() : new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-17T09:00:00Z' }), { status: 200 })),
    )
    vi.stubGlobal('fetch', fetchMock)
    render(<App />)
    pasteCredential('ticket-a')
    await screen.findByRole('heading', { name: 'Accepted' })
    pasteCredential('ticket-b')
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(2))
    const scanCalls = fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))
    const first = JSON.parse((scanCalls[0][1] as RequestInit).body as string)
    const second = JSON.parse((scanCalls[1][1] as RequestInit).body as string)
    expect(first.occurrence_id).not.toBe(second.occurrence_id)
  })

  it('surfaces a replayed acceptance distinctly', async () => {
    vi.stubGlobal('fetch', (url: string) => Promise.resolve(String(url).includes('voided-tickets') ? feedResponse() : new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-17T09:00:00Z', replay: true }), { status: 200 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByText(/previously recorded/i)).toBeDefined()
  })

  it('sends an undecodable credential through the existing scan path', async () => {
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => Promise.resolve(String(url).includes('voided-tickets')
      ? feedResponse()
      : new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-17T09:00:00Z' }), { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(<App />)
    pasteCredential('header.!.signature')
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith('/scans'))).toHaveLength(1)
  })

  it('queues an offline scan and reconciles it on sync, surfacing conflicts to the operator', async () => {
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => String(url).includes('voided-tickets') ? Promise.resolve(feedResponse()) : Promise.reject(new TypeError('network down')))
    vi.stubGlobal('fetch', fetchMock)
    render(<App />)
    pasteCredential('offline-ticket')
    expect(await screen.findByRole('heading', { name: 'Queued offline' })).toBeDefined()
    expect(await screen.findByText(/1 queued offline scan awaiting sync/i)).toBeDefined()

    // Reconnect: the sync posts the queued occurrence and maps its result.
    const sentCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/scans'))!
    const sent = JSON.parse((sentCall[1] as RequestInit).body as string)
    fetchMock.mockImplementation((url: string, _init?: RequestInit) => {
      if (String(url).includes('voided-tickets')) return Promise.resolve(feedResponse())
      if (String(url).endsWith('/reconciliations')) {
        return Promise.resolve(new Response(JSON.stringify({ results: [{ occurrence_id: sent.occurrence_id, result: 'conflict' }] }), { status: 200 }))
      }
      return Promise.reject(new TypeError('unexpected fetch'))
    })
    fireEvent.click(screen.getByRole('button', { name: /sync/i }))
    expect(await screen.findByText(/1 conflict/i)).toBeDefined()
    await waitFor(() => expect(screen.queryByText(/1 queued/i)).toBeNull())
    const reconcileCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/reconciliations'))!
    const reconcileBody = JSON.parse((reconcileCall[1] as RequestInit).body as string)
    expect(reconcileBody.occurrences).toEqual([
      { qr_payload: 'offline-ticket', occurrence_id: sent.occurrence_id, occurred_at: sent.occurred_at },
    ])
  })
})
