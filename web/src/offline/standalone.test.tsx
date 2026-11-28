// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { App } from '../App'
import type { OfflineApp } from '../api'
import { forgetOperations } from '../apps/operations'
import { setPreferences } from '../i18n'
import { OfflineShell } from './OfflineShell'
import { activate, deactivate, rememberUser, runtimeState, setSyncDeps } from './runtime'
import { type Download, scopeKey } from './store'

const definition: OfflineApp['definition'] = {
  definition_version: 1,
  requires: [],
  collection: {
    fields: [
      { name: 'description', type: 'string', required: true, max_length: 200 },
      { name: 'amount', type: 'number', required: true },
    ],
  },
  actions: { add_expense: { kind: 'insert' } },
}

function download(app: number, name: string): Download {
  const scope = { workspace: 1, space: 2, app }
  return {
    ...scope,
    key: scopeKey(scope),
    user: 7,
    name,
    release: 3,
    definition,
    messages: surface,
    scopeOf: 'all',
    records: 0,
    total: 0,
    truncated: false,
    canInsert: true,
    capturedAt: '2026-11-02T10:00:00.000Z',
    bytes: 100,
    modifiable: [],
    operations: {},
  }
}

function inputOf(label: string): HTMLInputElement {
  const input = screen.getByText(label).closest('a2ui-basic-textfield')?.querySelector('input')
  if (!input) throw new Error(`no input for ${label}`)
  return input
}

beforeEach(async () => {
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  rememberUser({ id: 7, name: 'Pat' })
  forgetOperations()
  document.title = 'taskto.ai'
  await setPreferences({})
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  await runtime.store.putDownload(download(1, 'Expenses'))
  await runtime.store.putDownload(download(2, 'Mileage'))
})

afterEach(async () => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '/')
  setSyncDeps({})
  await deactivate({ purge: false })
})

test('the app a run address names opens at once, as the whole page, with its name in the tab and a way to taskto', async () => {
  render(<OfflineShell user="Pat" onReconnect={() => {}} requested={{ workspace: 1, space: 2, app: 2 }} />)
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  expect(screen.getByRole('region', { name: 'Mileage' })).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Open Expenses' })).toBeNull()
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-2')
  await waitFor(() => expect(document.title).toBe('Mileage · taskto.ai'))
  fireEvent.click(screen.getByRole('button', { name: 'Back to the downloaded apps' }))
  expect(screen.getByRole('button', { name: 'Open Expenses' })).toBeTruthy()
  expect(screen.getByRole('heading', { level: 1, name: 'Offline' })).toBeTruthy()
  expect(document.title).toBe('taskto.ai')
})

test('a run address for an app that is not downloaded says so and keeps the list', async () => {
  render(<OfflineShell user="Pat" onReconnect={() => {}} requested={{ workspace: 1, space: 2, app: 9 }} />)
  await screen.findByText(/isn’t downloaded on this device/)
  expect(screen.getByRole('heading', { level: 1, name: 'Offline' })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Open Expenses' })).toBeTruthy()
  expect(screen.queryByRole('link', { name: 'Open in taskto' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Open Mileage' }))
  await screen.findByRole('button', { name: 'Back to the downloaded apps' })
  expect(screen.queryByRole('link', { name: 'Open in taskto' })).toBeNull()
})

test('a run address in another workspace does not open a download of the same space and app', async () => {
  render(<OfflineShell user="Pat" onReconnect={() => {}} requested={{ workspace: 5, space: 2, app: 1 }} />)
  await screen.findByText(/isn’t downloaded on this device/)
  expect(screen.queryByRole('heading', { level: 1, name: 'Expenses' })).toBeNull()
})

test('with no run address the list is shown and nothing is opened', async () => {
  render(<OfflineShell user="Pat" onReconnect={() => {}} />)
  await screen.findByRole('button', { name: 'Open Expenses' })
  expect(screen.queryByText(/isn’t downloaded on this device/)).toBeNull()
})

test('a cold start offline at a run address opens that app, not the list', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  window.history.replaceState(null, '', '/w/1/run/2-2')
  render(<App />)
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-2')
  expect(screen.queryByRole('button', { name: 'Open Expenses' })).toBeNull()
  expect(window.location.pathname).toBe('/w/1/run/2-2')
})

test('a cold start offline anywhere else shows the list', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  window.history.replaceState(null, '', '/w/1/apps/2-2')
  render(<App />)
  await screen.findByRole('button', { name: 'Open Mileage' })
  expect(screen.getByRole('heading', { level: 1, name: 'Offline' })).toBeTruthy()
})

test('"Open in taskto" offline leaves the standalone app for the list', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  window.history.replaceState(null, '', '/w/1/run/2-2')
  render(<App />)
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  fireEvent.click(screen.getByRole('link', { name: 'Open in taskto' }))
  expect(window.location.pathname).toBe('/w/1/apps/2-2')
  await screen.findByRole('button', { name: 'Open Mileage' })
  expect(screen.getByRole('heading', { level: 1, name: 'Offline' })).toBeTruthy()
  expect(screen.queryByRole('region', { name: 'Mileage' })).toBeNull()
  expect(screen.queryByRole('link', { name: 'Open in taskto' })).toBeNull()
})

test('the app shown follows the address when Back returns to a run address after an app was opened from the list', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  window.history.replaceState(null, '', '/w/1/run/2-2')
  render(<App />)
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  fireEvent.click(screen.getByRole('link', { name: 'Open in taskto' }))
  await screen.findByRole('button', { name: 'Open Expenses' })
  fireEvent.click(screen.getByRole('button', { name: 'Open Expenses' }))
  await screen.findByRole('button', { name: 'Back to the downloaded apps' })
  expect(screen.getByRole('region', { name: 'Expenses' })).toBeTruthy()

  act(() => {
    window.history.back()
  })
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/run/2-2'))
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  expect(screen.queryByRole('region', { name: 'Expenses' })).toBeNull()
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-2')
})

test('the notice that a save was kept stays with the app that was saved in when Back returns to the other app', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  window.history.replaceState(null, '', '/w/1/run/2-2')
  render(<App />)
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  fireEvent.click(screen.getByRole('link', { name: 'Open in taskto' }))
  await screen.findByRole('button', { name: 'Open Expenses' })
  fireEvent.click(screen.getByRole('button', { name: 'Open Expenses' }))
  await screen.findByRole('region', { name: 'Expenses' })
  await screen.findByText('Description')
  fireEvent.input(inputOf('Description'), { target: { value: 'Lunch' } })
  fireEvent.input(inputOf('Amount'), { target: { value: '12.25' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))
  const panel = () => within(document.querySelector('.ui-offline-app') as HTMLElement)
  await panel().findByText(/Saved on this device/)

  act(() => {
    window.history.back()
  })
  await screen.findByRole('heading', { level: 1, name: 'Mileage' })
  expect(screen.getByRole('region', { name: 'Mileage' })).toBeTruthy()
  expect(panel().queryByText(/Saved on this device/)).toBeNull()
})
