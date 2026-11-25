// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { IconButton } from './IconButton'
import { CLOSE_DELAY_MS, OPEN_DELAY_MS, Tooltip } from './Tooltip'

beforeEach(() => {
  vi.useFakeTimers()
  // Each test starts from the keyboard: the last input is heard across the page.
  fireEvent.keyDown(document, { key: 'Tab' })
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

  test("doesn't open on the focus a click gives a button", () => {
    const b = setup()
    fireEvent.pointerDown(b, { pointerType: 'mouse' })
    act(() => b.focus())
    expect(isOpen()).toBe(false)
  })

  test("doesn't open on a focus the browser doesn't show", () => {
    const b = setup()
    const matches = Element.prototype.matches
    const spy = vi.spyOn(Element.prototype, 'matches').mockImplementation(function (this: Element, s: string) {
      return s === ':focus-visible' ? false : matches.call(this, s)
    })
    act(() => b.focus())
    spy.mockRestore()
    expect(isOpen()).toBe(false)
  })

  test("on a text field: opens on the keyboard's focus, not on a click's or a tap's", () => {
    render(
      <Tooltip content="Your work email">
        <input aria-label="Email" />
      </Tooltip>,
    )
    const field = screen.getByRole('textbox', { name: 'Email' })
    // A text field is :focus-visible on every focus, a click's too: the last input decides.
    fireEvent.pointerDown(field, { pointerType: 'mouse' })
    act(() => field.focus())
    expect(isOpen()).toBe(false)
    act(() => field.blur())
    fireEvent.pointerDown(field, { pointerType: 'touch' })
    act(() => field.focus())
    expect(isOpen()).toBe(false)
    act(() => field.blur())
    fireEvent.keyDown(document, { key: 'Tab' })
    act(() => field.focus())
    expect(isOpen()).toBe(true)
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

  test('placed again when its words change while it is open', () => {
    const draw = (content: string) => (
      <Tooltip content={content}>
        <button type="button">Share</button>
      </Tooltip>
    )
    const { rerender } = render(draw('Short'))
    const b = screen.getByRole('button', { name: 'Share' })
    // At the window's end (jsdom's window is 1024 wide).
    b.getBoundingClientRect = () => ({
      top: 200,
      bottom: 236,
      left: 960,
      right: 1000,
      width: 40,
      height: 36,
      x: 960,
      y: 200,
      toJSON: () => ({}),
    })
    let width = 40
    Object.defineProperty(tip(), 'offsetWidth', { configurable: true, get: () => width })
    act(() => b.focus())
    const before = tip().style.left
    width = 200
    rerender(draw('A much longer line of words'))
    expect(tip().style.left).not.toBe(before)
    expect(Number.parseFloat(tip().style.left) + width).toBeLessThanOrEqual(1024)
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
    // jsdom lays nothing out: the tooltip is 0 wide, so its start is the gap (--space-2, 8px) after
    // the control's end, at 52px.
    expect(ltr.style.left).toBe('60px')
    cleanup()
    const rtl = draw('rtl')
    expect(Number.parseFloat(rtl.style.left)).toBeLessThan(960)
  })

  test('beside the control with room on neither side, it goes above or below it, never over it', () => {
    vi.stubGlobal('innerWidth', 320)
    render(
      <Tooltip content="Northwind Studio" describes={false} side="inline-end">
        <button type="button">N</button>
      </Tooltip>,
    )
    const b = screen.getByRole('button', { name: 'N' })
    const rect = { top: 100, bottom: 144, left: 16, right: 304, width: 288, height: 44, x: 16, y: 100 }
    b.getBoundingClientRect = () => ({ ...rect, toJSON: () => ({}) })
    Object.defineProperty(tip(), 'offsetWidth', { configurable: true, get: () => 134 })
    Object.defineProperty(tip(), 'offsetHeight', { configurable: true, get: () => 28 })
    act(() => b.focus())
    const top = Number.parseFloat(tip().style.top)
    const left = Number.parseFloat(tip().style.left)
    expect(tip().dataset.side).toBeUndefined()
    expect(top + 28 <= rect.top || top >= rect.bottom).toBe(true)
    expect(left).toBeGreaterThanOrEqual(8)
    expect(left + 134).toBeLessThanOrEqual(320 - 8)
    vi.unstubAllGlobals()
  })

  test("doesn't open on the focus that comes back from the menu its control opened, and does on a new one", () => {
    render(
      <div>
        <Tooltip content="Workspace" describes={false}>
          <button type="button" aria-controls="its-menu" aria-expanded="true">
            W
          </button>
        </Tooltip>
        <div id="its-menu">
          <button type="button">Item</button>
        </div>
        <button type="button">Elsewhere</button>
      </div>,
    )
    const control = screen.getByRole('button', { name: 'W' })
    act(() => screen.getByRole('button', { name: 'Item' }).focus())
    act(() => control.focus())
    expect(isOpen()).toBe(false)
    act(() => screen.getByRole('button', { name: 'Elsewhere' }).focus())
    act(() => control.focus())
    expect(isOpen()).toBe(true)
  })

  test('without describing, it is hidden from screen readers and adds no description', () => {
    const b = setup(false)
    expect(b.hasAttribute('aria-describedby')).toBe(false)
    expect(tip().getAttribute('aria-hidden')).toBe('true')
  })
})

describe('Tooltip without content', () => {
  test('never opens and describes nothing, and the control stays the same element when it comes', () => {
    const { rerender } = render(
      <Tooltip content={null}>
        <button type="button">A long name</button>
      </Tooltip>,
    )
    const b = screen.getByRole('button', { name: 'A long name' })
    expect(b.hasAttribute('aria-describedby')).toBe(false)
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    fireEvent.focus(b)
    expect(isOpen()).toBe(false)

    // Its words come (the name is cut now): the same button, which now opens it.
    rerender(
      <Tooltip content="A long name, in full">
        <button type="button">A long name</button>
      </Tooltip>,
    )
    expect(screen.getByRole('button', { name: 'A long name' })).toBe(b)
    expect(b.getAttribute('aria-describedby')).toBe(tip().id)
    fireEvent.pointerLeave(b, { pointerType: 'mouse' })
    fireEvent.pointerEnter(b, { pointerType: 'mouse' })
    act(() => vi.advanceTimersByTime(OPEN_DELAY_MS))
    expect(isOpen()).toBe(true)

    // They go while it is open: it closes.
    rerender(
      <Tooltip content="">
        <button type="button">A long name</button>
      </Tooltip>,
    )
    expect(isOpen()).toBe(false)
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
