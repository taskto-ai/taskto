// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Checklist, Me } from '../api'
import { Shell } from '../shell/Shell'
import { fakeFetch } from '../test/fakes'

const admin: Me = {
  user: { id: 1, email: 'ann@example.com', name: 'Ann' },
  workspaces: [{ id: 1, name: 'Acme', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const member: Me = { ...admin, workspaces: [{ id: 1, name: 'Acme', role: 'member' }] }

const emptyHome = { widgets: [], pins: [], hidden: [], limits: { widgets: 24, pins: 20 } }

const list = (over: Partial<Checklist> = {}): Checklist => ({
  dismissed: false,
  complete: false,
  items: [
    { id: 'invite', state: 'todo', optional: false },
    { id: 'provider', state: 'done', optional: false },
    { id: 'space', state: 'todo', optional: false },
    { id: 'app', state: 'todo', optional: false },
    { id: 'email', state: 'todo', optional: true },
    { id: 'backups', state: 'todo', optional: true },
  ],
  ...over,
})

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/home')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('an admin sees each step with its state and a link to the page where it is done', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': { body: list() },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  expect(within(panel).getByRole('status').textContent).toBe('1 of 4 steps handled')

  expect(within(panel).getByRole('list').tagName).toBe('OL')
  const steps = within(panel).getAllByRole('listitem')
  const said = (step: HTMLElement, name: string) => {
    expect(within(step).getByText(name)).toBeTruthy()
    return within(step)
      .getAllByText(/^(To do|Done|Skipped|Optional)$/)
      .map((b) => b.textContent)
  }
  expect([
    said(steps[0], 'Invite your team'),
    said(steps[1], 'Add a model provider'),
    said(steps[2], 'Create a shared space'),
    said(steps[3], 'Install an app template'),
    said(steps[4], 'Set up email'),
    said(steps[5], 'Set up backups'),
  ]).toEqual([['To do'], ['Done'], ['To do'], ['To do'], ['To do', 'Optional'], ['To do', 'Optional']])
  const link = (name: string) => within(panel).getByRole('link', { name }).getAttribute('href')
  expect(link('Invite people')).toBe('/w/1/settings/members')
  expect(link('Create a space')).toBe('/w/1/settings/spaces')
  expect(link('Browse templates')).toBe('/w/1/apps/templates')
  expect(link('See the backups')).toBe('/w/1/settings/about')
  expect(within(steps[1]).queryByRole('link')).toBeNull()
  expect(within(steps[1]).queryByRole('button')).toBeNull()
  expect(within(panel).getByRole('link', { name: 'Invite people' }).className).toContain('ui-button-primary')
  expect(within(panel).getByRole('link', { name: 'Create a space' }).className).not.toContain('ui-button-primary')
})

test('a member never sees the checklist or asks for it', async () => {
  const calls = fakeFetch({ 'GET /api/workspaces/1/home': { body: emptyHome } })
  render(<Shell me={member} onLogout={() => {}} />)
  await screen.findByText(/Nothing on your Home yet/)
  expect(screen.queryByRole('region', { name: 'Set up your workspace' })).toBeNull()
  expect(calls.some((c) => c.path.includes('/checklist'))).toBe(false)
})

test('a checklist that is complete, or can’t be loaded, leaves Home as it is', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': { body: list({ complete: true }) },
  })
  const { unmount } = render(<Shell me={admin} onLogout={() => {}} />)
  await screen.findByText(/Nothing on your Home yet/)
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Set up your workspace' })).toBeNull())
  unmount()

  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': { status: 500, body: { code: 'internal', error: 'internal error' } },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  await screen.findByText(/Nothing on your Home yet/)
  expect(screen.queryByRole('region', { name: 'Set up your workspace' })).toBeNull()
  expect(screen.queryByRole('alert')).toBeNull()
})

test('a step that doesn’t apply is skipped and taken back, keeping the keyboard where it was', async () => {
  let skipped = false
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': () => ({
      body: list({ items: list().items.map((i) => (i.id === 'invite' && skipped ? { ...i, state: 'skipped' } : i)) }),
    }),
    'PUT /api/workspaces/1/checklist/skipped/invite': () => {
      skipped = true
      return { status: 204 }
    },
    'DELETE /api/workspaces/1/checklist/skipped/invite': () => {
      skipped = false
      return { status: 204 }
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  const skip = within(panel).getByRole('button', { name: 'Skip: Invite your team' })
  skip.focus()
  fireEvent.click(skip)
  const undo = await within(panel).findByRole('button', { name: 'Undo skip: Invite your team' })
  expect(within(panel).getByRole('status').textContent).toBe('2 of 4 steps handled')
  expect(within(panel).getByText('Skipped')).toBeTruthy()
  expect(document.activeElement).toBe(undo)

  fireEvent.click(undo)
  await within(panel).findByRole('button', { name: 'Skip: Invite your team' })
  expect(within(panel).getByRole('status').textContent).toBe('1 of 4 steps handled')
  expect(calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`)).toEqual([
    'PUT /api/workspaces/1/checklist/skipped/invite',
    'DELETE /api/workspaces/1/checklist/skipped/invite',
  ])
})

test('dismissing puts it away, moves the keyboard to the page, and Customize brings it back', async () => {
  let dismissed = false
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'GET /api/workspaces/1/checklist': () => ({ body: list({ dismissed }) }),
    'PUT /api/workspaces/1/checklist/dismissed': () => {
      dismissed = true
      return { status: 204 }
    },
    'DELETE /api/workspaces/1/checklist/dismissed': () => {
      dismissed = false
      return { status: 204 }
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  await screen.findByRole('region', { name: 'Set up your workspace' })
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss checklist' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Set up your workspace' })).toBeNull())
  expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Home', level: 1 }))

  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Show the setup checklist' }))
  await waitFor(() => expect(document.activeElement?.id).toBe('ui-checklist-title'))
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Show the setup checklist' })).toBeNull())
  expect(calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`)).toEqual([
    'PUT /api/workspaces/1/checklist/dismissed',
    'DELETE /api/workspaces/1/checklist/dismissed',
  ])
})

test('a refused change is said, in words the person can read', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': { body: list() },
    'PUT /api/workspaces/1/checklist/skipped/space': {
      status: 403,
      body: { code: 'admin_only', error: 'only a workspace admin can do this' },
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  fireEvent.click(within(panel).getByRole('button', { name: 'Skip: Create a shared space' }))
  const alert = await within(panel).findByRole('alert')
  expect(alert.textContent).not.toBe('')
  expect(alert.textContent).not.toContain('admin_only')
})

test('while the list loads again after a change, the button being used stays, and Home isn’t asked for again', async () => {
  let skipped = false
  let release: (() => void) | undefined
  let hold = false
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': async () => {
      if (hold) await new Promise<void>((resolve) => (release = resolve))
      return {
        body: list({ items: list().items.map((i) => (i.id === 'invite' && skipped ? { ...i, state: 'skipped' } : i)) }),
      }
    },
    'PUT /api/workspaces/1/checklist/skipped/invite': () => {
      skipped = true
      return { status: 204 }
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  const skip = within(panel).getByRole('button', { name: 'Skip: Invite your team' })
  skip.focus()
  const homeLoads = calls.filter((c) => c.path === '/api/workspaces/1/home').length
  hold = true
  fireEvent.click(skip)
  await waitFor(() => expect(release).toBeDefined())
  expect(screen.getByRole('region', { name: 'Set up your workspace' })).toBeTruthy()
  expect(document.activeElement).toBe(skip)
  release?.()
  await within(panel).findByRole('button', { name: 'Undo skip: Invite your team' })
  expect(calls.filter((c) => c.path === '/api/workspaces/1/home')).toHaveLength(homeLoads)
})

test('a failed bringing back keeps its message, even while the list loads again', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'GET /api/workspaces/1/checklist': { body: list({ dismissed: true }) },
    'DELETE /api/workspaces/1/checklist/dismissed': {
      status: 403,
      body: { code: 'admin_only', error: 'only a workspace admin can do this' },
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  await screen.findByText(/Nothing on your Home yet/)
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Show the setup checklist' }))
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).not.toBe('')
  await waitFor(() => expect(calls.filter((c) => c.path === '/api/workspaces/1/checklist')).toHaveLength(4))
  expect(screen.getByRole('button', { name: 'Show the setup checklist' })).toBeTruthy()
  expect(screen.getByRole('alert')).toBe(alert)
})

test('skipping the last step moves the keyboard to the page and says the list is complete', async () => {
  let skipped = false
  const items = (): Checklist['items'] =>
    list().items.map((i) => ({
      ...i,
      state: i.id === 'space' && skipped ? 'skipped' : i.optional || i.id === 'space' ? i.state : 'done',
    }))
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': () => ({ body: list({ complete: skipped, items: items() }) }),
    'PUT /api/workspaces/1/checklist/skipped/space': () => {
      skipped = true
      return { status: 204 }
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  const skip = within(panel).getByRole('button', { name: 'Skip: Create a shared space' })
  skip.focus()
  fireEvent.click(skip)
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Set up your workspace' })).toBeNull())
  const done = await screen.findByText('The setup checklist is complete.')
  expect(done.closest('[role="status"]')).toBeTruthy()
  expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Home', level: 1 }))
})

test('a dismissal that fails leaves the checklist, and the keyboard, where they were', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': { body: list() },
    'PUT /api/workspaces/1/checklist/dismissed': {
      status: 403,
      body: { code: 'admin_only', error: 'only a workspace admin can do this' },
    },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  const dismiss = within(panel).getByRole('button', { name: 'Dismiss checklist' })
  dismiss.focus()
  fireEvent.click(dismiss)
  await within(panel).findByRole('alert')
  expect(document.activeElement).toBe(dismiss)
})

test('a list that can’t be loaded again says so under what is shown', async () => {
  let loads = 0
  fakeFetch({
    'GET /api/workspaces/1/home': { body: emptyHome },
    'GET /api/workspaces/1/checklist': () =>
      ++loads > 1 ? { status: 500, body: { code: 'internal', error: 'x' } } : { body: list() },
    'PUT /api/workspaces/1/checklist/skipped/invite': { status: 204 },
  })
  render(<Shell me={admin} onLogout={() => {}} />)
  const panel = await screen.findByRole('region', { name: 'Set up your workspace' })
  fireEvent.click(within(panel).getByRole('button', { name: 'Skip: Invite your team' }))
  const alert = await within(panel).findByRole('alert')
  expect(alert.textContent).not.toBe('')
  expect(within(panel).getAllByRole('listitem')).toHaveLength(6)
})
