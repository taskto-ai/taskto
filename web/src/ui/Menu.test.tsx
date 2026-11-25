// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator, MoreMenu } from './Menu'

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
    // Again later: the next one with that letter.
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 5000)
    fireEvent.keyDown(list(), { key: 'r' })
    expect(focused()).toBe('Rebuild')
    vi.restoreAllMocks()
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

  test('a menu button with text shows it, and its label names it', () => {
    render(
      <Menu align="start">
        <MenuButton label="Sort by">Newest</MenuButton>
        <MenuList>
          <MenuItem onSelect={() => {}}>Oldest</MenuItem>
        </MenuList>
      </Menu>,
    )
    const button = screen.getByRole('button', { name: 'Sort by' })
    expect(button.textContent).toContain('Newest')
    act(() => button.click())
    expect(focused()).toBe('Oldest')
  })
})
