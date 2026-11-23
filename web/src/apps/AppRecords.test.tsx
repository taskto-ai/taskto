// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { AppAccess } from '../api'
import { fakeFetch } from '../test/fakes'
import { AppRecords } from './AppRecords'

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

test('shows the records and the history of one', async () => {
  fakeFetch({
    [`GET ${base}/records`]: {
      body: { records: [record(4, 2, { description: 'Lunch', amount: 12.5 })], truncated: false },
    },
    [`GET ${base}/records/4/history`]: {
      body: {
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
  render(<AppRecords base={base} access={manager} writable onRestored={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: 'Records and history' }))
  await screen.findByText('Record 4')
  expect(screen.getByText('description: Lunch · amount: 12.5')).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'History of record 4' }))
  const versions = within(await screen.findByRole('list', { name: 'History of record 4' })).getAllByRole('listitem')
  expect(versions).toHaveLength(2)
  expect(versions[0].textContent).toContain('Changed')
  expect(versions[0].textContent).toContain('revision 2')
  expect(versions[1].textContent).toContain('Created')
  expect(versions[1].textContent).toContain('Rui (through the agent)')
  expect(versions[1].textContent).toContain('amount: 10')
  expect(versions[1].textContent).toContain('Made before history was kept')
})

test('restores a deleted record with the revision it saw, and tells the screen', async () => {
  let deleted = [record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z', deleted_by: ana })]
  const restored = vi.fn()
  const calls = fakeFetch({
    [`GET ${base}/records/deleted`]: () => ({ body: { records: deleted, truncated: false } }),
    [`POST ${base}/records/4/restore`]: () => {
      deleted = []
      return { body: record(4, 4, { description: 'Lunch' }) }
    },
  })
  render(<AppRecords base={base} access={manager} writable onRestored={restored} />)
  fireEvent.click(screen.getByRole('button', { name: 'Records and history' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Deleted' }))
  await screen.findByText(/Deleted .* by Ana/)
  fireEvent.click(screen.getByRole('button', { name: 'Restore record 4' }))
  await screen.findByText('Record 4 restored.')
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ expected_revision: 3 })
  expect(restored).toHaveBeenCalledTimes(1)
  await screen.findByText('No deleted records.')
})

test('a restore the server refuses is told, translated', async () => {
  fakeFetch({
    [`GET ${base}/records/deleted`]: {
      body: {
        records: [record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z', deleted_by: ana })],
        truncated: false,
      },
    },
    [`POST ${base}/records/4/restore`]: {
      status: 409,
      body: { error: 'the record changed since you read it', code: 'app_record_conflict', params: { id: 4 } },
    },
  })
  const restored = vi.fn()
  render(<AppRecords base={base} access={manager} writable onRestored={restored} />)
  fireEvent.click(screen.getByRole('button', { name: 'Records and history' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Deleted' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Restore record 4' }))
  expect((await screen.findByRole('alert')).textContent).toContain('Someone else changed this record')
  expect(restored).not.toHaveBeenCalled()
})

test('offers no restore without the right to change records, and nothing without reading them', async () => {
  fakeFetch({
    [`GET ${base}/records/deleted`]: {
      body: {
        records: [record(4, 3, { description: 'Lunch' }, { deleted_at: '2026-11-03T10:00:00.000Z', deleted_by: ana })],
        truncated: false,
      },
    },
  })
  const reader: AppAccess = { ...manager, roles: ['reader'], submit: false, modify_records: 'none' }
  const { unmount } = render(<AppRecords base={base} access={reader} writable onRestored={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: 'Records and history' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Deleted' }))
  await screen.findByText('Record 4')
  expect(screen.queryByRole('button', { name: /Restore/ })).toBeNull()
  unmount()

  const builder: AppAccess = { ...manager, roles: ['builder'], read_records: 'none', modify_records: 'none' }
  render(<AppRecords base={base} access={builder} writable onRestored={() => {}} />)
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull())
})
