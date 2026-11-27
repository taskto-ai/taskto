// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { OfflineApp } from '../api'
import { tableItems, tableSurface } from '../apps/table.testing'
import { setPreferences } from '../i18n'
import { OfflineApp as OfflineAppView } from './OfflineShell'
import { deactivate, rememberUser, setSyncDeps } from './runtime'
import { type Download, OfflineStore, scopeKey } from './store'
import { SyncEngine } from './sync'
import { soleWriter } from './writer.testing'

const scope = { workspace: 1, space: 2, app: 1 }

const definition: OfflineApp['definition'] = {
  definition_version: 3,
  requires: ['component.Table'],
  collection: {
    fields: [
      { name: 'title', type: 'string', required: true },
      {
        name: 'category',
        type: 'choice',
        options: ['home', 'other', 'work'],
        option_labels: { home: 'Home', other: 'Other', work: 'Work' },
      },
      { name: 'due_date', type: 'date' },
      { name: 'amount', type: 'number' },
      { name: 'done', type: 'boolean' },
    ],
  },
  actions: { open_task: { kind: 'navigate', screen: 'detail' } },
  summaries: {},
}

const downloaded: Download = {
  ...scope,
  key: scopeKey(scope),
  user: 7,
  name: 'Tasks',
  release: 3,
  definition,
  messages: tableSurface(tableItems.map(({ _labels, ...item }) => item)),
  scopeOf: 'all',
  records: 3,
  total: 3,
  truncated: false,
  canInsert: false,
  capturedAt: '2026-11-02T10:00:00.000Z',
  bytes: 100,
  operations: {},
}

beforeEach(async () => {
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  rememberUser({ id: 7, name: 'Pat' })
  await setPreferences({})
})

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  setSyncDeps({})
  await deactivate({ purge: false })
})

test('a table draws from a downloaded app, with the labels the definition gives, and sorts without a connection', async () => {
  const store = await OfflineStore.open(7)
  await store.putDownload(downloaded)
  const engine = new SyncEngine(store, {
    now: () => 1_000_000,
    setTimer: () => 0,
    clearTimer: () => {},
    random: () => 0.5,
  })
  const runtime = { user: store.user, store, engine, stop: () => {}, writer: soleWriter }
  render(<OfflineAppView runtime={runtime} download={downloaded} items={[]} />)

  const table = await screen.findByRole('table', { name: 'Tasks' })
  const rows = () => within(table).getAllByRole('row').slice(1)
  const titles = () => rows().map((r) => within(r).getAllByRole('rowheader')[0].textContent)
  expect(titles()).toEqual(['Pay rent', 'Fix the shelf', 'Call the bank'])
  expect(
    within(rows()[0])
      .getAllByRole('cell')
      .map((c) => c.textContent),
  ).toEqual(['Home', 'Mar 8, 2026', '1,234.5', 'No'])
  expect(screen.getByRole('button', { name: 'Open Pay rent' })).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Due' }))
  expect(titles()).toEqual(['Fix the shelf', 'Pay rent', 'Call the bank'])
})
