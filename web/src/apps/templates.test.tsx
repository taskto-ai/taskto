// @vitest-environment jsdom

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import equipment from '../../../internal/apps/templates/equipment-checkout.json'
import expense from '../../../internal/apps/templates/expense-tracker.json'
import inspection from '../../../internal/apps/templates/inspection.json'
import { AppView } from './AppView'
import { DEFINITION_VERSION } from './catalog'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const base = '/api/workspaces/1/spaces/2/apps/9'
const receipt = 'a'.repeat(32)
const signature = 'b'.repeat(32)

type Template = { definition: { screens: { name: string; components: unknown[] }[] } }

function messages(template: Template, screenName: string, record: Record<string, unknown>) {
  const found = template.definition.screens.find((s) => s.name === screenName)
  if (!found) throw new Error(`no screen ${screenName}`)
  const first = (found.components[0] as { id: string }).id
  return [
    { version: 'v0.9', createSurface: { surfaceId: 'app-9', catalogId: 'https://taskto.ai/catalogs/apps/v1.json' } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'app-9',
        components: [{ id: 'root', component: 'Column', children: [first] }, ...found.components],
      },
    },
    {
      version: 'v0.9',
      updateDataModel: { surfaceId: 'app-9', path: '/', value: { items: [], draft: {}, screen: screenName, record } },
    },
  ]
}

function detail(template: Template, record: Record<string, unknown>) {
  render(
    <AppView
      surfaceId="app-9"
      label="App"
      release={{
        definition_version: DEFINITION_VERSION,
        requires: ['component.Screens', 'component.Show', 'component.Photos', 'component.Stat'],
      }}
      transport={{
        load: async () => messages(template, 'detail', record),
        act: async () => [],
        files: { base, uploads: true },
      }}
    />,
  )
}

const stat = (label: string) => screen.getByText(label).closest('dl')?.querySelector('dd')?.textContent

test('the expense detail shows the category by its label and the receipt as a thumbnail', async () => {
  detail(expense, {
    id: 4,
    _revision: 1,
    description: 'Lunch',
    amount: 12.5,
    spent_on: '2026-11-02',
    category: 'food',
    _labels: { category: 'Food' },
    receipt: [receipt],
  })
  await screen.findByText('Lunch')
  expect(stat('Category')).toBe('Food')
  const photos = screen.getByRole('figure', { name: 'Receipt' })
  const img = within(photos).getByRole('img')
  expect(img.getAttribute('src')).toBe(`${base}/attachments/${receipt}/thumbnail`)
})

test('the inspection detail shows the result by its label and the signature as a thumbnail', async () => {
  detail(inspection, {
    id: 4,
    _revision: 1,
    site: 'Dock 2',
    result: 'fail',
    _labels: { result: 'Fail' },
    signed: signature,
  })
  await screen.findByText('Dock 2')
  expect(stat('Result')).toBe('Fail')
  const img = within(screen.getByRole('figure', { name: 'Signature' })).getByRole('img')
  expect(img.getAttribute('src')).toBe(`${base}/attachments/${signature}/thumbnail`)
})

test('the equipment detail shows the status by its label', async () => {
  detail(equipment, {
    id: 4,
    _revision: 1,
    equipment: 'TAG-1',
    borrower: 'Ana',
    checked_out_on: '2026-11-02',
    status: 'out',
    _labels: { status: 'Checked out' },
  })
  await screen.findByText('TAG-1')
  expect(stat('Status')).toBe('Checked out')
})

test.each([
  ['expense', expense, 'Expenses', 'New expense'],
  ['equipment', equipment, 'Equipment', 'Check out an item'],
  ['inspection', inspection, 'Inspections', 'New inspection'],
] as const)('the %s list draws its icons', async (_, template, heading, start) => {
  const { container } = render(
    <AppView
      surfaceId="app-9"
      label="App"
      transport={{ load: async () => messages(template, 'list', {}), act: async () => [] }}
    />,
  )
  await screen.findByText(heading)
  const button = screen.getByRole('button', { name: start })
  expect(button.querySelector('svg.ui-app-icon path')?.getAttribute('d')).toMatch(/^M/)
  const icons = container.querySelectorAll('svg.ui-app-icon')
  expect(icons).toHaveLength(2)
  for (const svg of icons) expect(svg.getAttribute('aria-hidden')).toBe('true')
})
