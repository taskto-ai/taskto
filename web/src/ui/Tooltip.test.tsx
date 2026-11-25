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

  test('stays open while the control has the focus, though the pointer leaves', () => {
    const b = setup()
    act(() => b.focus())
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    fireEvent.pointerLeave(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(CLOSE_DELAY_MS * 3))
    expect(isOpen()).toBe(true)
    act(() => b.blur())
    expect(isOpen()).toBe(false)
  })

  test('stays open while the pointer rests on the control, though the focus leaves', () => {
    const b = setup()
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => b.focus())
    expect(isOpen()).toBe(true)
    act(() => b.blur())
    expect(isOpen()).toBe(true)
    fireEvent.pointerLeave(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(CLOSE_DELAY_MS))
    expect(isOpen()).toBe(false)
  })

  test('a hover on its way still opens it when the focus leaves first', () => {
    const b = setup()
    act(() => b.focus())
    fireEvent.keyDown(b, { key: 'Escape' })
    expect(isOpen()).toBe(false)
    // The control still has the focus; the pointer comes, and the focus leaves before the delay.
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => b.blur())
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    expect(isOpen()).toBe(true)
  })

  test('follows the control when the page scrolls', () => {
    const b = setup()
    let top = 200
    b.getBoundingClientRect = () => ({
      top,
      bottom: top + 36,
      left: 100,
      right: 200,
      width: 100,
      height: 36,
      x: 100,
      y: top,
      toJSON: () => ({}),
    })
    act(() => b.focus())
    const before = tip().style.top
    top = 400
    act(() => {
      window.dispatchEvent(new Event('scroll'))
    })
    expect(tip().style.top).not.toBe(before)
  })

  test('one that can be seen takes the Escape that closes it, so a drawer or a dialog under it stays', () => {
    const b = setup()
    act(() => b.focus())
    const key = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true })
    act(() => {
      b.dispatchEvent(key)
    })
    expect(isOpen()).toBe(false)
    expect(key.defaultPrevented).toBe(true)
  })

  test('one a stylesheet hides leaves the Escape to what is under it', () => {
    const b = setup()
    act(() => b.focus())
    tip().style.display = 'none'
    const key = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true })
    act(() => {
      b.dispatchEvent(key)
    })
    expect(isOpen()).toBe(false)
    expect(key.defaultPrevented).toBe(false)
  })

  test('beside the control: after its end, or before its start right to left', () => {
    const rect = { top: 100, bottom: 144, left: 8, right: 52, width: 44, height: 44, x: 8, y: 100, toJSON: () => ({}) }
    const draw = (dir: 'ltr' | 'rtl') => {
      render(
        <div dir={dir}>
          <Tooltip content="Boards" describes={false} side="inline-end">
            <button type="button">B</button>
          </Tooltip>
        </div>,
      )
      const b = screen.getByRole('button', { name: 'B' })
      b.getBoundingClientRect = () => ({ ...rect, left: dir === 'ltr' ? 8 : 960, right: dir === 'ltr' ? 52 : 1004 })
      act(() => b.focus())
      return tip()
    }
    const ltr = draw('ltr')
    expect(ltr.dataset.side).toBe('inline-end')
    // jsdom lays nothing out: the tooltip is 0 wide, so its start is the gap after the control.
    expect(ltr.style.left).toBe('58px')
    cleanup()
    const rtl = draw('rtl')
    expect(Number.parseFloat(rtl.style.left)).toBeLessThan(960)
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
