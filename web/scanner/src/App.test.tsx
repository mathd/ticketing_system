import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import App from './App'

describe('App', () => {
  it('renders the scanner shell placeholder', () => {
    render(<App />)
    expect(screen.getByRole('heading').textContent).toBe('Gate scanner')
    expect(screen.getByTestId('placeholder')).toBeDefined()
  })
})
