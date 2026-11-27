// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { type Place, PlaceContext } from '../router'
import { PageHeader } from './PageHeader'

afterEach(cleanup)

const place = (): Place => ({ id: 'a', path: '/w/1/home', navigate: vi.fn(), setTitle: vi.fn() })

test('a page title is reported to its place, and withdrawn when the page goes', () => {
  const at = place()
  const view = render(
    <PlaceContext.Provider value={at}>
      <PageHeader title="Boards" />
    </PlaceContext.Provider>,
  )
  expect(at.setTitle).toHaveBeenLastCalledWith('Boards')
  view.unmount()
  expect(at.setTitle).toHaveBeenLastCalledWith(null)
  expect(at.setTitle).toHaveBeenCalledTimes(2)
})

test('a title that is not text reports its titleText, and one with neither reports nothing', () => {
  const at = place()
  const view = render(
    <PlaceContext.Provider value={at}>
      <PageHeader title={<em>Board</em>} titleText="Board" />
    </PlaceContext.Provider>,
  )
  expect(at.setTitle).toHaveBeenLastCalledWith('Board')
  view.rerender(
    <PlaceContext.Provider value={at}>
      <PageHeader title={<em>Board</em>} />
    </PlaceContext.Provider>,
  )
  expect(at.setTitle).toHaveBeenLastCalledWith(null)
})

test('a second-level heading is not the page’s title, and no place is no problem', () => {
  const at = place()
  render(
    <PlaceContext.Provider value={at}>
      <PageHeader level={2} title="Section" />
    </PlaceContext.Provider>,
  )
  expect(at.setTitle).not.toHaveBeenCalled()
  cleanup()
  const bare = render(<PageHeader title="Alone" />)
  expect(bare.getByRole('heading', { level: 1 }).textContent).toBe('Alone')
})
