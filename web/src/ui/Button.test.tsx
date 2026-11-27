// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Button, ButtonLink, buttonClass } from './Button'

afterEach(cleanup)

describe('Button', () => {
  test('is a secondary, regular button of type "button" by default', () => {
    render(<Button>Save</Button>)
    const b = screen.getByRole('button', { name: 'Save' })
    expect(b.getAttribute('type')).toBe('button')
    expect(b.className).toContain('ui-button-secondary')
    expect(b.className).not.toContain('ui-button-sm')
    expect(b.className).toContain('ui-control')
  })

  test("doesn't submit its form unless it is asked to", () => {
    const submit = vi.fn((e: Event) => e.preventDefault())
    render(
      <form onSubmit={(e) => submit(e.nativeEvent)}>
        <Button>Plain</Button>
        <Button type="submit">Send</Button>
      </form>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Plain' }))
    expect(submit).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(submit).toHaveBeenCalledTimes(1)
  })

  test('takes a variant, a size and native attributes', () => {
    render(
      <Button variant="primary" size="sm" name="go" title="Go now" aria-describedby="x">
        Go
      </Button>,
    )
    const b = screen.getByRole('button', { name: 'Go' })
    expect(b.className).toContain('ui-button-primary')
    expect(b.className).toContain('ui-button-sm')
    expect(b.getAttribute('name')).toBe('go')
    expect(b.getAttribute('title')).toBe('Go now')
  })

  test('draws its icon before the label, hidden from screen readers', () => {
    render(<Button icon="plus">New card</Button>)
    const b = screen.getByRole('button', { name: 'New card' })
    const svg = b.querySelector('svg')
    expect(svg?.getAttribute('aria-hidden')).toBe('true')
    expect(svg?.compareDocumentPosition(screen.getByText('New card')) ?? 0).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
  })

  test('loading: says so, keeps its label and its focus, and a press does nothing', () => {
    const click = vi.fn()
    const submit = vi.fn((e: Event) => e.preventDefault())
    const { rerender } = render(
      <form onSubmit={(e) => submit(e.nativeEvent)}>
        <Button type="submit" onClick={click}>
          Save
        </Button>
      </form>,
    )
    const b = screen.getByRole('button', { name: 'Save' })
    b.focus()
    rerender(
      <form onSubmit={(e) => submit(e.nativeEvent)}>
        <Button type="submit" onClick={click} loading>
          Save
        </Button>
      </form>,
    )
    expect(document.activeElement).toBe(b)
    expect(b.getAttribute('aria-busy')).toBe('true')
    expect(b.getAttribute('aria-disabled')).toBe('true')
    expect(b.hasAttribute('disabled')).toBe(false)
    expect(screen.getByRole('button', { name: /^Save\s*Loading$/ })).toBe(b)
    fireEvent.click(b)
    expect(click).not.toHaveBeenCalled()
    expect(submit).not.toHaveBeenCalled()
  })

  test('disabled: not pressable', () => {
    const click = vi.fn()
    render(
      <Button disabled onClick={click}>
        Archive
      </Button>,
    )
    const b = screen.getByRole('button', { name: 'Archive' })
    expect((b as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(b)
    expect(click).not.toHaveBeenCalled()
  })

  test('is pressed from the keyboard as a native button is', () => {
    const click = vi.fn()
    render(<Button onClick={click}>Go</Button>)
    const b = screen.getByRole('button', { name: 'Go' })
    b.focus()
    expect(document.activeElement).toBe(b)
    expect(b.tagName).toBe('BUTTON')
  })
})

describe('ButtonLink', () => {
  test("navigates in the app through the router, with a button's look", () => {
    render(
      <ButtonLink href="/w/1/boards" variant="primary" icon="boards">
        Boards
      </ButtonLink>,
    )
    const a = screen.getByRole('link', { name: 'Boards' })
    expect(a.className).toBe(buttonClass({ variant: 'primary' }))
    fireEvent.click(a)
    expect(window.location.pathname).toBe('/w/1/boards')
    window.history.replaceState(null, '', '/')
  })

  test('is a plain anchor when asked', () => {
    render(
      <ButtonLink href="/api/export" plain download>
        Export
      </ButtonLink>,
    )
    const a = screen.getByRole('link', { name: 'Export' })
    expect(a.hasAttribute('download')).toBe(true)
    const e = fireEvent.click(a)
    expect(e).toBe(true)
  })

  test('opens in a new tab when it has a target: the router leaves the click to the browser', () => {
    render(
      <ButtonLink href="/w/1/boards" target="_blank">
        Boards
      </ButtonLink>,
    )
    const a = screen.getByRole('link', { name: 'Boards' })
    expect(a.getAttribute('target')).toBe('_blank')
    expect(a.getAttribute('rel')).toBe('noopener noreferrer')
    expect(fireEvent.click(a)).toBe(true)
    expect(window.location.pathname).toBe('/')
    cleanup()
    render(
      <ButtonLink href="https://example.com/" target="_blank" rel="noopener">
        Example
      </ButtonLink>,
    )
    expect(screen.getByRole('link', { name: 'Example' }).getAttribute('rel')).toBe('noopener')
  })
})
