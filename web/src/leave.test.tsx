// @vitest-environment jsdom

import { act, cleanup, render } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { type BeforeLeave, navigate, useBeforeLeave, usePath } from './router'

// A page that may ask: `ask` is what it does when the person would leave, or null.
function Page({ ask }: { ask: BeforeLeave | null }) {
  useBeforeLeave(ask)
  return <p data-testid="path">{usePath()}</p>
}

// jsdom moves through the history in a task of its own, then fires popstate.
const traversed = () =>
  new Promise<void>((resolve) => window.addEventListener('popstate', () => setTimeout(resolve, 0), { once: true }))
async function go(delta: number) {
  await act(async () => {
    const done = traversed()
    window.history.go(delta)
    await done
  })
}
// After an undone move, the undo's own popstate.
const settled = () => act(() => new Promise((resolve) => setTimeout(resolve, 20)))

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/home')
})
afterEach(cleanup)

test('a page that asks holds a new page until it says go', () => {
  let leave: (() => void) | null = null
  const ask = vi.fn((go: () => void) => {
    leave = go
  })
  render(<Page ask={ask} />)
  act(() => navigate('/w/1/boards'))
  expect(ask).toHaveBeenCalledTimes(1)
  expect(window.location.pathname).toBe('/w/1/home')
  act(() => leave?.())
  expect(window.location.pathname).toBe('/w/1/boards')
})

test('a replace is never asked about, nor a move when no page asks', () => {
  const ask = vi.fn()
  const { rerender } = render(<Page ask={ask} />)
  act(() => navigate('/w/1/home/', { replace: true }))
  expect(ask).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/home/')
  rerender(<Page ask={null} />)
  act(() => navigate('/w/1/apps'))
  expect(ask).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/apps')
})

test('Back with a page that asks is undone, and done again when the page says go', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/start'))
  act(() => navigate('/w/1/boards'))
  let leave: (() => void) | null = null
  const ask = vi.fn((go: () => void) => {
    leave = go
  })
  cleanup()
  const { getByTestId } = render(<Page ask={ask} />)

  await go(-1)
  await settled()
  expect(ask).toHaveBeenCalledTimes(1)
  // The person stays where they were until they answer.
  expect(window.location.pathname).toBe('/w/1/boards')
  expect(getByTestId('path').textContent).toBe('/w/1/boards')

  await act(async () => {
    const done = traversed()
    leave?.()
    await done
  })
  expect(window.location.pathname).toBe('/w/1/start')
  expect(getByTestId('path').textContent).toBe('/w/1/start')
})

test('a page with nothing to lose lets Back through at once, with no undoing', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/start'))
  act(() => navigate('/w/1/boards'))
  cleanup()
  const ask = vi.fn((go: () => void) => go())
  const { getByTestId } = render(<Page ask={ask} />)
  await go(-1)
  await settled()
  expect(ask).toHaveBeenCalledTimes(1)
  expect(window.location.pathname).toBe('/w/1/start')
  expect(getByTestId('path').textContent).toBe('/w/1/start')
})

test('an entry the app did not make (from before it started) goes on without the question', async () => {
  window.history.pushState(null, '', '/w/1/elsewhere')
  render(<Page ask={null} />)
  act(() => navigate('/w/1/boards'))
  cleanup()
  const ask = vi.fn()
  render(<Page ask={ask} />)
  await go(-1)
  await settled()
  expect(ask).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/elsewhere')
})

test('a page that stops asking (it went, or its draft was saved) is asked no more', () => {
  const ask = vi.fn()
  const { unmount } = render(<Page ask={ask} />)
  unmount()
  render(<Page ask={null} />)
  act(() => navigate('/w/1/knowledge'))
  expect(ask).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/knowledge')
})
