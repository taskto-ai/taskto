// @vitest-environment jsdom
//
// Contract test with the Go server (internal/apps): the golden files there are the messages
// Go sends, the action it expects back from the button, and the catalog it checks saved apps
// against.

import { MessageProcessor, STRICT_VALIDATION } from '@a2ui/web_core/v0_9'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { z } from 'zod'
import goCatalog from '../../../internal/apps/testdata/catalog.json'
import expectedAction from '../../../internal/apps/testdata/expenses.action.json'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { fakeFetch } from '../test/fakes'
import { type ActionPayload, AppView, httpTransport } from './AppView'
import { appsCatalog, catalogCapabilities, DEFINITION_VERSION } from './catalog'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// The value shown next to a Stat's label.
function stat(label: string): string {
  return screen.getByText(label).closest('dl')?.querySelector('dd')?.textContent ?? ''
}

// A2UI's TextField doesn't link its <label> to its <input>, so getByLabelText can't find it.
function field(label: string): HTMLInputElement {
  const input = screen.getByText(label).closest('a2ui-basic-textfield')?.querySelector('input')
  if (!input) throw new Error(`no input for ${label}`)
  return input
}

test('draws a saved app, sends the button action, and shows the result', async () => {
  const sent: ActionPayload[] = []
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={{
        load: async () => surface,
        act: async (action) => {
          sent.push(action)
          return afterAdd
        },
      }}
    />,
  )

  await screen.findByText('Expense tracker')
  expect(stat('Total spent')).toBe('4.50')
  expect(screen.getByText('Coffee')).toBeTruthy()

  fireEvent.input(field('Description'), { target: { value: 'Lunch' } })
  fireEvent.input(field('Amount'), { target: { value: '12.25' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))

  await waitFor(() => expect(sent).toHaveLength(1))
  const { timestamp: _sentAt, ...action } = sent[0] as unknown as Record<string, unknown>
  const { timestamp: _goldenAt, ...golden } = expectedAction
  expect(action).toEqual(golden)

  await screen.findByText('Lunch')
  expect(stat('Total spent')).toBe('16.75')
})

test("a row's button sends the row's id", async () => {
  const sent: ActionPayload[] = []
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={{
        load: async () => surface,
        act: async (action) => {
          sent.push(action)
          return afterAdd
        },
      }}
    />,
  )
  await screen.findByText('Coffee')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(sent).toHaveLength(1))
  expect(sent[0]).toMatchObject({ name: 'delete_expense', surfaceId: 'app-1', context: { id: 1 } })
})

test('shows what the server refused', async () => {
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={{
        load: async () => surface,
        act: async () => {
          throw new Error('you can only read this space')
        },
      }}
    />,
  )
  await screen.findByText('Coffee')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  expect((await screen.findByRole('alert')).textContent).toBe('you can only read this space')
})

const strict = () => new MessageProcessor([appsCatalog], undefined, { validationConfig: STRICT_VALIDATION })

test('a new transport object on re-render does not reload the app', async () => {
  let loads = 0
  const transport = () => ({
    load: async () => {
      loads++
      return surface
    },
    act: async () => afterAdd,
  })
  const { rerender } = render(<AppView surfaceId="app-1" label="Expenses" transport={transport()} />)
  await screen.findByText('Expense tracker')
  rerender(<AppView surfaceId="app-1" label="Expenses" transport={transport()} />)
  rerender(<AppView surfaceId="app-1" label="Expenses" transport={transport()} />)
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(loads).toBe(1)
})

// The server checks saved apps against its catalog; the renderer must offer exactly the same
// components, props and functions, so what the server accepts draws, and nothing else does.
test('the catalog is the one the server checks apps against', () => {
  expect(appsCatalog.id).toBe(goCatalog.id)
  const components: Record<string, string[]> = {}
  for (const [name, api] of appsCatalog.components) {
    components[name] = Object.keys((api.schema as z.AnyZodObject).shape).sort()
  }
  expect(components).toEqual(goCatalog.components)
  expect([...appsCatalog.functions.keys()].sort()).toEqual(goCatalog.functions)
})

// What a release needs is named by the server's capabilities: the client must know the same set, and
// read the same definition format version, or it would draw apps it can't, or refuse ones it can.
test('the capabilities and definition version are the ones the server speaks', () => {
  expect([...catalogCapabilities].sort()).toEqual(goCatalog.capabilities)
  expect(DEFINITION_VERSION).toBe(goCatalog.definition_version)
})

const never = { load: vi.fn(async () => surface), act: vi.fn(async () => afterAdd) }

test('a release that needs a capability this client lacks shows "update required", not a broken app', async () => {
  never.load.mockClear()
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={never}
      release={{ definition_version: 1, requires: ['component.Text', 'component.Gauge', 'function.sparkline'] }}
    />,
  )
  await screen.findByText('Update required')
  expect(screen.getByText(/published with features this version of TaskTo.AI doesn’t have yet/)).toBeTruthy()
  // It says what is missing, not what the client has.
  expect(screen.getByText('Needs: component.Gauge, function.sparkline')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Update TaskTo.AI' })).toBeTruthy()
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(never.load).not.toHaveBeenCalled()
  expect(screen.queryByText('Coffee')).toBeNull()
})

test('a definition format newer than the client reads shows "update required" too', async () => {
  never.load.mockClear()
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={never}
      release={{ definition_version: DEFINITION_VERSION + 1, requires: ['component.Text'] }}
    />,
  )
  await screen.findByText('Update required')
  expect(screen.getByText(`Needs: definition_version.${DEFINITION_VERSION + 1}`)).toBeTruthy()
  expect(never.load).not.toHaveBeenCalled()
})

test('a release the client can draw is drawn', async () => {
  render(
    <AppView
      surfaceId="app-1"
      label="Expenses"
      transport={never}
      release={{ definition_version: DEFINITION_VERSION, requires: [...catalogCapabilities] }}
    />,
  )
  await screen.findByText('Coffee')
  expect(screen.queryByText('Update required')).toBeNull()
})

test('the app is drawn from the release it names, and its actions name that release', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=3': { body: surface },
    'POST /api/workspaces/1/spaces/2/apps/1/actions': { body: afterAdd },
  })
  const base = '/api/workspaces/1/spaces/2/apps/1'
  const transport = httpTransport(base, 3)
  await transport.load()
  await transport.act({
    name: 'add_expense',
    surfaceId: 'app-1',
    sourceComponentId: 'add',
    timestamp: 't',
    context: {},
  })
  expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([`GET ${base}/surface?release=3`, `POST ${base}/actions`])
  expect(calls[1].body).toMatchObject({ name: 'add_expense', release: 3 })
  // A draft is drawn, never acted on.
  const preview = httpTransport(base, 'draft')
  await expect(
    preview.act({ name: 'x', surfaceId: 'app-1', sourceComponentId: 'x', timestamp: 't', context: {} }),
  ).rejects.toThrow('draft preview')
  expect(calls).toHaveLength(2)
})

for (const [name, component] of [
  ['outside the catalog', { id: 'root', component: 'Script', src: 'x' }],
  ['left out of the basic catalog', { id: 'root', component: 'Image', url: 'https://elsewhere.example/p.png' }],
] as const) {
  test(`refuses components ${name}`, () => {
    const processor = strict()
    const errors = processor.processMessagesWithErrors([
      { version: 'v0.9', createSurface: { surfaceId: 's', catalogId: appsCatalog.id } },
      { version: 'v0.9', updateComponents: { surfaceId: 's', components: [component] } },
    ])
    expect(errors.length).toBeGreaterThan(0)
  })
}

test('refuses surfaces that name another catalog', () => {
  const processor = strict()
  const errors = processor.processMessagesWithErrors([
    { version: 'v0.9', createSurface: { surfaceId: 's', catalogId: 'https://evil.example/catalog.json' } },
  ])
  expect(errors.length).toBeGreaterThan(0)
  expect(processor.model.surfacesMap.size).toBe(0)
})
