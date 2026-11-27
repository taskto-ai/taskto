// @vitest-environment jsdom

import { act, cleanup, render } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { type BeforeLeave, navigate, useBeforeLeave, usePath } from './router'

function Page({ ask }: { ask: BeforeLeave | null }) {
  useBeforeLeave(ask)
  return <p data-testid="path">{usePath()}</p>
}

const traversed = () =>
  new Promise<void>((resolve) => window.addEventListener('popstate', () => setTimeout(resolve, 0), { once: true }))
async function go(delta: number) {
  await act(async () => {
    const done = traversed()
    window.history.go(delta)
    await done
  })
}
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
  render(<Page ask={null} />)
  window.history.pushState(null, '', '/w/1/elsewhere')
  act(() => navigate('/w/1/boards'))
  cleanup()
  const ask = vi.fn()
  const { getByTestId } = render(<Page ask={ask} />)
  await go(-1)
  await settled()
  expect(ask).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/elsewhere')
  await go(1)
  await settled()
  expect(window.location.pathname).toBe('/w/1/boards')
  expect(getByTestId('path').textContent).toBe('/w/1/boards')
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

test('an answer to an older question pushes nothing once another move was asked about', () => {
  const answers: (() => void)[] = []
  const ask = vi.fn((go: () => void) => {
    answers.push(go)
  })
  render(<Page ask={ask} />)
  act(() => navigate('/w/1/boards'))
  act(() => navigate('/w/1/apps'))
  expect(ask).toHaveBeenCalledTimes(2)
  act(() => answers[0]())
  expect(window.location.pathname).toBe('/w/1/home')
  act(() => answers[1]())
  expect(window.location.pathname).toBe('/w/1/apps')
})

test('Back pressed twice while the page asks ends where the app says it is, and a yes goes where it was asked', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/a'))
  act(() => navigate('/w/1/b'))
  act(() => navigate('/w/1/c'))
  cleanup()
  const answers: (() => void)[] = []
  const ask = vi.fn((go: () => void) => {
    answers.push(go)
  })
  const { getByTestId } = render(<Page ask={ask} />)
  await act(async () => {
    window.history.back()
    window.history.back()
    await new Promise((resolve) => setTimeout(resolve, 100))
  })
  await settled()
  expect(window.location.pathname).toBe('/w/1/c')
  expect(getByTestId('path').textContent).toBe('/w/1/c')
  expect(ask).toHaveBeenCalled()
  await act(async () => {
    for (const go of answers.slice(0, -1)) go()
    answers.at(-1)?.()
    await new Promise((resolve) => setTimeout(resolve, 100))
  })
  expect(getByTestId('path').textContent).toBe(window.location.pathname)
  expect(window.location.pathname).not.toBe('/w/1/c')
})

test('a new page agreed to while a Back is being undone waits for the undoing, and Back from it is undone right', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/a'))
  act(() => navigate('/w/1/b'))
  act(() => navigate('/w/1/c'))
  cleanup()
  let hold = true
  const answers: (() => void)[] = []
  const ask = vi.fn((go: () => void) => {
    if (hold) answers.push(go)
    else go()
  })
  const { getByTestId } = render(<Page ask={ask} />)
  window.addEventListener(
    'popstate',
    () => {
      hold = false
      navigate('/w/1/d')
      hold = true
    },
    { once: true },
  )
  await act(async () => {
    window.history.back()
    await new Promise((resolve) => setTimeout(resolve, 100))
  })
  await settled()
  expect(window.location.pathname).toBe('/w/1/d')
  expect(getByTestId('path').textContent).toBe('/w/1/d')

  await act(async () => {
    window.history.back()
    await new Promise((resolve) => setTimeout(resolve, 100))
  })
  await settled()
  expect(window.location.pathname).toBe('/w/1/d')
  expect(getByTestId('path').textContent).toBe('/w/1/d')
})

test('a page drawn while a Back is being undone shows where the app is, not where the address went', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/b'))
  act(() => navigate('/w/1/c'))
  cleanup()
  const ask = vi.fn()
  const { getByTestId, rerender } = render(<Page ask={ask} />)
  let drawn = ''
  window.addEventListener(
    'popstate',
    () => {
      act(() => rerender(<Page ask={ask} />))
      drawn = getByTestId('path').textContent ?? ''
    },
    { once: true },
  )
  window.history.back()
  await new Promise((resolve) => setTimeout(resolve, 100))
  await settled()
  expect(ask).toHaveBeenCalled()
  expect(drawn).toBe('/w/1/c')
  expect(window.location.pathname).toBe('/w/1/c')
})

test('an undoing that never lands is given up: the app takes where the address is, and a waiting push numbers from there', async () => {
  render(<Page ask={null} />)
  act(() => navigate('/w/1/b'))
  act(() => navigate('/w/1/c'))
  cleanup()
  let hold = true
  const ask = vi.fn((go: () => void) => {
    if (!hold) go()
  })
  const { getByTestId } = render(<Page ask={ask} />)
  const lost = vi.spyOn(window.history, 'go').mockImplementationOnce(() => {})
  window.addEventListener(
    'popstate',
    () => {
      hold = false
      navigate('/w/1/d')
      hold = true
    },
    { once: true },
  )
  await act(async () => {
    window.history.back()
    await new Promise((resolve) => setTimeout(resolve, 1300))
  })
  lost.mockRestore()
  expect(window.location.pathname).toBe('/w/1/d')
  expect(getByTestId('path').textContent).toBe('/w/1/d')
  await go(-1)
  await settled()
  expect(window.location.pathname).toBe('/w/1/d')
  expect(getByTestId('path').textContent).toBe('/w/1/d')
})
