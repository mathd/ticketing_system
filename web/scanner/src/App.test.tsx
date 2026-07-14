import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function pasteCredential(value = 'signed-ticket') {
  fireEvent.change(screen.getByLabelText('Ticket credential'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check ticket' }))
}

describe('App', () => {
  it('renders the scanner and accepts a pasted credential', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
  })

  it('shows the original scan time for a duplicate', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'rejected', reason: 'already_redeemed', original_scan_at: '2026-07-13T12:00:00Z' }), { status: 409 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByText(/Already redeemed at/)).toBeDefined()
  })

  it('rejects an invalid credential', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'rejected', reason: 'invalid_credential' }), { status: 422 })))
    render(<App />)
    pasteCredential()
    expect(await screen.findByRole('heading', { name: 'Rejected' })).toBeDefined()
  })

  it('keeps the paste path available when camera detection is unavailable', async () => {
    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))
    await waitFor(() => expect(screen.getByText(/Paste the credential instead/)).toBeDefined())
    expect(screen.getByLabelText('Ticket credential')).toBeDefined()
  })

  it('stops a failed camera loop and restores the paste path', async () => {
    const stop = vi.fn()
    const detect = vi.fn().mockRejectedValue(new Error('detector failed'))
    const getUserMedia = vi.fn().mockResolvedValue({ getTracks: () => [{ stop }] })
    vi.stubGlobal('BarcodeDetector', class {
      detect = detect
    })
    vi.stubGlobal('navigator', { mediaDevices: { getUserMedia } })
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ decision: 'accepted', scanned_at: '2026-07-13T12:00:00Z' }), { status: 200 })))

    render(<App />)
    fireEvent.click(screen.getByRole('button', { name: 'Use camera' }))

    expect(await screen.findByText(/Camera QR detection stopped unexpectedly/)).toBeDefined()
    await waitFor(() => expect(stop).toHaveBeenCalledOnce())
    expect(screen.getByRole('button', { name: 'Use camera' }).hasAttribute('disabled')).toBe(false)

    pasteCredential('fallback-ticket')
    expect(await screen.findByRole('heading', { name: 'Accepted' })).toBeDefined()
  })
})
