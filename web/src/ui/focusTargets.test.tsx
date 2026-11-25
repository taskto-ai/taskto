// @vitest-environment jsdom

// The places a screen moves the keyboard to from a script (a page's title, a heading), what a link
// row's onOpen does when the link is followed another way, and rows whose order means something.

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { createRef } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { Heading } from './Heading'
import { PageHeader } from './PageHeader'
import { Row, Rows } from './Rows'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

test('a header given a title ref lets a script, not Tab, move the keyboard to its title', () => {
  const ref = createRef<HTMLHeadingElement>()
  const { rerender } = render(<PageHeader title="Home" titleRef={ref} />)
  const h1 = screen.getByRole('heading', { level: 1, name: 'Home' })
  expect(ref.current).toBe(h1)
  ref.current?.focus()
  expect(document.activeElement).toBe(h1)
  expect(h1.tabIndex).toBe(-1)
  // Without one, the title is not a place for the keyboard at all.
  rerender(<PageHeader title="Home" />)
  expect(screen.getByRole('heading', { level: 1 }).hasAttribute('tabindex')).toBe(false)
})

test('a Heading takes a ref, so a script can move the keyboard to it', () => {
  const ref = createRef<HTMLHeadingElement>()
  render(
    <Heading level={2} ref={ref} tabIndex={-1}>
      Set up your workspace
    </Heading>,
  )
  const h2 = screen.getByRole('heading', { level: 2, name: 'Set up your workspace' })
  expect(ref.current).toBe(h2)
  ref.current?.focus()
  expect(document.activeElement).toBe(h2)
})

test('a link row’s onOpen runs however the link is followed: in this tab, or a new one with Ctrl or Cmd', () => {
  window.history.replaceState(null, '', '/w/1/notifications')
  const opened = vi.fn()
  render(
    <Rows>
      <Row title="A card was assigned to you" href="/w/1/boards/3" onOpen={opened} />
    </Rows>,
  )
  const link = screen.getByRole('link', { name: 'A card was assigned to you' })
  // A new tab: the app stays where it is, and the row was still opened.
  fireEvent.click(link, { ctrlKey: true })
  expect(opened).toHaveBeenCalledTimes(1)
  expect(window.location.pathname).toBe('/w/1/notifications')
  fireEvent.click(link, { metaKey: true })
  expect(opened).toHaveBeenCalledTimes(2)
  // This tab: the app goes there.
  fireEvent.click(link)
  expect(opened).toHaveBeenCalledTimes(3)
  expect(window.location.pathname).toBe('/w/1/boards/3')
})

test('rows whose order means something are an ordered list; others are not', () => {
  const { rerender } = render(
    <Rows label="Steps" ordered>
      <Row title="Invite your team" />
    </Rows>,
  )
  expect(screen.getByRole('list', { name: 'Steps' }).tagName).toBe('OL')
  rerender(
    <Rows label="Steps">
      <Row title="Invite your team" />
    </Rows>,
  )
  expect(screen.getByRole('list', { name: 'Steps' }).tagName).toBe('UL')
})
