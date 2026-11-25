// @vitest-environment jsdom

// The frame of a launched app: its name heads the page, the way back and the ways to its records and
// to editing are in its header, the app's own primary button is the only primary on the page, and
// opening the records takes the keyboard to them.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { fakeFetch } from '../test/fakes'
import { LaunchedApp } from './LaunchedApp'
import { forgetOperations } from './operations'

afterEach(() => {
  cleanup()
  forgetOperations()
  vi.unstubAllGlobals()
})

const base = '/api/workspaces/1/spaces/2/apps/1'
const everything = {
  roles: ['builder', 'publisher', 'user', 'data_manager', 'reader'],
  restricted: false,
  view: true,
  design: true,
  publish: true,
  submit: true,
  manage_roles: true,
  read_records: 'all',
  modify_records: 'all',
}
const definition = { collection: { fields: [{ name: 'description', type: 'string' }] }, actions: {} }
const app = (over: Record<string, unknown> = {}) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
  access: everything,
  live: {
    number: 1,
    definition_version: 1,
    requires: ['component.Column', 'component.Text'],
    definition,
    published_at: '2026-11-18T10:00:00.000Z',
    published_by: { id: 1, name: 'Ann', agent: false },
    live: true,
  },
  unpublished_changes: false,
  draft: { definition_version: 1, requires: [] },
  ...over,
})

function serve(over: Record<string, unknown> = {}) {
  return fakeFetch({
    [`GET ${base}`]: { body: app(over) },
    [`GET ${base}/surface?release=1`]: { body: surface },
    [`GET ${base}/effects`]: { body: { effects: [], active: 0, failed: 0 } },
    [`GET ${base}/records/query`]: {
      body: { records: [], next_cursor: null, total: 0, aggregates: [] },
    },
  })
}

test("the app's name heads the page, with the way back, to its records and to editing it", async () => {
  serve()
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const heading = await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  const header = heading.closest('header') as HTMLElement
  expect(within(header).getByRole('link', { name: 'All apps' }).getAttribute('href')).toBe('/w/1/apps')
  expect(within(header).getByRole('link', { name: 'Edit Expenses with AI' })).toBeTruthy()
  expect(within(header).getByRole('button', { name: 'Records and history' })).toBeTruthy()
})

test("nothing the frame draws is a primary button: the app's own is the page's one", async () => {
  serve()
  const { container } = render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByText('Coffee')
  // The renderer draws the app's own buttons; every button of the library here is the frame's.
  expect(container.querySelectorAll('.ui-button').length).toBeGreaterThan(0)
  expect(container.querySelectorAll('.ui-button-primary')).toHaveLength(0)
})

test('opening the records shows them under the app and takes the keyboard there; closing gives it back', async () => {
  serve()
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const toggle = await screen.findByRole('button', { name: 'Records and history' })
  expect(toggle.getAttribute('aria-expanded')).toBe('false')
  fireEvent.click(toggle)
  const panel = await screen.findByRole('region', { name: 'Records and history' })
  expect(toggle.getAttribute('aria-expanded')).toBe('true')
  expect(toggle.getAttribute('aria-controls')).toBe(panel.id)
  await waitFor(() => expect(document.activeElement).toBe(within(panel).getByRole('heading', { level: 2 })))
  await within(panel).findByText('No records yet.')

  fireEvent.click(within(panel).getByRole('button', { name: 'Hide records and history' }))
  expect(screen.queryByRole('region', { name: 'Records and history' })).toBeNull()
  expect(toggle.getAttribute('aria-expanded')).toBe('false')
})

test('a reader without records sees no way to them, and a viewer no way to edit', async () => {
  serve({ access: { ...everything, design: false, read_records: 'none' } })
  render(<LaunchedApp workspace={1} space={2} app={1} writable={false} />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull()
  expect(screen.queryByRole('link', { name: /with AI/ })).toBeNull()
  // What they may do is said under the name.
  expect(screen.getByText(/You can view this app/)).toBeTruthy()
})

test('an offline app says so, and its restore is the one primary action', async () => {
  serve({ archived_at: '2026-11-18T00:00:00.000Z' })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const restore = await screen.findByRole('button', { name: 'Restore' })
  expect(restore.className).toContain('ui-button-primary')
  expect(screen.getByText(/This app is offline/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull()
})
