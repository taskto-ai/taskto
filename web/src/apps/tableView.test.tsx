// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { AppView } from './AppView'
import { DEFINITION_VERSION } from './catalog'
import { tableItems, tableSurface } from './table.testing'
import type { ActionPayload } from './transport'

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await setPreferences({ user: null })
})

function app(messages: unknown[], sent: ActionPayload[] = []) {
  return (
    <AppView
      surfaceId="app-1"
      label="Tasks app"
      release={{ definition_version: DEFINITION_VERSION, requires: ['component.Table'] }}
      transport={{
        load: async () => structuredClone(messages),
        act: async (action) => {
          sent.push(action)
          return []
        },
      }}
    />
  )
}

const bodyRows = () => within(screen.getByRole('table')).getAllByRole('row').slice(1)
const firstCells = () => bodyRows().map((r) => within(r).getAllByRole('rowheader')[0].textContent)

test('a table shows the chosen fields of the rows, with choice labels and formatted dates, numbers and booleans', async () => {
  render(app(tableSurface()))
  await screen.findByRole('table', { name: 'Tasks' })
  const heads = screen.getAllByRole('columnheader').map((h) => h.textContent)
  expect(heads).toEqual(['Title', 'Category', 'Due', 'Amount', 'Done'])
  const [rent, shelf, bank] = bodyRows()
  expect(
    within(rent)
      .getAllByRole('cell')
      .map((c) => c.textContent),
  ).toEqual(['Home', 'Mar 8, 2026', '1,234.5', 'No'])
  expect(
    within(shelf)
      .getAllByRole('cell')
      .map((c) => c.textContent),
  ).toEqual(['Other', 'Jan 2, 2026', '20', 'Yes'])
  expect(
    within(bank)
      .getAllByRole('cell')
      .map((c) => c.textContent),
  ).toEqual(['Work', '', '3', ''])
  expect(screen.queryByRole('status')).toBeNull()
})

test('a click on a header sorts, again reverses, and a row without a value stays last', async () => {
  render(app(tableSurface()))
  await screen.findByRole('table')
  expect(firstCells()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank'])

  fireEvent.click(screen.getByRole('button', { name: 'Due' }))
  expect(firstCells()).toEqual(['Fix the shelf', 'Pay rent', 'Call the bank'])
  expect(screen.getByRole('columnheader', { name: 'Due' }).getAttribute('aria-sort')).toBe('ascending')

  fireEvent.click(screen.getByRole('button', { name: 'Due' }))
  expect(firstCells()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank'])
  expect(screen.getByRole('columnheader', { name: 'Due' }).getAttribute('aria-sort')).toBe('descending')

  fireEvent.click(screen.getByRole('button', { name: 'Amount' }))
  expect(firstCells()).toEqual(['Call the bank', 'Fix the shelf', 'Pay rent'])
  expect(screen.getByRole('columnheader', { name: 'Due' }).getAttribute('aria-sort')).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Category' }))
  expect(firstCells()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank'])
})

test('the table starts sorted as the definition says', async () => {
  render(app(tableSurface(tableItems, { sort: 'amount', dir: 'desc' })))
  await screen.findByRole('table')
  expect(firstCells()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank'])
  expect(screen.getByRole('columnheader', { name: 'Amount' }).getAttribute('aria-sort')).toBe('descending')
})

test('a row’s open button runs the action with the row’s id, and the revision it was read at', async () => {
  const sent: ActionPayload[] = []
  render(app(tableSurface(), sent))
  await screen.findByRole('table')
  fireEvent.click(screen.getByRole('button', { name: 'Open Fix the shelf' }))
  await waitFor(() => expect(sent).toHaveLength(1))
  expect(sent[0]).toMatchObject({ name: 'open_task', context: { id: 2 }, expectedRevision: 1 })
})

test('without open, the first column is plain text and nothing opens', async () => {
  render(app(tableSurface(tableItems, { open: undefined })))
  await screen.findByRole('table')
  expect(screen.queryByRole('button', { name: /^Open / })).toBeNull()
  expect(screen.getByText('Pay rent')).toBeTruthy()
})

test('it shows at most 200 rows and says how many there are', async () => {
  const many = Array.from({ length: 250 }, (_, i) => ({ id: i + 1, _revision: 1, title: `Task ${i + 1}` }))
  render(
    app(
      tableSurface(many, {
        columns: [{ field: 'title', label: 'Title', type: 'string' }],
      }),
    ),
  )
  await screen.findByRole('table')
  expect(bodyRows()).toHaveLength(200)
  expect(screen.getByRole('status').textContent).toBe('Showing 200 of 250')
})

test('it is drawn in the viewer’s language', async () => {
  await setPreferences({ user: 'pt-BR' })
  render(app(tableSurface()))
  await screen.findByRole('table')
  expect(within(bodyRows()[0]).getAllByRole('cell')[3].textContent).toBe('Não')
  expect(within(bodyRows()[0]).getAllByRole('cell')[2].textContent).toBe('1.234,5')
  expect(screen.getByRole('button', { name: 'Abrir Pay rent' })).toBeTruthy()
})

test('a record saved offline and not yet sent is listed, without an open button until the server has it', async () => {
  const items = [...tableItems, { id: -7, title: 'Draft the plan', category: 'work', _labels: { category: 'Work' } }]
  render(app(tableSurface(items)))
  await screen.findByRole('table')
  expect(firstCells()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank', 'Draft the plan'])
  expect(screen.queryByRole('button', { name: 'Open Draft the plan' })).toBeNull()
  expect(screen.getByRole('button', { name: 'Open Pay rent' })).toBeTruthy()
})

test('only a table with an open action has a row header, and only the numbers are set to align as numbers', async () => {
  render(app(tableSurface(tableItems, { open: undefined })))
  await screen.findByRole('table')
  expect(screen.queryAllByRole('rowheader')).toHaveLength(0)
  cleanup()
  render(app(tableSurface()))
  await screen.findByRole('table')
  expect(screen.getAllByRole('rowheader')).toHaveLength(3)
  const [first] = bodyRows()
  const numeric = within(first)
    .getAllByRole('cell')
    .map((c) => c.getAttribute('data-numeric'))
  expect(numeric).toEqual([null, null, 'true', null])
})

test('a row with no text in its first column is opened by its number', async () => {
  const items = [{ id: 9, _revision: 1, title: '', category: 'home', _labels: { category: 'Home' } }]
  render(app(tableSurface(items)))
  await screen.findByRole('table')
  expect(screen.getByRole('button', { name: 'Open 9' })).toBeTruthy()
})

test('a click on a choice column sorts by the labels people read, not the stored values', async () => {
  const items = [
    { id: 1, _revision: 1, title: 'One', category: 'a', _labels: { category: 'Zed' } },
    { id: 2, _revision: 1, title: 'Two', category: 'b', _labels: { category: 'Amy' } },
    { id: 3, _revision: 1, title: 'Three', category: 'c', _labels: { category: 'Bob' } },
  ]
  render(app(tableSurface(items)))
  await screen.findByRole('table')
  fireEvent.click(screen.getByRole('button', { name: 'Category' }))
  expect(firstCells()).toEqual(['Two', 'Three', 'One'])
})

test('a column with a key the table does not know is refused, so nothing is drawn', async () => {
  const columns = [{ field: 'title', label: 'Title', type: 'string', width: 3 }]
  render(app(tableSurface(tableItems, { columns })))
  await waitFor(() => expect(screen.queryByRole('table')).toBeNull())
  expect(screen.queryByText('Pay rent')).toBeNull()
})

test('the 200 rows shown are the first 200 after sorting, not the first 200 received', async () => {
  const many = Array.from({ length: 250 }, (_, i) => ({ id: i + 1, _revision: 1, n: i + 1 }))
  render(
    app(
      tableSurface(many, {
        columns: [{ field: 'n', label: 'Number', type: 'number' }],
        open: undefined,
        sort: 'n',
        dir: 'desc',
      }),
    ),
  )
  await screen.findByRole('table')
  const cells = bodyRows().map((r) => within(r).getAllByRole('cell')[0].textContent)
  expect(cells).toHaveLength(200)
  expect(cells[0]).toBe('250')
  expect(cells.at(-1)).toBe('51')
})
