// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import { AppView } from './AppView'
import { APPS_CATALOG_ID } from './catalog'

afterEach(cleanup)

function messages() {
  return [
    { version: 'v0.9', createSurface: { surfaceId: 'notes', catalogId: APPS_CATALOG_ID } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'notes',
        components: [
          { id: 'root', component: 'Column', children: ['title', 'notes', 'bound'] },
          { id: 'title', component: 'Text', text: 'Meeting notes', variant: 'h2' },
          {
            id: 'notes',
            component: 'Markdown',
            text: '# Plan\n\nSome **bold**, a [link](https://example.com/x) and <script>alert(1)</script>.\n\n- one\n- two',
          },
          { id: 'bound', component: 'Markdown', text: { path: '/record/notes' } },
        ],
      },
    },
    {
      version: 'v0.9',
      updateDataModel: {
        surfaceId: 'notes',
        path: '/',
        value: { record: { notes: '## From a record' } },
      },
    },
  ]
}

test('draws formatted text, from a literal and a record, with links as text and no HTML', async () => {
  const { container } = render(
    <AppView surfaceId="notes" label="Notes" transport={{ load: async () => messages(), act: async () => [] }} />,
  )
  await screen.findByText('Meeting notes')
  await screen.findByText('From a record')
  const [text, record] = container.querySelectorAll('.ui-app-markdown') as NodeListOf<HTMLElement>
  expect(text.querySelector('h3')?.textContent).toBe('Plan')
  expect(record.querySelector('h4')?.textContent).toBe('From a record')
  expect(text.querySelector('strong')?.textContent).toBe('bold')
  expect([...text.querySelectorAll('li')].map((li) => li.textContent)).toEqual(['one', 'two'])
  expect(text.querySelector('a')).toBeNull()
  expect(text.textContent).toContain('link (https://example.com/x)')
  expect(text.querySelector('script, img')).toBeNull()
  expect(text.textContent).toContain('<script>alert(1)</script>')
})
