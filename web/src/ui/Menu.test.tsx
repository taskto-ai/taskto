// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Menu, MenuButton, MenuGroup, MenuItem, MenuLink, MenuList, MenuSeparator, MoreMenu } from './Menu'

afterEach(cleanup)

function setup() {
  const history = vi.fn()
  const rename = vi.fn()
  const remove = vi.fn()
  const disabled = vi.fn()
  render(
    <div>
      <MoreMenu>
        <MenuItem icon="clock" onSelect={history}>
          History
        </MenuItem>
        <MenuItem icon="edit" onSelect={rename}>
          Rename
        </MenuItem>
        <MenuItem onSelect={disabled} disabled>
          Rebuild
        </MenuItem>
        <MenuSeparator />
        <MenuItem tone="danger" onSelect={remove}>
          Delete
        </MenuItem>
      </MoreMenu>
      <button type="button">Elsewhere</button>
    </div>,
  )
  const button = screen.getByRole('button', { name: 'More actions' })
  return { button, history, rename, remove, disabled }
}

const list = () => document.querySelector('[role="menu"]') as HTMLElement
const items = () => screen.getAllByRole('menuitem')
const focused = () => document.activeElement?.textContent

describe('Menu', () => {
  test('a button named "More actions" that says it opens a menu, closed at first', () => {
    const { button } = setup()
    expect(button.getAttribute('aria-haspopup')).toBe('menu')
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(screen.queryAllByRole('menuitem')).toHaveLength(0)
    expect(list().hasAttribute('data-popover-open')).toBe(false)
  })

  test('a click opens it as a popover on the first item, labelled by its button', () => {
    const { button } = setup()
    fireEvent.click(button)
    expect(button.getAttribute('aria-expanded')).toBe('true')
    expect(button.getAttribute('aria-controls')).toBe(list().id)
    expect(list().hasAttribute('data-popover-open')).toBe(true)
    expect(list().getAttribute('aria-labelledby')).toBe(button.id)
    expect(focused()).toBe('History')
  })

  test('ArrowUp on the button opens it on the last item, ArrowDown on the first', () => {
    const { button } = setup()
    fireEvent.keyDown(button, { key: 'ArrowUp' })
    expect(focused()).toBe('Delete')
    fireEvent.keyDown(list(), { key: 'Escape' })
    fireEvent.keyDown(button, { key: 'ArrowDown' })
    expect(focused()).toBe('History')
  })

  test('the arrow keys move round the items, Home and End to the ends, past the separator', () => {
    const { button } = setup()
    fireEvent.click(button)
    fireEvent.keyDown(list(), { key: 'ArrowDown' })
    expect(focused()).toBe('Rename')
    fireEvent.keyDown(list(), { key: 'ArrowDown' })
    fireEvent.keyDown(list(), { key: 'ArrowDown' })
    expect(focused()).toBe('Delete')
    fireEvent.keyDown(list(), { key: 'ArrowDown' })
    expect(focused()).toBe('History')
    fireEvent.keyDown(list(), { key: 'ArrowUp' })
    expect(focused()).toBe('Delete')
    fireEvent.keyDown(list(), { key: 'Home' })
    expect(focused()).toBe('History')
    fireEvent.keyDown(list(), { key: 'End' })
    expect(focused()).toBe('Delete')
    expect(screen.getByRole('separator')).toBeTruthy()
  })

  test('a letter moves to the next item that starts with it', () => {
    const { button } = setup()
    fireEvent.click(button)
    fireEvent.keyDown(list(), { key: 'r' })
    expect(focused()).toBe('Rename')
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 5000)
    fireEvent.keyDown(list(), { key: 'r' })
    expect(focused()).toBe('Rebuild')
    vi.restoreAllMocks()
  })

  test('the same letter typed quickly again and again goes round the items that start with it', () => {
    const { button } = setup()
    fireEvent.click(button)
    fireEvent.keyDown(list(), { key: 'r' })
    fireEvent.keyDown(list(), { key: 'r' })
    expect(focused()).toBe('Rebuild')
    fireEvent.keyDown(list(), { key: 'r' })
    expect(focused()).toBe('Rename')
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 5000)
    fireEvent.keyDown(list(), { key: 'h' })
    fireEvent.keyDown(list(), { key: 'i' })
    expect(focused()).toBe('History')
    vi.restoreAllMocks()
  })

  test('the outside-press listener is there only while it is open', () => {
    const add = vi.spyOn(document, 'addEventListener')
    const remove = vi.spyOn(document, 'removeEventListener')
    const { button } = setup()
    const presses = (spy: typeof add) => spy.mock.calls.filter((c) => c[0] === 'pointerdown').length
    expect(presses(add)).toBe(0)
    fireEvent.click(button)
    expect(presses(add)).toBe(1)
    fireEvent.keyDown(list(), { key: 'Escape' })
    expect(presses(remove)).toBe(1)
    vi.restoreAllMocks()
  })

  test('closes when its button scrolls out of sight, and follows it while it is in sight', async () => {
    const { button } = setup()
    fireEvent.click(button)
    const rect = (top: number) => ({
      top,
      bottom: top + 32,
      left: 100,
      right: 132,
      width: 32,
      height: 32,
      x: 100,
      y: top,
      toJSON() {},
    })
    const spy = vi.spyOn(button, 'getBoundingClientRect').mockReturnValue(rect(300) as DOMRect)
    await act(async () => {
      window.dispatchEvent(new Event('scroll'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(button.getAttribute('aria-expanded')).toBe('true')
    spy.mockReturnValue(rect(-100) as DOMRect)
    await act(async () => {
      window.dispatchEvent(new Event('scroll'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(button.getAttribute('aria-expanded')).toBe('false')
    vi.restoreAllMocks()
  })

  test('scrolling inside the menu leaves it where it is, and its height limit on', async () => {
    const { button } = setup()
    fireEvent.click(button)
    list().style.maxBlockSize = '123px'
    await act(async () => {
      list().dispatchEvent(new Event('scroll'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(list().style.maxBlockSize).toBe('123px')
    vi.spyOn(button, 'getBoundingClientRect').mockReturnValue({
      top: 300,
      bottom: 332,
      left: 100,
      right: 132,
      width: 32,
      height: 32,
      x: 100,
      y: 300,
      toJSON() {},
    } as DOMRect)
    await act(async () => {
      window.dispatchEvent(new Event('scroll'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(list().style.maxBlockSize).not.toBe('123px')
    vi.restoreAllMocks()
  })

  test('placed again (the page scrolls, the window changes size), a long menu keeps its scroll', async () => {
    const { button } = setup()
    fireEvent.click(button)
    vi.spyOn(button, 'getBoundingClientRect').mockReturnValue({
      top: 300,
      bottom: 332,
      left: 100,
      right: 132,
      width: 32,
      height: 32,
      x: 100,
      y: 300,
      toJSON() {},
    } as DOMRect)
    const el = list()
    let top = 0
    Object.defineProperty(el, 'scrollTop', {
      configurable: true,
      get: () => top,
      set: (v: number) => {
        top = v
      },
    })
    const measure = el.getBoundingClientRect.bind(el)
    vi.spyOn(el, 'getBoundingClientRect').mockImplementation(() => {
      if (el.style.maxBlockSize === '') top = 0
      return measure()
    })
    el.scrollTop = 600
    await act(async () => {
      window.dispatchEvent(new Event('scroll'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(el.scrollTop).toBe(600)
    await act(async () => {
      window.dispatchEvent(new Event('resize'))
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })
    expect(el.scrollTop).toBe(600)
    vi.restoreAllMocks()
  })

  test('a menu with no items takes the focus, so Escape is still its own', () => {
    render(<MoreMenu>{null}</MoreMenu>)
    const button = screen.getByRole('button', { name: 'More actions' })
    button.focus()
    fireEvent.click(button)
    expect(document.activeElement).toBe(list())
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(button)
  })

  test('choosing an item runs it, closes the menu and gives the focus back to the button', () => {
    const { button, rename } = setup()
    fireEvent.click(button)
    fireEvent.click(screen.getByRole('menuitem', { name: 'Rename' }))
    expect(rename).toHaveBeenCalledOnce()
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(list().hasAttribute('data-popover-open')).toBe(false)
    expect(document.activeElement).toBe(button)
  })

  test('a disabled item says so and does nothing', () => {
    const { button, disabled } = setup()
    fireEvent.click(button)
    const item = screen.getByRole('menuitem', { name: 'Rebuild' })
    expect(item.getAttribute('aria-disabled')).toBe('true')
    fireEvent.click(item)
    expect(disabled).not.toHaveBeenCalled()
    expect(button.getAttribute('aria-expanded')).toBe('true')
  })

  test('a danger item is marked as one', () => {
    const { button } = setup()
    fireEvent.click(button)
    expect(screen.getByRole('menuitem', { name: 'Delete' }).dataset.tone).toBe('danger')
  })

  test('Escape closes it, gives the focus back, and stops there', () => {
    const { button } = setup()
    const outer = vi.fn()
    document.addEventListener('keydown', outer)
    fireEvent.click(button)
    fireEvent.keyDown(items()[1], { key: 'Escape' })
    document.removeEventListener('keydown', outer)
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(button)
    expect(outer).not.toHaveBeenCalled()
  })

  test("an Escape a tooltip took is not the menu's too: the next one closes it", () => {
    const { button } = setup()
    fireEvent.click(button)
    const tooltip = (e: KeyboardEvent) => e.preventDefault()
    document.addEventListener('keydown', tooltip, true)
    fireEvent.keyDown(items()[0], { key: 'Escape' })
    document.removeEventListener('keydown', tooltip, true)
    expect(button.getAttribute('aria-expanded')).toBe('true')
    fireEvent.keyDown(items()[0], { key: 'Escape' })
    expect(button.getAttribute('aria-expanded')).toBe('false')
  })

  test('a press outside closes it and leaves the focus where it went', () => {
    const { button } = setup()
    fireEvent.click(button)
    const elsewhere = screen.getByRole('button', { name: 'Elsewhere' })
    fireEvent.pointerDown(elsewhere)
    elsewhere.focus()
    expect(button.getAttribute('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(elsewhere)
  })

  test('a press on the button while it is open closes it, and does not open it again', () => {
    const { button } = setup()
    fireEvent.click(button)
    fireEvent.pointerDown(button)
    fireEvent.click(button)
    expect(button.getAttribute('aria-expanded')).toBe('false')
  })

  test('Tab closes it', () => {
    const { button } = setup()
    fireEvent.click(button)
    fireEvent.keyDown(items()[0], { key: 'Tab' })
    expect(button.getAttribute('aria-expanded')).toBe('false')
  })

  test('a menu button with text is named by what it chooses and by the words it shows', () => {
    render(
      <Menu align="start">
        <MenuButton label="Sort by">Newest</MenuButton>
        <MenuList>
          <MenuItem onSelect={() => {}}>Oldest</MenuItem>
        </MenuList>
      </Menu>,
    )
    const button = screen.getByRole('button', { name: 'Sort by: Newest' })
    expect(button.getAttribute('aria-label')).toBeNull()
    act(() => button.click())
    expect(focused()).toBe('Oldest')
  })
  test('a link item is a real link: followed by a click, Enter or Space, and the menu closes', () => {
    window.history.replaceState(null, '', '/start')
    render(
      <MoreMenu>
        <MenuLink href="/elsewhere">Open the page</MenuLink>
      </MoreMenu>,
    )
    const button = screen.getByRole('button', { name: 'More actions' })
    fireEvent.click(button)
    const link = screen.getByRole('menuitem', { name: 'Open the page' })
    expect(link.tagName).toBe('A')
    expect(link.getAttribute('href')).toBe('/elsewhere')
    expect(document.activeElement).toBe(link)
    fireEvent.keyDown(link, { key: ' ' })
    expect(window.location.pathname).toBe('/elsewhere')
    expect(button.getAttribute('aria-expanded')).toBe('false')
  })

  test('radio items in a named group say which is checked, and the keys move through them', () => {
    const pick = vi.fn()
    render(
      <Menu>
        <MenuButton label="Theme" />
        <MenuList>
          <MenuGroup label="Theme">
            <MenuItem checked={false} onSelect={() => pick('light')}>
              Light
            </MenuItem>
            <MenuItem checked onSelect={() => pick('dark')}>
              Dark
            </MenuItem>
          </MenuGroup>
          <MenuSeparator />
          <MenuItem onSelect={() => pick('other')}>Other</MenuItem>
        </MenuList>
      </Menu>,
    )
    act(() => screen.getByRole('button', { name: 'Theme' }).click())
    const group = screen.getByRole('group', { name: 'Theme' })
    const radios = within(group).getAllByRole('menuitemradio')
    expect(radios.map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'true'])
    expect(focused()).toBe('Light')
    fireEvent.keyDown(radios[0], { key: 'ArrowDown' })
    expect(focused()).toBe('Dark')
    fireEvent.keyDown(radios[1], { key: 'ArrowDown' })
    expect(focused()).toBe('Other')
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'ArrowDown' })
    fireEvent.click(document.activeElement as HTMLElement)
    expect(pick).toHaveBeenCalledWith('light')
  })

  test("a text menu button's indicator can be another icon, or none", () => {
    render(
      <>
        <Menu>
          <MenuButton label="Switch" indicator="chevron-updown">
            A
          </MenuButton>
          <MenuList>
            <MenuItem onSelect={() => {}}>x</MenuItem>
          </MenuList>
        </Menu>
        <Menu>
          <MenuButton label="Account" indicator={null}>
            B
          </MenuButton>
          <MenuList>
            <MenuItem onSelect={() => {}}>y</MenuItem>
          </MenuList>
        </Menu>
      </>,
    )
    expect(screen.getByRole('button', { name: 'Switch: A' }).querySelectorAll('svg')).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'Account: B' }).querySelector('svg')).toBeNull()
  })
})
