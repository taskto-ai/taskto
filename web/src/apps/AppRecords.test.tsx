// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import type { AppAccess, AppDefinition } from '../api'
import { fakeFetch } from '../test/fakes'
import { AppRecords, RecordsButton } from './AppRecords'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const base = '/api/workspaces/1/spaces/2/apps/9'
const manager: AppAccess = {
  roles: ['data_manager'],
  restricted: true,
  view: true,
  design: false,
  publish: false,
  submit: true,
  manage_roles: false,
  read_records: 'all',
  modify_records: 'all',
}
const definition: AppDefinition = {
  collection: {
    fields: [
      { name: 'description', type: 'string', required: true },
      { name: 'amount', type: 'number' },
      { name: 'kind', type: 'choice', options: ['food', 'travel'] },
    ],
  },
  actions: { add: { kind: 'insert' }, edit: { kind: 'update' }, remove: { kind: 'delete' } },
}
const ana = { id: 1, name: 'Ana', agent: false }
const record = (id: number, revision: number, data: Record<string, unknown>, over: Record<string, unknown> = {}) => ({
  id,
  revision,
  data,
  created_at: '2026-11-01T10:00:00.000Z',
  created_by: ana,
  updated_at: '2026-11-02T10:00:00.000Z',
  updated_by: ana,
  deleted_at: null,
  deleted_by: null,
  ...over,
})
const page = (records: unknown[], over: Record<string, unknown> = {}) => ({
  records,
  next_cursor: null,
  total: records.length,
  aggregates: [],
  ...over,
})
const params = (path: string) => new URL(path, 'http://localhost').searchParams
const phone = () =>
  vi.stubGlobal('matchMedia', (media: string) => ({
    matches: true,
    media,
    addEventListener: () => {},
    removeEventListener: () => {},
  }))
type RecordsProps = Omit<Parameters<typeof AppRecords>[0], 'id' | 'open' | 'onClose'>

// The records as the app's page has them: the button in its header, and the panel it opens.
function Records(props: RecordsProps) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <RecordsButton access={props.access} open={open} controls="records" onToggle={() => setOpen((o) => !o)} />
      <AppRecords id="records" open={open} onClose={() => setOpen(false)} {...props} />
    </>
  )
}

const renderRecords = (over: Partial<RecordsProps> = {}) =>
  render(
    <Records
      base={base}
      appId={9}
      access={manager}
      writable
      definition={definition}
      release={3}
      onRestored={() => {}}
      {...over}
    />,
  )
// The record open in its drawer, and one of its fields by name (a required field's label carries a mark).
const drawer = (id = 4) => screen.getByRole('region', { name: `Record ${id}` })
const field = (name: string) => within(drawer()).getByLabelText(new RegExp(`^${name}`))
const openExplorer = () => fireEvent.click(screen.getByRole('button', { name: 'Records and history' }))

test('pages by cursor, with the total and the sums of every matching record, not of the page', async () => {
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: (call) => {
      const q = params(call.path)
      const aggregates = [
        { kind: 'sum', field: 'amount', value: 1234.5, count: 40 },
        { kind: 'avg', field: 'amount', value: 30.86, count: 40 },
        { kind: 'min', field: 'amount', value: 1, count: 40 },
        { kind: 'max', field: 'amount', value: 99, count: 40 },
      ]
      return q.get('cursor') === 'c2'
        ? { body: page([record(1, 1, { description: 'Taxi', amount: 8 })], { total: 40, aggregates }) }
        : {
            body: page([record(4, 2, { description: 'Lunch', amount: 12.5 })], {
              total: 40,
              next_cursor: 'c2',
              aggregates,
            }),
          }
    },
  })
  renderRecords()
  openExplorer()
  await screen.findByText('Lunch')
  // The totals are of all 40 matching records, and say so.
  expect((await screen.findByRole('status')).textContent).toBe('Showing 1–1 of 40 matching records')
  const totals = screen.getByRole('region', { name: 'Totals' })
  expect(totals.textContent).toContain('Over all 40 matching records, not just this page.')
  expect(totals.textContent).toContain('1,234.5')
  expect(calls.at(-1)?.path).toContain('aggregate=sum%3Aamount')

  fireEvent.click(screen.getByRole('button', { name: 'Next page' }))
  await screen.findByText('Taxi')
  expect(params(calls.at(-1)?.path ?? '').get('cursor')).toBe('c2')
  expect(screen.getByRole('region', { name: 'Totals' }).textContent).toContain('1,234.5')
  expect((screen.getByRole('button', { name: 'Next page' }) as HTMLButtonElement).disabled).toBe(true)

  fireEvent.click(screen.getByRole('button', { name: 'Previous page' }))
  await screen.findByText('Lunch')
})

test('sorts by a column and filters by a condition, asking the server', async () => {
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Lunch', amount: 12.5 })]) },
  })
  renderRecords()
  openExplorer()
  await screen.findByText('Lunch')
  expect(params(calls.at(-1)?.path ?? '').get('sort')).toBe('_updated')
  expect(params(calls.at(-1)?.path ?? '').get('dir')).toBe('desc')

  fireEvent.click(screen.getByRole('button', { name: 'amount' }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').get('sort')).toBe('amount'))
  expect(params(calls.at(-1)?.path ?? '').get('dir')).toBe('asc')
  fireEvent.click(screen.getByRole('button', { name: 'amount' }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').get('dir')).toBe('desc'))

  const form = screen.getByRole('form', { name: 'Filter the records' })
  fireEvent.change(within(form).getByLabelText('Field'), { target: { value: 'amount' } })
  fireEvent.change(within(form).getByLabelText('Condition'), { target: { value: 'gt' } })
  fireEvent.change(within(form).getByLabelText('Value'), { target: { value: '10' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add filter' }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').getAll('filter')).toEqual(['amount:gt:10']))
  expect(screen.getByRole('list', { name: 'Filters in use' }).textContent).toContain('amount is more than 10')

  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'lunch' } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').get('q')).toBe('lunch'))

  fireEvent.click(screen.getByRole('button', { name: /Remove filter/ }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').getAll('filter')).toEqual([]))
})

test('restores a deleted record from the explorer with the revision it saw, and tells the screen', async () => {
  let deleted = [record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z', deleted_by: ana })]
  const restored = vi.fn()
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: () => ({ body: page(deleted) }),
    [`GET ${base}/records/4/history`]: () => ({
      body: { record: deleted[0] ?? record(4, 4, { description: 'Lunch' }), history: [], truncated: false },
    }),
    [`POST ${base}/records/4/restore`]: () => {
      deleted = []
      return { body: record(4, 4, { description: 'Lunch' }) }
    },
  })
  renderRecords({ onRestored: restored })
  openExplorer()
  fireEvent.click(await screen.findByRole('tab', { name: 'Deleted' }))
  expect(params(calls.find((c) => c.path.includes('state=deleted'))?.path ?? '').get('state')).toBe('deleted')
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Restore record 4' }))
  await waitFor(() => expect(restored).toHaveBeenCalledTimes(1))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ expected_revision: 3 })
  await screen.findByText('No deleted records.')
})

test('a restore the server refuses is told, translated', async () => {
  fakeFetch({
    [`GET ${base}/records/query`]: {
      body: page([record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z', deleted_by: ana })]),
    },
    [`GET ${base}/records/4/history`]: {
      body: {
        record: record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z' }),
        history: [],
        truncated: false,
      },
    },
    [`POST ${base}/records/4/restore`]: {
      status: 409,
      body: { error: 'the record changed since you read it', code: 'app_record_conflict', params: { id: 4 } },
    },
  })
  const restored = vi.fn()
  renderRecords({ onRestored: restored })
  openExplorer()
  fireEvent.click(await screen.findByRole('tab', { name: 'Deleted' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Restore record 4' }))
  expect((await screen.findByRole('alert')).textContent).toContain('Someone else changed this record')
  expect(restored).not.toHaveBeenCalled()
})

test('shows the history of a record, and offers no restore or edit without the right to change records', async () => {
  fakeFetch({
    [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Lunch', amount: 12.5 })]) },
    [`GET ${base}/records/4/history`]: {
      body: {
        record: record(4, 2, { description: 'Lunch', amount: 12.5 }),
        history: [
          {
            revision: 2,
            operation: 'update',
            data: { description: 'Lunch', amount: 12.5 },
            at: '2026-11-02T10:00:00.000Z',
            by: ana,
            action: 'edit',
          },
          {
            revision: 1,
            operation: 'insert',
            data: { description: 'Lunch', amount: 10 },
            at: '2026-11-01T10:00:00.000Z',
            by: { id: 2, name: 'Rui', agent: true },
            synthetic: true,
          },
        ],
        truncated: false,
      },
    },
  })
  const reader: AppAccess = { ...manager, roles: ['reader'], submit: false, modify_records: 'none' }
  const { unmount } = renderRecords({ access: reader })
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  const versions = within(await screen.findByRole('list', { name: 'History of record 4' })).getAllByRole('listitem')
  expect(versions).toHaveLength(2)
  expect(versions[0].textContent).toContain('Changed')
  expect(versions[1].textContent).toContain('Rui (through the agent)')
  expect(versions[1].textContent).toContain('amount: 10')
  expect(versions[1].textContent).toContain('Made before history was kept')
  // No inputs, no save, no delete, no restore.
  expect(within(drawer()).queryByRole('textbox')).toBeNull()
  expect(within(drawer()).queryByRole('button', { name: /Save|Delete|Restore/ })).toBeNull()
  unmount()

  const builder: AppAccess = { ...manager, roles: ['builder'], read_records: 'none', modify_records: 'none' }
  renderRecords({ access: builder })
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull())
})

test('on a phone: cards, an editor that sends the revision it saw, and a conflict that keeps what was typed', async () => {
  phone()
  let revision = 2
  let current: Record<string, unknown> = { description: 'Lunch', amount: 12.5, kind: 'food' }
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: () => ({ body: page([record(4, revision, current)]) }),
    [`GET ${base}/records/4/history`]: () => ({
      body: { record: record(4, revision, current), history: [], truncated: false },
    }),
    [`POST ${base}/actions`]: (call) => {
      const sent = call.body as { expectedRevision: number; context: Record<string, unknown> }
      if (sent.expectedRevision !== revision) {
        return {
          status: 409,
          body: {
            error: 'the record changed since you read it',
            code: 'app_record_conflict',
            params: { id: 4 },
            current: { id: 4, _revision: revision, ...current },
          },
        }
      }
      const { id: _id, ...changes } = sent.context
      current = { ...current, ...changes }
      revision += 1
      return { body: [] }
    },
  })
  renderRecords()
  openExplorer()
  // A card, not a table.
  expect(await screen.findByText('Lunch')).toBeTruthy()
  expect(screen.queryByRole('table')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Open record 4' }))

  // Someone else changes the amount and the kind, and the description too, meanwhile.
  const description = (await waitFor(() => field('description'))) as HTMLInputElement
  fireEvent.change(description, { target: { value: 'Team lunch' } })
  revision = 3
  current = { description: 'Business lunch', amount: 20, kind: 'travel' }
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))

  // Refused: the sent edit named the revision seen, and the input is still there.
  const conflict = await screen.findByRole('region', { name: 'Someone else changed this record meanwhile' })
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
  const first = calls.find((c) => c.method === 'POST')
  expect((first?.body as { expectedRevision: number } | undefined)?.expectedRevision).toBe(2)
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')
  // The current values are shown beside it, and the untouched fields now show the current record's.
  expect(conflict.textContent).toContain('Team lunch')
  expect(conflict.textContent).toContain('Business lunch')
  expect((field('amount') as HTMLInputElement).value).toBe('20')
  expect((field('kind') as HTMLSelectElement).value).toBe('travel')
  expect(screen.getByRole('alert').textContent).toContain('Someone else changed this record')

  // Saving over it names the current revision and sends only what the person changed.
  fireEvent.click(screen.getByRole('button', { name: 'Save my changes over the current version' }))
  await waitFor(() => expect(within(drawer()).getByRole('status').textContent).toBe('Saved.'))
  const second = calls.filter((c) => c.method === 'POST')[1]?.body as {
    expectedRevision: number
    context: Record<string, unknown>
    name: string
    release: number
  }
  expect(second.expectedRevision).toBe(3)
  expect(second.context).toEqual({ id: 4, description: 'Team lunch' })
  expect(second.name).toBe('edit')
  expect(second.release).toBe(3)
  expect(current).toEqual({ description: 'Team lunch', amount: 20, kind: 'travel' })
})

test('dropping an edit after a conflict goes back to the current record', async () => {
  phone()
  let revision = 2
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: () => ({
      body: page([record(4, revision, { description: revision === 2 ? 'Lunch' : 'Dinner' })]),
    }),
    [`GET ${base}/records/4/history`]: () => ({
      body: {
        record: record(4, revision, { description: revision === 2 ? 'Lunch' : 'Dinner' }),
        history: [],
        truncated: false,
      },
    }),
    [`POST ${base}/actions`]: () => ({
      status: 409,
      body: {
        error: 'x',
        code: 'app_record_conflict',
        params: { id: 4 },
        current: { id: 4, _revision: 3, description: 'Dinner' },
      },
    }),
  })
  renderRecords()
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  fireEvent.change(await waitFor(() => field('description')), { target: { value: 'Brunch' } })
  revision = 3
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await screen.findByRole('region', { name: 'Someone else changed this record meanwhile' })
  expect((field('description') as HTMLInputElement).value).toBe('Brunch')
  fireEvent.click(screen.getByRole('button', { name: 'Drop my changes and use the current record' }))
  await waitFor(() => expect((field('description') as HTMLInputElement).value).toBe('Dinner'))
  expect(screen.queryByRole('region', { name: 'Someone else changed this record meanwhile' })).toBeNull()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
})

const captured: AppDefinition = {
  collection: {
    fields: [
      { name: 'amount', type: 'number' },
      { name: 'code', type: 'barcode' },
      { name: 'place', type: 'location' },
      { name: 'signed', type: 'signature' },
      { name: 'photos', type: 'attachment' },
    ],
  },
  actions: { add: { kind: 'insert' }, edit: { kind: 'update' } },
}

test('only the fields the server can put in order are offered to sort by', async () => {
  fakeFetch({ [`GET ${base}/records/query`]: { body: page([record(4, 2, { amount: 12.5 })]) } })
  renderRecords({ definition: captured })
  openExplorer()
  await screen.findByText('12.5')
  // The table: a column to sort by for what can be, a plain heading for what can't.
  for (const sortable of ['amount', 'code']) expect(screen.getByRole('button', { name: sortable })).toBeTruthy()
  for (const evidence of ['place', 'signed', 'photos']) {
    expect(screen.queryByRole('button', { name: evidence })).toBeNull()
    expect(screen.getByRole('columnheader', { name: evidence })).toBeTruthy()
  }
})

test('on a phone the sort menu leaves out what the server refuses to sort by', async () => {
  phone()
  fakeFetch({ [`GET ${base}/records/query`]: { body: page([record(4, 2, { amount: 12.5 })]) } })
  renderRecords({ definition: captured })
  openExplorer()
  const menu = (await screen.findByLabelText('Sort by')) as HTMLSelectElement
  expect([...menu.options].map((o) => o.value)).toEqual(['_updated', 'amount', 'code'])
})

test('the filter offers only the fields the server can filter by, each with its own conditions', async () => {
  fakeFetch({ [`GET ${base}/records/query`]: { body: page([record(4, 2, { amount: 12.5 })]) } })
  renderRecords({ definition: captured })
  openExplorer()
  await screen.findByText('12.5')
  const form = screen.getByRole('form', { name: 'Filter the records' })
  const field = within(form).getByLabelText('Field') as HTMLSelectElement
  // Neither a photo nor evidence (a location, a signature) can be filtered by: the server refuses it.
  expect([...field.options].map((o) => o.value)).toEqual(['amount', 'code'])
  const conditions = () =>
    [...(within(form).getByLabelText('Condition') as HTMLSelectElement).options].map((o) => o.value)
  expect(conditions()).toEqual(['eq', 'ne', 'lt', 'lte', 'gt', 'gte'])
  fireEvent.change(field, { target: { value: 'code' } })
  expect(conditions()).toEqual(['eq', 'ne', 'contains'])
})

const labelled: AppDefinition = {
  collection: {
    fields: [
      { name: 'description', type: 'string' },
      { name: 'kind', type: 'choice', options: ['food', 'travel'], option_labels: { food: 'Food', travel: 'Travel' } },
    ],
  },
  actions: { add: { kind: 'insert' }, edit: { kind: 'update' } },
}

test('a choice reads by its label in the table, the filter and the record, and is stored and asked for as it is', async () => {
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Lunch', kind: 'food' })]) },
    [`GET ${base}/records/4/history`]: {
      body: {
        record: record(4, 2, { description: 'Lunch', kind: 'food' }),
        history: [
          {
            revision: 1,
            operation: 'insert',
            data: { description: 'Lunch', kind: 'travel' },
            at: '2026-11-01T10:00:00.000Z',
            by: ana,
          },
        ],
        truncated: false,
      },
    },
  })
  renderRecords({ definition: labelled })
  openExplorer()
  const row = (await screen.findByText('Lunch')).closest('tr') as HTMLElement
  expect(within(row).getByText('Food')).toBeTruthy()
  expect(within(row).queryByText('food')).toBeNull()

  // The filter's value picker offers the labels and sends the stored option.
  const form = screen.getByRole('form', { name: 'Filter the records' })
  fireEvent.change(within(form).getByLabelText('Field'), { target: { value: 'kind' } })
  const value = within(form).getByLabelText('Value') as HTMLSelectElement
  expect([...value.options].map((o) => o.textContent)).toEqual(['Value', 'Food', 'Travel'])
  fireEvent.change(value, { target: { value: 'food' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add filter' }))
  await waitFor(() => expect(params(calls.at(-1)?.path ?? '').getAll('filter')).toEqual(['kind:eq:food']))
  expect(screen.getByRole('list', { name: 'Filters in use' }).textContent).toContain('kind is Food')

  // The record: its picker, and its history.
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  const picker = (await waitFor(() => field('kind'))) as HTMLSelectElement
  expect([...picker.options].map((o) => o.textContent)).toEqual(['Not set', 'Food', 'Travel'])
  expect(picker.value).toBe('food')
  const versions = within(await screen.findByRole('list', { name: 'History of record 4' })).getAllByRole('listitem')
  expect(versions[0].textContent).toContain('kind: Travel')
})

test('a stored option with no label, or that the app no longer lists, reads as it is stored', async () => {
  fakeFetch({ [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Walk', kind: 'gone' })]) } })
  renderRecords({
    definition: {
      ...labelled,
      collection: {
        fields: [
          labelled.collection.fields[0],
          { name: 'kind', type: 'choice', options: ['food', 'travel'], option_labels: { food: 'Food' } },
        ],
      },
    },
  })
  openExplorer()
  const row = (await screen.findByText('Walk')).closest('tr') as HTMLElement
  expect(within(row).getByText('gone')).toBeTruthy()
})

test('a record opens in a drawer beside the list; deleting it asks first, and a cancel sends nothing', async () => {
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Lunch', amount: 12.5 })]) },
    [`GET ${base}/records/4/history`]: {
      body: { record: record(4, 2, { description: 'Lunch', amount: 12.5 }), history: [], truncated: false },
    },
    [`POST ${base}/actions`]: { body: [] },
  })
  const changed = vi.fn()
  renderRecords({ onRestored: changed })
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  await waitFor(() => field('description'))
  // The list is still there behind it.
  expect(screen.getByRole('table')).toBeTruthy()

  fireEvent.click(within(drawer()).getByRole('button', { name: 'Delete' }))
  const ask = await screen.findByRole('alertdialog', { name: 'Delete record 4?' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(calls.some((c) => c.method === 'POST')).toBe(false)

  fireEvent.click(within(drawer()).getByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog', { name: 'Delete record 4?' })).getByRole('button', {
      name: 'Yes, delete this record',
    }),
  )
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(1))
  const sent = calls.find((c) => c.method === 'POST')?.body as {
    name: string
    context: unknown
    expectedRevision: number
  }
  expect(sent.name).toBe('remove')
  expect(sent.context).toEqual({ id: 4 })
  expect(sent.expectedRevision).toBe(2)
  // Deleted: the drawer is closed, and the keyboard is on what the list holds now (its row is gone).
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Record 4' })).toBeNull())
  await waitFor(() => expect(document.activeElement?.textContent).toMatch(/matching records/))
})

// Two records, the first open in the drawer with its description changed and not saved.
async function editing(over: { conflict?: boolean } = {}) {
  let revision = 2
  const current = { description: 'Lunch', amount: 12.5, kind: 'food' }
  const calls = fakeFetch({
    [`GET ${base}/records/query`]: {
      body: page([record(4, revision, current), record(5, 1, { description: 'Taxi', amount: 8 })]),
    },
    [`GET ${base}/records/4/history`]: () => ({
      body: { record: record(4, revision, current), history: [], truncated: false },
    }),
    [`GET ${base}/records/5/history`]: {
      body: { record: record(5, 1, { description: 'Taxi', amount: 8 }), history: [], truncated: false },
    },
    [`POST ${base}/actions`]: () => {
      revision += 1
      return {
        status: 409,
        body: {
          error: 'the record changed since you read it',
          code: 'app_record_conflict',
          params: { id: 4 },
          current: { id: 4, _revision: revision, ...current, description: 'Business lunch' },
        },
      }
    },
  })
  renderRecords()
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  const description = (await waitFor(() => field('description'))) as HTMLInputElement
  fireEvent.change(description, { target: { value: 'Team lunch' } })
  if (over.conflict) {
    fireEvent.click(within(drawer()).getByRole('button', { name: 'Save changes' }))
    await within(drawer()).findByRole('region', { name: 'Someone else changed this record meanwhile' })
  }
  return { calls, description }
}

const leaving = () => screen.findByRole('alertdialog', { name: 'Discard your changes to this record?' })
const pressEscape = (el: Element) => fireEvent.keyDown(el, { key: 'Escape' })

test('with a change not saved, Escape asks first; keeping is the default and keeps the text', async () => {
  await editing()
  // From the drawer's own buttons (Escape in a field is the field's, below).
  const close = within(drawer()).getByRole('button', { name: 'Close' })
  close.focus()
  pressEscape(close)
  const ask = await leaving()
  // Keeping is what the dialog starts on.
  expect(document.activeElement).toBe(within(ask).getByRole('button', { name: 'Keep editing' }))
  fireEvent.click(within(ask).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')

  // Discarding closes it.
  pressEscape(close)
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Discard my changes' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Record 4' })).toBeNull())
})

test('Escape in a field is the field’s (a text, a choice, the list’s search): it never closes the drawer', async () => {
  const { description } = await editing()
  for (const el of [description, field('kind'), screen.getByRole('searchbox')]) {
    pressEscape(el)
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(drawer()).toBeTruthy()
  }
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')
})

test('closing the drawer with its button, with a change not saved, asks first', async () => {
  await editing()
  fireEvent.click(within(drawer()).getByRole('button', { name: 'Close' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')
})

test('sorting the list with a change not saved asks first, and asks the server nothing until discarded', async () => {
  const { calls } = await editing()
  const before = calls.filter((c) => c.path.includes('/records/query')).length
  fireEvent.click(screen.getByRole('button', { name: 'amount' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(calls.filter((c) => c.path.includes('/records/query')).length).toBe(before)
  expect(drawer()).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'amount' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Discard my changes' }))
  await waitFor(() => expect(calls.filter((c) => c.path.includes('/records/query')).length).toBe(before + 1))
  expect(screen.queryByRole('region', { name: 'Record 4' })).toBeNull()
})

test('opening another record with a change not saved asks first', async () => {
  await editing()
  fireEvent.click(screen.getByRole('button', { name: 'Open record 5' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')
  expect(screen.queryByRole('region', { name: 'Record 5' })).toBeNull()
})

test('with a conflict on screen, Escape asks first: neither version is lost without a choice', async () => {
  await editing({ conflict: true })
  pressEscape(drawer())
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(within(drawer()).getByRole('region', { name: 'Someone else changed this record meanwhile' })).toBeTruthy()
  expect((field('description') as HTMLInputElement).value).toBe('Team lunch')
})

test('with nothing changed, Escape closes the drawer at once', async () => {
  fakeFetch({
    [`GET ${base}/records/query`]: { body: page([record(4, 2, { description: 'Lunch' })]) },
    [`GET ${base}/records/4/history`]: {
      body: { record: record(4, 2, { description: 'Lunch' }), history: [], truncated: false },
    },
  })
  renderRecords()
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  await waitFor(() => field('description'))
  pressEscape(drawer())
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Record 4' })).toBeNull())
  expect(screen.queryByRole('alertdialog')).toBeNull()
})

test('the record’s form leaves required fields to the server: the browser never blocks a save', async () => {
  await editing()
  // description is required in the definition: the mark shows it and aria-required says it, while the
  // input itself doesn't refuse an empty value (the server answers that, as it always has).
  const description = field('description') as HTMLInputElement
  expect(description.required).toBe(false)
  expect(description.getAttribute('aria-required')).toBe('true')
  expect(within(drawer()).getByText('description').closest('.ui-field')?.textContent).toContain('*')
  // A field that isn't required says nothing of it.
  expect(field('amount').getAttribute('aria-required')).toBeNull()
})

test('a delete that finishes after another record was opened closes nothing, and what was typed there stays', async () => {
  let finish: (a: { body: unknown }) => void = () => {}
  fakeFetch({
    [`GET ${base}/records/query`]: {
      body: page([record(4, 2, { description: 'Lunch' }), record(5, 1, { description: 'Taxi' })]),
    },
    [`GET ${base}/records/4/history`]: {
      body: { record: record(4, 2, { description: 'Lunch' }), history: [], truncated: false },
    },
    [`GET ${base}/records/5/history`]: {
      body: { record: record(5, 1, { description: 'Taxi' }), history: [], truncated: false },
    },
    // The delete's answer comes when the test says.
    [`POST ${base}/actions`]: () =>
      new Promise((resolve) => {
        finish = resolve
      }),
  })
  renderRecords()
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  await waitFor(() => field('description'))
  fireEvent.click(within(drawer(4)).getByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog', { name: 'Delete record 4?' })).getByRole('button', {
      name: 'Yes, delete this record',
    }),
  )
  // While it is on its way, the person opens another record and types in it.
  fireEvent.click(screen.getByRole('button', { name: 'Open record 5' }))
  const taxi = (await waitFor(() => within(drawer(5)).getByLabelText(/^description/))) as HTMLInputElement
  fireEvent.change(taxi, { target: { value: 'Taxi to the station' } })
  // The delete of record 4 succeeds now: record 5 stays open, with its text.
  await act(async () => finish({ body: [] }))
  await new Promise((r) => setTimeout(r, 0))
  expect(screen.getByRole('region', { name: 'Record 5' })).toBeTruthy()
  expect((within(drawer(5)).getByLabelText(/^description/) as HTMLInputElement).value).toBe('Taxi to the station')
})

test('on a phone, choosing a sort with a change not saved applies the sort chosen once discarded', async () => {
  phone()
  const { calls } = await editing()
  fireEvent.change(screen.getByLabelText('Sort by'), { target: { value: 'amount' } })
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Discard my changes' }))
  await waitFor(() =>
    expect(calls.some((c) => c.path.includes('/records/query') && params(c.path).get('sort') === 'amount')).toBe(true),
  )
})

test('a filter not added after all (the person kept a record’s changes) keeps its typed value', async () => {
  await editing()
  const form = screen.getByRole('form', { name: 'Filter the records' })
  fireEvent.change(within(form).getByLabelText('Value'), { target: { value: 'Taxi' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add filter' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect((within(form).getByLabelText('Value') as HTMLInputElement).value).toBe('Taxi')
  expect(screen.queryByRole('list', { name: 'Filters in use' })).toBeNull()

  // Added after all: the value goes, and the filter is in use.
  fireEvent.click(within(form).getByRole('button', { name: 'Add filter' }))
  fireEvent.click(within(await leaving()).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('list', { name: 'Filters in use' })
  expect((within(form).getByLabelText('Value') as HTMLInputElement).value).toBe('')
})

test('a delete that finishes after the same record was opened again closes nothing of the new opening', async () => {
  let finish: (a: { body: unknown }) => void = () => {}
  fakeFetch({
    [`GET ${base}/records/query`]: {
      body: page([record(4, 2, { description: 'Lunch' }), record(5, 1, { description: 'Taxi' })]),
    },
    [`GET ${base}/records/4/history`]: {
      body: { record: record(4, 2, { description: 'Lunch' }), history: [], truncated: false },
    },
    [`GET ${base}/records/5/history`]: {
      body: { record: record(5, 1, { description: 'Taxi' }), history: [], truncated: false },
    },
    [`POST ${base}/actions`]: () =>
      new Promise((resolve) => {
        finish = resolve
      }),
  })
  renderRecords()
  openExplorer()
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  await waitFor(() => field('description'))
  fireEvent.click(within(drawer(4)).getByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog', { name: 'Delete record 4?' })).getByRole('button', {
      name: 'Yes, delete this record',
    }),
  )
  // While it is on its way: another record, then record 4 again, and a change typed in it.
  fireEvent.click(screen.getByRole('button', { name: 'Open record 5' }))
  await waitFor(() => within(drawer(5)).getByLabelText(/^description/))
  fireEvent.click(screen.getByRole('button', { name: 'Open record 4' }))
  const again = (await waitFor(() => within(drawer(4)).getByLabelText(/^description/))) as HTMLInputElement
  fireEvent.change(again, { target: { value: 'Lunch with the team' } })
  // The first opening's delete succeeds now: the new opening stays, with what was typed.
  await act(async () => finish({ body: [] }))
  await new Promise((r) => setTimeout(r, 0))
  expect(screen.getByRole('region', { name: 'Record 4' })).toBeTruthy()
  expect((within(drawer(4)).getByLabelText(/^description/) as HTMLInputElement).value).toBe('Lunch with the team')
})
