// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import type { Me } from '../api'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { Shell } from './Shell'

const me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [
    { id: 1, name: 'Home', role: 'admin' },
    { id: 4, name: 'Team', role: 'member' },
  ],
}

// What the shell's default page (conversations) needs, for either workspace.
const pages = {
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  'GET /api/workspaces/4/conversations': { body: { conversations: [] } },
  // Settings → About, for an admin.
  'GET /api/workspaces/1/backup': { body: { health: 'none' } },
}

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
  delete document.documentElement.dataset.theme
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('first run: the setup screen sends the token from the log and signs in', async () => {
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: true } },
    'POST /api/setup': (call) =>
      (call.body as { setup_token: string }).setup_token === 'tok-123'
        ? { status: 201, body: me }
        : { status: 403, body: { error: 'wrong setup token' } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Set up this server' })

  const fill = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } })
  fill('Setup token', 'wrong')
  fill('Your name', 'Owner')
  fill('Email', 'owner@example.com')
  fill('Password', 'correct horse battery')
  fireEvent.click(screen.getByRole('button', { name: 'Create the owner' }))
  expect((await screen.findByRole('alert')).textContent).toBe('wrong setup token')

  fill('Setup token', '  tok-123\n')
  fireEvent.click(screen.getByRole('button', { name: 'Create the owner' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(calls.filter((c) => c.path === '/api/setup' && c.method === 'POST').at(-1)?.body).toEqual({
    setup_token: 'tok-123',
    name: 'Owner',
    email: 'owner@example.com',
    password: 'correct horse battery',
    workspace_name: 'Home',
  })
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/conversations'))
})

test('logs in, keeps the URL it was opened at, and logs out', async () => {
  window.history.replaceState(null, '', '/w/4/boards')
  let session = false
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': () => (session ? { body: me } : { status: 401, body: { error: 'not logged in' } }),
    'POST /api/login': (call) => {
      if ((call.body as { password: string }).password !== 'right password!') {
        return { status: 401, body: { error: 'wrong email or password' } }
      }
      session = true
      return { body: me }
    },
    'POST /api/logout': { status: 204 },
    'GET /api/workspaces/4/spaces': { body: { spaces: [] } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Log in' })
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'nope' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect((await screen.findByRole('alert')).textContent).toBe('wrong email or password')
  // A failed login is not a lost session.
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()

  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'right password!' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await screen.findByRole('heading', { name: 'Boards' })
  expect(window.location.pathname).toBe('/w/4/boards')
  expect((screen.getByLabelText('Workspace') as HTMLSelectElement).value).toBe('4')

  fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
  await screen.findByRole('heading', { name: 'Log in' })
  expect(calls.some((c) => c.method === 'POST' && c.path === '/api/logout')).toBe(true)
})

test('each person picks their language in the account menu; it is saved and followed on the next load', async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'PATCH /api/me': (call) => ({ body: { ...me, user: { ...me.user, ...(call.body as object) } } }),
    ...pages,
  })
  render(<App />)
  const language = await screen.findByRole('combobox', { name: 'Language' })
  expect((language as HTMLSelectElement).value).toBe('')
  expect(
    within(language)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['Automatic: English', 'English', 'Português (Brasil)'])
  fireEvent.change(language, { target: { value: 'pt-BR' } })
  const nav = await screen.findByRole('navigation', { name: 'Seções' })
  expect(
    within(nav)
      .getAllByRole('link')
      .map((a) => a.textContent),
  ).toEqual(['Conversas', 'Base de conhecimento', 'Quadros', 'Apps', 'Configurações'])
  expect(document.documentElement.lang).toBe('pt-BR')
  await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH' && c.path === '/api/me')).toHaveLength(1))
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ locale: 'pt-BR' })

  // Back to automatic: the browser's English.
  fireEvent.change(screen.getByRole('combobox', { name: 'Idioma' }), { target: { value: '' } })
  await screen.findByRole('navigation', { name: 'Sections' })
  cleanup()

  // Another browser: the account's pick, from /api/me.
  localStorage.clear()
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, user: { ...me.user, locale: 'pt-BR' } } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Seções' })
  await act(() => setPreferences({ user: null }))
})

test('a language picked for the agent that the app has no file for shows as picked; the app stays in English', async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, user: { ...me.user, locale: 'de' } } },
    ...pages,
  })
  render(<App />)
  const language = (await screen.findByRole('combobox', { name: 'Language' })) as HTMLSelectElement
  expect(language.value).toBe('de')
  expect(within(language).getByRole('option', { name: 'Deutsch' })).toBeTruthy()
  expect(screen.getByRole('navigation', { name: 'Sections' })).toBeTruthy()
  await act(() => setPreferences({ user: null }))
})

test('a language the server refuses to save is undone, so the browser and the account agree', async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'PATCH /api/me': { status: 500, body: { error: 'internal error', code: 'internal' } },
    ...pages,
  })
  render(<App />)
  const language = await screen.findByRole('combobox', { name: 'Language' })
  fireEvent.change(language, { target: { value: 'pt-BR' } })
  expect((await screen.findByRole('alert')).textContent).toMatch(/^Not saved: /)
  expect((screen.getByRole('combobox', { name: 'Language' }) as HTMLSelectElement).value).toBe('')
  expect(screen.getByRole('navigation', { name: 'Sections' })).toBeTruthy()
  expect(localStorage.getItem('taskto.locale')).toBeNull()
})

test('quick language picks are saved one at a time, and the account ends with the last one', async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  const held: (() => void)[] = []
  let inFlight = 0
  let most = 0
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'PATCH /api/me': () => {
      most = Math.max(most, ++inFlight)
      return new Promise((done) =>
        held.push(() => {
          inFlight--
          done({ status: 200, body: {} })
        }),
      )
    },
    ...pages,
  })
  render(<App />)
  const language = (await screen.findByRole('combobox', { name: 'Language' })) as HTMLSelectElement
  const patches = () => calls.filter((c) => c.method === 'PATCH').map((c) => c.body)

  fireEvent.change(language, { target: { value: 'pt-BR' } })
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }]))
  fireEvent.change(language, { target: { value: 'en' } })
  fireEvent.change(language, { target: { value: '' } })
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(patches()).toHaveLength(1) // the first is still in flight

  // The first answers; only the latest pick is sent next, the one in between never is.
  await act(async () => held.shift()?.())
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }, { locale: '' }]))
  await act(async () => held.shift()?.())
  expect(most).toBe(1)
  expect(language.value).toBe('')
  expect(screen.queryByRole('alert')).toBeNull()
})

test("the account's value arriving again while a pick is saved doesn't undo the pick", async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  const held: (() => void)[] = []
  const calls = fakeFetch({
    'PATCH /api/me': () => new Promise((done) => held.push(() => done({ status: 200, body: {} }))),
    ...pages,
  })
  const account = (locale: string): Me => ({ ...me, user: { ...me.user, locale } }) as Me
  const { rerender } = render(<Shell me={account('en')} onLogout={() => {}} />)
  const language = (await screen.findByRole('combobox', { name: 'Language' })) as HTMLSelectElement
  const patches = () => calls.filter((c) => c.method === 'PATCH').map((c) => c.body)

  fireEvent.change(language, { target: { value: 'pt-BR' } })
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }]))
  // /api/me again, from before the pick: the pick stays shown, and isn't sent over.
  rerender(<Shell me={account('')} onLogout={() => {}} />)
  expect(language.value).toBe('pt-BR')
  await act(async () => held.shift()?.())
  await screen.findByRole('navigation', { name: 'Seções' })
  expect(patches()).toEqual([{ locale: 'pt-BR' }])
  expect(language.value).toBe('pt-BR')
  await act(() => setPreferences({ user: null }))
})

test('the sidebar and the workspace switcher change the page', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    ...pages,
    'GET /api/workspaces/1/spaces': {
      body: { spaces: [{ id: 2, workspace_id: 1, kind: 'private', name: 'Private', access: 'write' }] },
    },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [{ path: 'notes/plan.md', version: 'v', size: 7 }] } },
    'GET /api/workspaces/1/spaces/2/docs/notes/plan.md': {
      body: { path: 'notes/plan.md', version: 'v', size: 7, content: '# Plan\n' },
    },
  })
  render(<App />)
  const nav = await screen.findByRole('navigation', { name: 'Sections' })
  expect(within(nav).getByRole('link', { name: 'Conversations' }).getAttribute('aria-current')).toBe('page')

  fireEvent.click(within(nav).getByRole('link', { name: 'Knowledge base' }))
  expect(window.location.pathname).toBe('/w/1/knowledge')
  fireEvent.click(await screen.findByRole('button', { name: /notes\/plan\.md/ }))
  expect((await screen.findByRole('heading', { name: 'Plan' })).tagName).toBe('H1')

  fireEvent.change(screen.getByLabelText('Workspace'), { target: { value: '4' } })
  expect(window.location.pathname).toBe('/w/4/knowledge')
  expect(localStorage.getItem('taskto.workspace')).toBe('4')

  // Back goes to the previous workspace's page.
  window.history.back()
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/knowledge'))
  await waitFor(() => expect((screen.getByLabelText('Workspace') as HTMLSelectElement).value).toBe('1'))
})

test('a URL for a workspace the user is not in goes to the last one used', async () => {
  localStorage.setItem('taskto.workspace', '4')
  window.history.replaceState(null, '', '/w/99/apps')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    ...pages,
    'GET /api/workspaces/4/spaces': { body: { spaces: [] } },
  })
  render(<App />)
  await waitFor(() => expect(window.location.pathname).toBe('/w/4/apps'))
  expect(await screen.findByText('You have no spaces in this workspace.')).toBeTruthy()
})

test('a session that ends goes back to the login', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'GET /api/workspaces/1/conversations': { status: 401, body: { error: 'not logged in' } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Log in' })
})

test('the theme can be light, dark or the system one, and is remembered', async () => {
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  const theme = (await screen.findByLabelText('Theme')) as HTMLSelectElement
  expect(theme.value).toBe('system')
  expect(document.documentElement.dataset.theme).toBeUndefined()

  fireEvent.change(theme, { target: { value: 'dark' } })
  expect(document.documentElement.dataset.theme).toBe('dark')
  expect(localStorage.getItem('taskto.theme')).toBe('dark')

  fireEvent.change(theme, { target: { value: 'system' } })
  expect(document.documentElement.dataset.theme).toBeUndefined()
  expect(localStorage.getItem('taskto.theme')).toBeNull()
})

test('the menu button opens the sidebar as a drawer, and a link closes it', async () => {
  // Opened at a page's own URL: from /, the shell first goes to the default page, and that
  // change of page closes the drawer, so a click before it lands would be undone.
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  const menu = await screen.findByRole('button', { name: 'Open the menu' })
  const shell = container.querySelector('.tt-shell') as HTMLElement
  expect(shell.dataset.drawer).toBe('closed')
  fireEvent.click(menu)
  expect(shell.dataset.drawer).toBe('open')
  expect(menu.getAttribute('aria-expanded')).toBe('true')
  fireEvent.click(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Apps' }))
  expect(shell.dataset.drawer).toBe('closed')
})

test('the drawer takes focus, makes the page inert, and Escape closes it back to its button', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  // The page has loaded, and with it the language: nothing is left to redraw the shell.
  await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull())
  await screen.findByText('Pick a conversation, or start a new one.')
  const menu = screen.getByRole('button', { name: 'Open the menu' })
  const main = container.querySelector('main') as HTMLElement
  expect(main.hasAttribute('inert')).toBe(false)

  fireEvent.click(menu)
  const nav = screen.getByRole('navigation', { name: 'Sections' })
  expect(document.activeElement).toBe(within(nav).getByRole('link', { name: 'Conversations' }))
  expect(main.hasAttribute('inert')).toBe(true)

  fireEvent.keyDown(document, { key: 'Escape' })
  expect((container.querySelector('.tt-shell') as HTMLElement).dataset.drawer).toBe('closed')
  expect(document.activeElement).toBe(menu)
  expect(main.hasAttribute('inert')).toBe(false)
})

test('picking a page in the drawer moves focus to the page', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull())
  await screen.findByText('Pick a conversation, or start a new one.')
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  // jsdom doesn't make inert elements unfocusable, as browsers do: check the page isn't inert
  // when it is focused.
  const real = HTMLElement.prototype.focus
  const inertWhenFocused: boolean[] = []
  const focus = vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (this: HTMLElement, options) {
    if (this.tagName === 'MAIN') inertWhenFocused.push(this.hasAttribute('inert'))
    real.call(this, options)
  })
  fireEvent.click(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Boards' }))
  await waitFor(() => expect(document.activeElement).toBe(container.querySelector('main')))
  expect(inertWhenFocused).toEqual([false])
  focus.mockRestore()
})

test('the skip link is the first stop and moves focus to the page', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  const skip = await screen.findByRole('button', { name: 'Skip to the content' })
  expect(container.querySelector('.tt-shell')?.firstElementChild).toBe(skip)
  fireEvent.click(skip)
  expect(document.activeElement).toBe(container.querySelector('main'))
  expect(window.location.pathname).toBe('/w/1/conversations')
})

test('the account menu leads to the install guidance', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  fireEvent.click(await screen.findByRole('link', { name: 'Install the app' }))
  await screen.findByRole('heading', { name: 'Install the app' })
  expect(window.location.pathname).toBe('/w/1/settings/about')
})
