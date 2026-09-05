import { render, screen } from '@testing-library/svelte'
import { expect, test, vi } from 'vitest'
import App from './App.svelte'

test('renders the app heading', () => {
  // Stub fetch so the onMount check doesn't hit the network.
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, status: 200 })))
  render(App)
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('app')
})
