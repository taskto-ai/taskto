// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import {
  askPlace,
  type BeforeLeave,
  beforeLeaving,
  Link,
  navigate,
  type Place,
  PlaceContext,
  setFocusedPlace,
  useBeforeLeave,
  useNavigate,
  usePlacePath,
} from './router'

function place(id: string, path = `/w/1/${id}`): Place {
  return { id, path, navigate: vi.fn(), setTitle: vi.fn() }
}

function Guarded({ ask }: { ask: BeforeLeave | null }) {
  useBeforeLeave(ask)
  return null
}

function In({ at, children }: { at: Place; children: ReactNode }) {
  return <PlaceContext.Provider value={at}>{children}</PlaceContext.Provider>
}

let go: ReturnType<typeof useNavigate> | null = null
function Grab() {
  go = useNavigate()
  return <span data-testid="path">{usePlacePath()}</span>
}

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/home')
  go = null
})
afterEach(() => {
  cleanup()
  setFocusedPlace(null)
})

test('without a place, useNavigate is the router navigate and the path is the address', () => {
  render(<Grab />)
  expect(go).toBe(navigate)
  expect(screen.getByTestId('path').textContent).toBe('/w/1/home')
  act(() => go?.('/w/1/boards'))
  expect(window.location.pathname).toBe('/w/1/boards')
})

test('in a place, useNavigate is the place’s and the browser’s address is left alone', () => {
  const a = place('a', '/w/1/knowledge')
  const length = window.history.length
  render(
    <In at={a}>
      <Grab />
    </In>,
  )
  expect(screen.getByTestId('path').textContent).toBe('/w/1/knowledge')
  act(() => go?.('/w/1/boards', { replace: true }))
  expect(a.navigate).toHaveBeenCalledWith('/w/1/boards', { replace: true })
  expect(window.location.pathname).toBe('/w/1/home')
  expect(window.history.length).toBe(length)
})

test('a link in a place goes through the place, and a modified click is left to the browser', () => {
  const a = place('a')
  const hold = (e: Event) => e.preventDefault()
  document.addEventListener('click', hold)
  render(
    <In at={a}>
      <Link href="/w/1/apps">Apps</Link>
    </In>,
  )
  fireEvent.click(screen.getByRole('link', { name: 'Apps' }))
  expect(a.navigate).toHaveBeenCalledWith('/w/1/apps')
  expect(window.location.pathname).toBe('/w/1/home')
  fireEvent.click(screen.getByRole('link', { name: 'Apps' }), { ctrlKey: true })
  expect(a.navigate).toHaveBeenCalledTimes(1)
  document.removeEventListener('click', hold)
})

test('each place has its own guard, and navigating asks the focused place’s only', () => {
  const a = place('a')
  const b = place('b')
  const askA = vi.fn()
  const askB = vi.fn()
  render(
    <>
      <In at={a}>
        <Guarded ask={askA} />
      </In>
      <In at={b}>
        <Guarded ask={askB} />
      </In>
    </>,
  )
  setFocusedPlace('a')
  act(() => navigate('/w/1/boards'))
  expect(askA).toHaveBeenCalledTimes(1)
  expect(askB).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/home')
  setFocusedPlace('b')
  act(() => navigate('/w/1/apps'))
  expect(askA).toHaveBeenCalledTimes(1)
  expect(askB).toHaveBeenCalledTimes(1)
  setFocusedPlace(null)
  act(() => navigate('/w/1/settings'))
  expect(askA).toHaveBeenCalledTimes(1)
  expect(askB).toHaveBeenCalledTimes(1)
  expect(window.location.pathname).toBe('/w/1/settings')
})

test('a guard leaves with its place, and the one before it in the same place asks again', () => {
  const a = place('a')
  const first = vi.fn()
  const second = vi.fn()
  const view = render(
    <In at={a}>
      <Guarded ask={first} />
      <Guarded ask={second} />
    </In>,
  )
  setFocusedPlace('a')
  act(() => navigate('/w/1/boards'))
  expect(second).toHaveBeenCalledTimes(1)
  expect(first).not.toHaveBeenCalled()
  view.rerender(
    <In at={a}>
      <Guarded ask={first} />
    </In>,
  )
  act(() => navigate('/w/1/apps'))
  expect(first).toHaveBeenCalledTimes(1)
})

test('leaving the whole app asks every place in turn and stops at the first refusal', () => {
  const a = place('a')
  const b = place('b')
  const wholes: (boolean | undefined)[] = []
  const agree: BeforeLeave = (go, whole) => {
    wholes.push(whole)
    go()
  }
  const refuse = vi.fn()
  const askB = vi.fn(agree)
  const left = vi.fn()
  const view = render(
    <>
      <In at={a}>
        <Guarded ask={refuse} />
      </In>
      <In at={b}>
        <Guarded ask={askB} />
      </In>
    </>,
  )
  act(() => beforeLeaving(left))
  expect(refuse).toHaveBeenCalledTimes(1)
  expect(askB).not.toHaveBeenCalled()
  expect(left).not.toHaveBeenCalled()

  view.rerender(
    <>
      <In at={a}>
        <Guarded ask={agree} />
      </In>
      <In at={b}>
        <Guarded ask={askB} />
      </In>
    </>,
  )
  act(() => beforeLeaving(left))
  expect(askB).toHaveBeenCalledTimes(1)
  expect(left).toHaveBeenCalledTimes(1)
  expect(wholes).toEqual([true, true])
})

test('leaving the whole app with no guard anywhere goes at once', () => {
  const left = vi.fn()
  beforeLeaving(left)
  expect(left).toHaveBeenCalledTimes(1)
})

test('a place can be asked on its own, for example before its tab closes', () => {
  const a = place('a')
  const b = place('b')
  const askA = vi.fn()
  const askB = vi.fn((go: () => void) => go())
  const closed = vi.fn()
  render(
    <>
      <In at={a}>
        <Guarded ask={askA} />
      </In>
      <In at={b}>
        <Guarded ask={askB} />
      </In>
    </>,
  )
  act(() => askPlace('b', closed))
  expect(askB).toHaveBeenCalledTimes(1)
  expect(askA).not.toHaveBeenCalled()
  expect(closed).toHaveBeenCalledTimes(1)
  act(() => askPlace('a', closed))
  expect(askA).toHaveBeenCalledTimes(1)
  expect(closed).toHaveBeenCalledTimes(1)
  act(() => askPlace('none', closed))
  expect(closed).toHaveBeenCalledTimes(2)
})

test('a newer navigation cancels a leave that is still waiting for a place', () => {
  const a = place('a')
  const holds: (() => void)[] = []
  const left = vi.fn()
  render(
    <In at={a}>
      <Guarded ask={(go) => holds.push(go)} />
    </In>,
  )
  act(() => beforeLeaving(left))
  setFocusedPlace('a')
  act(() => navigate('/w/1/boards'))
  expect(holds).toHaveLength(2)
  act(() => holds[0]?.())
  expect(left).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/w/1/home')
  act(() => holds[1]?.())
  expect(window.location.pathname).toBe('/w/1/boards')
})

test('a place that lost its guard while an earlier one was asked is passed over, and the later ones are still asked', () => {
  const a = place('a')
  const b = place('b')
  const c = place('c')
  let leaveA: (() => void) | null = null
  const askA = vi.fn<BeforeLeave>((go) => {
    leaveA = go
  })
  const askC = vi.fn<BeforeLeave>((go) => go())
  const left = vi.fn()
  const view = render(
    <>
      <In at={a}>
        <Guarded ask={askA} />
      </In>
      <In at={b}>
        <Guarded ask={vi.fn()} />
      </In>
      <In at={c}>
        <Guarded ask={askC} />
      </In>
    </>,
  )
  act(() => beforeLeaving(left))
  view.rerender(
    <>
      <In at={a}>
        <Guarded ask={askA} />
      </In>
      <In at={c}>
        <Guarded ask={askC} />
      </In>
    </>,
  )
  act(() => leaveA?.())
  expect(askC).toHaveBeenCalledTimes(1)
  expect(left).toHaveBeenCalledTimes(1)
})
