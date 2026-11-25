// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Tabs, tabPanelProps } from './Tabs'

afterEach(() => {
  cleanup()
  document.documentElement.removeAttribute('dir')
})

const ITEMS = [
  { id: 'members', label: 'Members', count: 3 },
  { id: 'invites', label: 'Invites' },
  { id: 'roles', label: 'Roles' },
]

function Panels({ onChange }: { onChange?: (id: string) => void }) {
  const [value, setValue] = useState('members')
  return (
    <>
      <Tabs
        id="people"
        label="People"
        items={ITEMS}
        value={value}
        onChange={(id) => {
          onChange?.(id)
          setValue(id)
        }}
      />
      <div {...tabPanelProps('people', value)}>Panel {value}</div>
    </>
  )
}

const tab = (name: RegExp) => screen.getByRole('tab', { name })

describe('Tabs as buttons', () => {
  test('a named tab list, the current tab selected and the only tab stop, its panel labelled by it', () => {
    render(<Panels />)
    expect(screen.getByRole('tablist', { name: 'People' })).toBeTruthy()
    expect(tab(/Members/).getAttribute('aria-selected')).toBe('true')
    expect(tab(/Members/).tabIndex).toBe(0)
    expect(tab(/Invites/).tabIndex).toBe(-1)
    const panel = screen.getByRole('tabpanel')
    expect(panel.getAttribute('aria-labelledby')).toBe(tab(/Members/).id)
    expect(tab(/Members/).getAttribute('aria-controls')).toBe(panel.id)
  })

  test('the arrow keys, Home and End move to a tab and show it, round the ends', () => {
    const onChange = vi.fn()
    render(<Panels onChange={onChange} />)
    tab(/Members/).focus()
    fireEvent.keyDown(tab(/Members/), { key: 'ArrowRight' })
    expect(document.activeElement).toBe(tab(/Invites/))
    expect(tab(/Invites/).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tabpanel').textContent).toBe('Panel invites')
    fireEvent.keyDown(tab(/Invites/), { key: 'End' })
    expect(document.activeElement).toBe(tab(/Roles/))
    fireEvent.keyDown(tab(/Roles/), { key: 'ArrowRight' })
    expect(document.activeElement).toBe(tab(/Members/))
    fireEvent.keyDown(tab(/Members/), { key: 'ArrowLeft' })
    expect(document.activeElement).toBe(tab(/Roles/))
    fireEvent.keyDown(tab(/Roles/), { key: 'Home' })
    expect(onChange.mock.calls.map((c) => c[0])).toEqual(['invites', 'roles', 'members', 'roles', 'members'])
  })

  test('in right-to-left, ArrowLeft is the next tab', () => {
    document.documentElement.setAttribute('dir', 'rtl')
    render(
      <div style={{ direction: 'rtl' }}>
        <Panels />
      </div>,
    )
    tab(/Members/).focus()
    fireEvent.keyDown(tab(/Members/), { key: 'ArrowLeft' })
    expect(document.activeElement).toBe(tab(/Invites/))
  })

  test('a click shows a tab', () => {
    render(<Panels />)
    fireEvent.click(tab(/Roles/))
    expect(tab(/Roles/).getAttribute('aria-selected')).toBe('true')
  })
})

describe('Tabs as links', () => {
  const links = [
    { id: 'general', label: 'General', href: '/w/1/settings/general' },
    { id: 'members', label: 'Members', href: '/w/1/settings/members' },
  ]

  test('a named navigation, the current page marked', () => {
    render(<Tabs label="Settings" items={links} value="members" />)
    expect(screen.getByRole('navigation', { name: 'Settings' })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Members' }).getAttribute('aria-current')).toBe('page')
    expect(screen.getByRole('link', { name: 'General' }).getAttribute('aria-current')).toBeNull()
    expect(screen.queryByRole('tab')).toBeNull()
  })

  test('the arrow keys move between the links without following them', () => {
    render(<Tabs label="Settings" items={links} value="members" />)
    const general = screen.getByRole('link', { name: 'General' })
    general.focus()
    fireEvent.keyDown(general, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(screen.getByRole('link', { name: 'Members' }))
    expect(window.location.pathname).not.toBe('/w/1/settings/members')
  })
})
