import type { ReactElement } from 'react'
import { act, render } from '@testing-library/react'
import { vi } from 'vitest'
import { bounded, type TestPage } from './page-fixture'

/** Flush the App update from its initial production queue read before using its controls. */
export async function renderWithQueue(element: ReactElement, page: TestPage): Promise<void> {
  const store = page.store()
  const queued = store.queued.bind(store)
  let read!: () => void
  const initialRead = new Promise<void>((resolve) => { read = resolve })
  const observer = vi.spyOn(store, 'queued').mockImplementation(async () => {
    const result = await queued()
    read()
    return result
  })
  try {
    render(element)
    await act(async () => { await bounded(initialRead, 'App initial queue read') })
  } finally {
    observer.mockRestore()
  }
}
