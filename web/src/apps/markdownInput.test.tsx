// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { forgetSharedDictationStatus } from '../conversations/Dictation'
import { fakeFetch } from '../test/fakes'
import { AppView } from './AppView'
import { APPS_CATALOG_ID, DEFINITION_VERSION } from './catalog'
import { MarkdownField } from './MarkdownInput'
import { insertText, TextDictation } from './TextDictation'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  forgetSharedDictationStatus()
})

function Harness({
  start = '',
  maxLength,
  seen,
}: {
  start?: string
  maxLength?: number
  seen?: (value: string) => void
}) {
  const [value, setValue] = useState(start)
  return (
    <MarkdownField
      label="Notes"
      value={value}
      maxLength={maxLength}
      onChange={(next) => {
        setValue(next)
        seen?.(next)
      }}
    />
  )
}

const box = () => screen.getByLabelText('Notes') as HTMLTextAreaElement

function select(el: HTMLTextAreaElement, from: number, to: number) {
  el.focus()
  el.setSelectionRange(from, to)
}

test('typing sends the whole text to the bound value', () => {
  const seen = vi.fn()
  render(<Harness seen={seen} />)
  fireEvent.change(box(), { target: { value: 'hello' } })
  expect(seen).toHaveBeenLastCalledWith('hello')
  expect(box().value).toBe('hello')
})

test('every toolbar button changes the text once and keeps the selection on the changed part', () => {
  const seen = vi.fn()
  render(<Harness start="plan the week" seen={seen} />)
  select(box(), 9, 13)
  fireEvent.click(screen.getByRole('button', { name: 'Bold' }))
  expect(seen).toHaveBeenCalledTimes(1)
  expect(box().value).toBe('plan the **week**')
  expect(box().value.slice(box().selectionStart, box().selectionEnd)).toBe('week')

  select(box(), 0, 4)
  fireEvent.click(screen.getByRole('button', { name: 'Italic' }))
  expect(box().value).toBe('_plan_ the **week**')

  select(box(), 0, 0)
  fireEvent.click(screen.getByRole('button', { name: 'Heading' }))
  expect(box().value).toBe('### _plan_ the **week**')
  fireEvent.click(screen.getByRole('button', { name: 'Heading' }))
  expect(box().value).toBe('_plan_ the **week**')
  fireEvent.click(screen.getByRole('button', { name: 'Bulleted list' }))
  expect(box().value).toBe('- _plan_ the **week**')
  fireEvent.click(screen.getByRole('button', { name: 'Checklist' }))
  expect(box().value).toBe('- [ ] _plan_ the **week**')

  select(box(), 6, 12)
  fireEvent.click(screen.getByRole('button', { name: 'Link' }))
  expect(box().value).toBe('- [ ] [_plan_](https://) the **week**')
  expect(box().value.slice(box().selectionStart, box().selectionEnd)).toBe('https://')
})

test('Ctrl and Cmd with B and I wrap the selection, other keys do nothing', () => {
  const seen = vi.fn()
  render(<Harness start="loud soft" seen={seen} />)
  select(box(), 0, 4)
  fireEvent.keyDown(box(), { key: 'b', ctrlKey: true })
  expect(box().value).toBe('**loud** soft')
  select(box(), 9, 13)
  fireEvent.keyDown(box(), { key: 'i', metaKey: true })
  expect(box().value).toBe('**loud** _soft_')
  seen.mockClear()
  fireEvent.keyDown(box(), { key: 'b' })
  fireEvent.keyDown(box(), { key: 'u', ctrlKey: true })
  fireEvent.keyDown(box(), { key: 'b', ctrlKey: true, shiftKey: true })
  expect(seen).not.toHaveBeenCalled()
})

test('the toolbar is one tab stop and the arrow keys move along it', () => {
  render(<Harness />)
  const bar = screen.getByRole('toolbar', { name: 'Formatting' })
  const buttons = [...bar.querySelectorAll('button')]
  expect(buttons.map((b) => b.getAttribute('aria-label'))).toEqual([
    'Bold',
    'Italic',
    'Heading',
    'Bulleted list',
    'Checklist',
    'Link',
  ])
  expect(buttons.filter((b) => b.tabIndex === 0)).toHaveLength(1)
  buttons[0].focus()
  fireEvent.keyDown(buttons[0], { key: 'ArrowRight' })
  expect(document.activeElement).toBe(buttons[1])
  expect(buttons.filter((b) => b.tabIndex === 0)).toEqual([buttons[1]])
  fireEvent.keyDown(buttons[1], { key: 'End' })
  expect(document.activeElement).toBe(buttons[5])
  fireEvent.keyDown(buttons[5], { key: 'ArrowRight' })
  expect(document.activeElement).toBe(buttons[0])
  fireEvent.keyDown(buttons[0], { key: 'ArrowLeft' })
  expect(document.activeElement).toBe(buttons[5])
})

test('the preview shows formatted text with no link, no HTML, and Write brings the text back', () => {
  render(
    <Harness start={'# Plan\n\nSome **bold**, [a link](https://example.com/x) <script>alert(1)</script>\n\n- one'} />,
  )
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const panel = screen.getByRole('tabpanel')
  expect(panel.querySelector('h3')?.textContent).toBe('Plan')
  expect(panel.querySelector('strong')?.textContent).toBe('bold')
  expect(panel.querySelector('li')?.textContent).toBe('one')
  expect(panel.querySelector('a, script')).toBeNull()
  expect(panel.textContent).toContain('a link (https://example.com/x)')
  expect(screen.queryByRole('textbox')).toBeNull()
  fireEvent.click(screen.getByRole('tab', { name: 'Write' }))
  expect(box().value).toContain('**bold**')
})

test('an empty preview says there is nothing to show', () => {
  render(<Harness start="  " />)
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  expect(screen.getByText('Nothing to preview')).toBeTruthy()
})

test('the counter appears past 80 % of the limit, and says so when the limit is passed', () => {
  render(<Harness start={'x'.repeat(80)} maxLength={100} />)
  expect(screen.queryByText(/ of /)).toBeNull()
  fireEvent.change(box(), { target: { value: 'x'.repeat(81) } })
  expect(screen.getByText('81 of 100').hasAttribute('data-full')).toBe(false)
  fireEvent.change(box(), { target: { value: 'x'.repeat(101) } })
  expect(screen.getByText('101 of 100').hasAttribute('data-full')).toBe(true)
})

test('the counter counts characters the way the server does, an emoji as one', () => {
  render(<Harness start={'😀'.repeat(81)} maxLength={100} />)
  expect(screen.getByText('81 of 100').hasAttribute('data-full')).toBe(false)
  fireEvent.change(box(), { target: { value: '😀'.repeat(100) } })
  expect(screen.getByText('100 of 100').hasAttribute('data-full')).toBe(false)
  fireEvent.change(box(), { target: { value: '😀'.repeat(79) } })
  expect(screen.queryByText(/ of /)).toBeNull()
})

test('with no limit known there is no counter', () => {
  render(<Harness start={'x'.repeat(5000)} />)
  expect(screen.queryByText(/ of /)).toBeNull()
})

test('text put in by dictation reaches the bound value', () => {
  const seen = vi.fn()
  render(<Harness start="Checked" seen={seen} />)
  box().setSelectionRange(7, 7)
  insertText(box(), 'the hinge')
  expect(seen).toHaveBeenLastCalledWith('Checked the hinge')
  expect(box().value).toBe('Checked the hinge')
})

const messages = (extra: Record<string, unknown> = {}) => [
  { version: 'v0.9', createSurface: { surfaceId: 'app-9', catalogId: APPS_CATALOG_ID } },
  {
    version: 'v0.9',
    updateComponents: {
      surfaceId: 'app-9',
      components: [
        { id: 'root', component: 'Column', children: ['notes', 'go'] },
        {
          id: 'notes',
          component: 'MarkdownInput',
          field: 'notes',
          label: 'Notes',
          value: { path: '/draft/notes' },
          ...extra,
        },
        { id: 'go_label', component: 'Text', text: 'Save' },
        {
          id: 'go',
          component: 'Button',
          child: 'go_label',
          action: { event: { name: 'add', context: { notes: { path: '/draft/notes' } } } },
        },
      ],
    },
  },
  {
    version: 'v0.9',
    updateDataModel: {
      surfaceId: 'app-9',
      path: '/',
      value: { items: [], draft: {}, _fields: { notes: { max_length: 100, required: false, editable: false } } },
    },
  },
]

const release = { definition_version: DEFINITION_VERSION, requires: ['component.MarkdownInput'] }
const files = { base: '/api/workspaces/1/spaces/2/apps/9', uploads: true }

test('in an app the text, with the toolbar, reaches the action and the limit comes from the server', async () => {
  fakeFetch({})
  const acted = vi.fn(async () => [])
  render(
    <AppView
      surfaceId="app-9"
      label="Meeting"
      release={release}
      transport={{ load: async () => structuredClone(messages()), act: acted, files }}
    />,
  )
  const field = (await screen.findByLabelText('Notes')) as HTMLTextAreaElement
  fireEvent.change(field, { target: { value: 'x'.repeat(85) } })
  expect(await screen.findByText('85 of 100')).toBeTruthy()
  field.setSelectionRange(0, 3)
  fireEvent.click(screen.getByRole('button', { name: 'Bold' }))
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(1))
  const sent = (acted.mock.calls[0] as unknown as [{ context: Record<string, unknown> }])[0]
  expect(sent.context.notes).toBe(`**xxx**${'x'.repeat(82)}`)
})

test('a failed check shows its message beside the field', async () => {
  fakeFetch({})
  const checks = [
    {
      condition: { call: 'length', args: { value: { path: '/draft/notes' }, min: 5 } },
      message: 'Write at least five characters',
    },
  ]
  render(
    <AppView
      surfaceId="app-9"
      label="Meeting"
      release={release}
      transport={{ load: async () => structuredClone(messages({ checks })), act: async () => [], files }}
    />,
  )
  const field = await screen.findByLabelText('Notes')
  fireEvent.change(field, { target: { value: 'ab' } })
  expect(await screen.findByText('Write at least five characters')).toBeTruthy()
  fireEvent.change(field, { target: { value: 'abcdef' } })
  await waitFor(() => expect(screen.queryByText('Write at least five characters')).toBeNull())
})

test('the dictation button sits beside a MarkdownInput and puts its text in the field', async () => {
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: { engine: 'local', max_seconds: 600 } } })
  const acted = vi.fn(async () => [])
  render(
    <AppView
      surfaceId="app-9"
      label="Meeting"
      release={release}
      transport={{ load: async () => structuredClone(messages()), act: acted, files }}
    />,
  )
  expect(await screen.findByRole('button', { name: 'Dictate into Notes' })).toBeTruthy()
  const field = screen.getByLabelText('Notes') as HTMLTextAreaElement
  act(() => insertText(field, 'spoken words'))
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(1))
  const sent = (acted.mock.calls[0] as unknown as [{ context: Record<string, unknown> }])[0]
  expect(sent.context.notes).toBe('spoken words')
})

test('the scan for dictation hosts finds the input in a page that has no app around it', () => {
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: { engine: 'local', max_seconds: 600 } } })
  const root = document.createElement('div')
  document.body.appendChild(root)
  render(<Harness />, { container: root })
  render(<TextDictation root={root} workspace={1} />)
  expect(root.querySelector('.ui-app-markdown-input .ui-dictate-slot')).not.toBeNull()
  root.remove()
})
