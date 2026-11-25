// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Checkbox } from './Checkbox'
import { Field } from './Field'
import { Input } from './Input'
import { SegmentedControl } from './SegmentedControl'
import { Select } from './Select'
import { Switch } from './Switch'
import { Textarea } from './Textarea'

afterEach(cleanup)

describe('Field', () => {
  test('labels its control, and ties the hint and the error to it', () => {
    render(
      <Field label="Email" hint="Where invites are sent." error="Enter a full email address." required>
        <Input type="email" defaultValue="ana@" />
      </Field>,
    )
    const input = screen.getByRole('textbox', { name: /^Email/ })
    expect(input.getAttribute('aria-invalid')).toBe('true')
    expect((input as HTMLInputElement).required).toBe(true)
    const described = (input.getAttribute('aria-describedby') ?? '').split(' ')
    expect(described.map((id) => document.getElementById(id)?.textContent)).toEqual([
      'Where invites are sent.',
      'Enter a full email address.',
    ])
    // The required mark is for the eye; the control's `required` is what a screen reader says.
    expect(screen.getByText('*').getAttribute('aria-hidden')).toBe('true')
  })

  test('without an error the control is valid and described by the hint only', () => {
    render(
      <Field label="Name" hint="As your team knows you.">
        <Input />
      </Field>,
    )
    const input = screen.getByRole('textbox', { name: 'Name' })
    expect(input.hasAttribute('aria-invalid')).toBe(false)
    expect(input.getAttribute('aria-describedby')?.split(' ')).toHaveLength(1)
  })

  test("the field's id names the control, and the control's own description is kept", () => {
    render(
      <>
        <p id="extra">More</p>
        <Field label="Notes" hint="Optional." id="notes">
          <Textarea aria-describedby="extra" />
        </Field>
      </>,
    )
    const t = screen.getByRole('textbox', { name: 'Notes' })
    expect(t.id).toBe('notes')
    expect(t.getAttribute('aria-describedby')).toBe('extra notes-hint')
  })

  test('works for a select, which stays a native one', () => {
    const change = vi.fn()
    render(
      <Field label="Role">
        <Select defaultValue="member" onChange={(e) => change(e.target.value)}>
          <option value="member">Member</option>
          <option value="admin">Admin</option>
        </Select>
      </Field>,
    )
    const s = screen.getByRole('combobox', { name: 'Role' })
    expect(s.tagName).toBe('SELECT')
    fireEvent.change(s, { target: { value: 'admin' } })
    expect(change).toHaveBeenCalledWith('admin')
  })
})

describe('the text controls', () => {
  test('a field and a button of one size share the size class that sets their height', () => {
    render(
      <>
        <Input aria-label="a" />
        <Input aria-label="b" size="sm" />
        <Select aria-label="c" size="sm">
          <option>x</option>
        </Select>
      </>,
    )
    expect(screen.getByRole('textbox', { name: 'a' }).className).toContain('ui-textbox-md')
    expect(screen.getByRole('textbox', { name: 'b' }).className).toContain('ui-textbox-sm')
    expect(screen.getByRole('combobox', { name: 'c' }).className).toContain('ui-textbox-sm')
  })

  test('invalid alone, without a Field', () => {
    render(<Input aria-label="Code" invalid />)
    expect(screen.getByRole('textbox', { name: 'Code' }).getAttribute('aria-invalid')).toBe('true')
  })

  test('every one is left out of the old element rules', () => {
    render(
      <>
        <Input aria-label="a" />
        <Textarea aria-label="b" />
        <Select aria-label="c">
          <option>x</option>
        </Select>
      </>,
    )
    for (const el of [...screen.getAllByRole('textbox'), screen.getByRole('combobox')]) {
      expect(el.classList.contains('ui-control')).toBe(true)
    }
  })
})

describe('Checkbox', () => {
  test('is a labelled native checkbox that Space and a click turn', () => {
    const change = vi.fn()
    render(<Checkbox label="Overdue only" onChange={(e) => change(e.target.checked)} />)
    const box = screen.getByRole('checkbox', { name: 'Overdue only' })
    fireEvent.click(screen.getByText('Overdue only'))
    expect(change).toHaveBeenLastCalledWith(true)
    expect(box.tagName).toBe('INPUT')
  })

  test('can be mixed', () => {
    const { rerender } = render(<Checkbox label="All" indeterminate />)
    const box = screen.getByRole('checkbox', { name: 'All' }) as HTMLInputElement
    expect(box.indeterminate).toBe(true)
    rerender(<Checkbox label="All" />)
    expect(box.indeterminate).toBe(false)
  })
})

describe('Switch', () => {
  test('is a switch to a screen reader, on or off', () => {
    render(<Switch label="Semantic search" defaultChecked />)
    const s = screen.getByRole('switch', { name: 'Semantic search' }) as HTMLInputElement
    expect(s.checked).toBe(true)
    fireEvent.click(s)
    expect(s.checked).toBe(false)
  })
})

function Mode() {
  const [mode, setMode] = useState<'write' | 'preview'>('write')
  return (
    <SegmentedControl
      label="Mode"
      value={mode}
      onChange={setMode}
      options={[
        { value: 'write', label: 'Write' },
        { value: 'preview', label: 'Preview' },
      ]}
    />
  )
}

describe('SegmentedControl', () => {
  test('is a group of radios with one chosen', () => {
    render(<Mode />)
    const group = screen.getByRole('radiogroup', { name: 'Mode' })
    expect(group).toBeTruthy()
    const [write, preview] = screen.getAllByRole('radio') as HTMLInputElement[]
    expect(write.checked).toBe(true)
    expect(preview.checked).toBe(false)
    // One name: the browser moves the choice with the arrow keys and makes the group one Tab stop.
    expect(write.name).toBe(preview.name)
    fireEvent.click(screen.getByText('Preview'))
    expect(preview.checked).toBe(true)
    expect(write.checked).toBe(false)
  })
})
