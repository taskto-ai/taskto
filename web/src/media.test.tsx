// @vitest-environment jsdom

import { act, cleanup, render, screen } from '@testing-library/react'
import { useRef } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { usePageNarrow } from './media'
import { type Place, PlaceContext } from './router'

function Probe() {
  const ref = useRef<HTMLDivElement>(null)
  const narrow = usePageNarrow(ref)
  return (
    <div ref={ref} data-testid="probe">
      {narrow ? 'narrow' : 'wide'}
    </div>
  )
}

function viewportIs(narrow: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: narrow,
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  }))
}

function placeIs(width: number) {
  let resize: (width: number) => void = () => {}
  class Sized {
    constructor(private readonly callback: ResizeObserverCallback) {
      resize = (next) =>
        this.callback([{ contentRect: { width: next } } as ResizeObserverEntry], this as unknown as ResizeObserver)
    }
    observe() {
      resize(width)
    }
    unobserve() {}
    disconnect() {}
  }
  vi.stubGlobal('ResizeObserver', Sized)
  return (next: number) => resize(next)
}

const place: Place = { id: 'a', path: '/w/1/apps', navigate: vi.fn(), setTitle: vi.fn() }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('with no place the viewport decides, whatever the size of the page’s box', () => {
  viewportIs(true)
  placeIs(2000)
  render(<Probe />)
  expect(screen.getByTestId('probe').textContent).toBe('narrow')
  cleanup()
  viewportIs(false)
  placeIs(300)
  render(<Probe />)
  expect(screen.getByTestId('probe').textContent).toBe('wide')
})

test('in a place its own width decides, and follows it as it changes, whatever the viewport', () => {
  viewportIs(false)
  const resize = placeIs(500)
  render(
    <PlaceContext.Provider value={place}>
      <div data-place="a">
        <Probe />
      </div>
    </PlaceContext.Provider>,
  )
  expect(screen.getByTestId('probe').textContent).toBe('narrow')
  act(() => resize(1000))
  expect(screen.getByTestId('probe').textContent).toBe('wide')
  act(() => resize(600))
  expect(screen.getByTestId('probe').textContent).toBe('narrow')
})
