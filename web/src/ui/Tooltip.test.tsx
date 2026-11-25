// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { IconButton } from './IconButton'
import { CLOSE_DELAY_MS, OPEN_DELAY_MS, Tooltip } from './Tooltip'

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

const tip = () => document.querySelector('[role="tooltip"]') as HTMLElement
const isOpen = () => tip().hasAttribute('data-open')

function setup(describes = true) {
  render(
    <Tooltip content="Copies the link" describes={describes}>
      <button type="button">Share</button>
    </Tooltip>,
  )
  return screen.getByRole('button', { name: 'Share' })
}

describe('Tooltip', () => {
  test('describes the control it is on', () => {
    const b = setup()
    expect(b.getAttribute('aria-describedby')).toBe(tip().id)
    expect(tip().textContent).toBe('Copies the link')
    expect(isOpen()).toBe(false)
  })

  test('keeps a description the control already has', () => {
    render(
      <Tooltip content="More">
        <button type="button" aria-describedby="own">
          Share
        </button>
      </Tooltip>,
    )
    expect(screen.getByRole('button').getAttribute('aria-describedby')).toBe(`own ${tip().id}`)
  })

  test('opens after the mouse rests on the control, and closes when it leaves', () => {
    const b = setup()
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    expect(isOpen()).toBe(false)
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    expect(isOpen()).toBe(true)
    fireEvent.pointerLeave(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(CLOSE_DELAY_MS))
    expect(isOpen()).toBe(false)
  })

  test('stays open while the pointer moves onto it', () => {
    const b = setup()
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    fireEvent.pointerLeave(b, { pointerType: 'mouse' })
    fireEvent.pointerEnter(tip(), { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(CLOSE_DELAY_MS * 3))
    expect(isOpen()).toBe(true)
  })

  test("doesn't open on touch", () => {
    const b = setup()
    fireEvent.pointerEnter(b, { pointerType: 'touch' })
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS * 3))
    expect(isOpen()).toBe(false)
  })

  test('opens at once on keyboard focus, and Escape closes it', () => {
    const b = setup()
    act(() => b.focus())
    expect(isOpen()).toBe(true)
    fireEvent.keyDown(b, { key: 'Escape' })
    expect(isOpen()).toBe(false)
    // The focus stays where it was.
    expect(document.activeElement).toBe(b)
  })

  test('closes on blur and when the control is pressed', () => {
    const b = setup()
    act(() => b.focus())
    act(() => b.blur())
    expect(isOpen()).toBe(false)
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    fireEvent.pointerDown(b, { pointerType: 'mouse' })
    expect(isOpen()).toBe(false)
  })

  test('without describing, it is hidden from screen readers and adds no description', () => {
    const b = setup(false)
    expect(b.hasAttribute('aria-describedby')).toBe(false)
    expect(tip().getAttribute('aria-hidden')).toBe('true')
  })
})

describe('IconButton', () => {
  test('its label is its name and its tooltip, said once', () => {
    render(<IconButton icon="edit" label="Rename" />)
    const b = screen.getByRole('button', { name: 'Rename' })
    expect(b.getAttribute('type')).toBe('button')
    expect(b.hasAttribute('aria-describedby')).toBe(false)
    expect(tip().textContent).toBe('Rename')
    expect(b.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
    act(() => b.focus())
    expect(isOpen()).toBe(true)
  })

  test('takes native attributes: a toggle, disabled, a click', () => {
    const click = vi.fn()
    render(<IconButton icon="pin" label="Pin" size="sm" aria-pressed onClick={click} />)
    const b = screen.getByRole('button', { name: 'Pin', pressed: true })
    expect(b.className).toContain('ui-icon-button-sm')
    fireEvent.click(b)
    expect(click).toHaveBeenCalledTimes(1)
  })
})
