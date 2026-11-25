// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import type { Me } from '../api'
import { setPreferences } from '../i18n'
import { type BeforeLeave, navigate, useBeforeLeave } from '../router'
import { fakeFetch } from '../test/fakes'
import { resetToasts } from '../ui/Toast'
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
  'GET /api/workspaces/1/updates': { body: { configured: false, running_version: 'dev', status: null } },
}

// What Shell.css does on a phone and a wide screen, which jsdom has no stylesheet for: the
// tooltips of the sidebar's links and of the account are hidden (only the rail shows them), so an
// open one can't be seen. A test that wants one seen says so.
let sidebarTooltipsSeen = false
const hiddenByShellCss = (el: Element) =>
  !sidebarTooltipsSeen && el.closest('.ui-shell-nav, .ui-shell-account') !== null

beforeEach(() => {
  resetToasts()
  window.history.replaceState(null, '', '/')
  localStorage.clear()
  delete document.documentElement.dataset.theme
  sidebarTooltipsSeen = false
  // jsdom has no checkVisibility: this one, for these tests only, is the stylesheet's say.
  HTMLElement.prototype.checkVisibility = function (this: HTMLElement) {
    return !hiddenByShellCss(this)
  }
})
afterEach(() => {
  delete (HTMLElement.prototype as Partial<HTMLElement>).checkVisibility
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const shellOf = (container: HTMLElement) => container.querySelector('.ui-shell') as HTMLElement

// The account menu, opened from its button at the sidebar's foot (named in either language).
async function openAccount(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole('button', { name: /^(Account|Conta): Owner owner@example\.com$/ }))
  return screen.getByRole('menu')
}

// A choice of the account menu (a theme, a language), picked as a person does.
async function pickInAccount(name: string | RegExp) {
  const menu = await openAccount()
  fireEvent.click(within(menu).getByRole('menuitemradio', { name }))
}

// The workspace switcher's menu.
async function openSwitcher(current = 'Home'): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole('button', { name: `Workspace: ${current}` }))
  return screen.getByRole('menu')
}

const checked = (menu: HTMLElement) =>
  within(menu)
    .getAllByRole('menuitemradio')
    .filter((r) => r.getAttribute('aria-checked') === 'true')
    .map((r) => r.textContent)

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
  expect(screen.getByRole('button', { name: 'Workspace: Team' })).toBeTruthy()

  fireEvent.click(within(await openAccount()).getByRole('menuitem', { name: 'Log out' }))
  await screen.findByRole('heading', { name: 'Log in' })
  expect(calls.some((c) => c.method === 'POST' && c.path === '/api/logout')).toBe(true)
})

test("signing in sends the browser's time zone once, to an account that has none", async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({
    timeZone: 'America/Sao_Paulo',
  } as Intl.ResolvedDateTimeFormatOptions)
  let zone = ''
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': () => ({ body: { ...me, user: { ...me.user, time_zone: zone } } }),
    'PATCH /api/me': (call) => {
      zone = (call.body as { browser_time_zone: string }).browser_time_zone
      return { body: { ...me, user: { ...me.user, time_zone: zone } } }
    },
    ...pages,
  })
  const first = render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  const patches = () => calls.filter((c) => c.method === 'PATCH' && c.path === '/api/me').map((c) => c.body)
  expect(patches()).toEqual([{ browser_time_zone: 'America/Sao_Paulo' }])
  // The next load finds the account has one, and sends nothing.
  first.unmount()
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(patches()).toHaveLength(1)
  vi.restoreAllMocks()
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
  const menu = await openAccount()
  const language = within(menu).getByRole('group', { name: 'Language' })
  expect(
    within(language)
      .getAllByRole('menuitemradio')
      .map((o) => o.textContent),
  ).toEqual(['Automatic: English', 'English', 'Português (Brasil)'])
  expect(checked(language)).toEqual(['Automatic: English'])
  fireEvent.click(within(language).getByRole('menuitemradio', { name: 'Português (Brasil)' }))
  const nav = await screen.findByRole('navigation', { name: 'Seções' })
  expect(
    within(nav)
      .getAllByRole('link')
      .map((a) => a.textContent),
  ).toEqual(['Início', 'Conversas', 'Base de conhecimento', 'Quadros', 'Apps', 'Notificações', 'Configurações'])
  expect(document.documentElement.lang).toBe('pt-BR')
  await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH' && c.path === '/api/me')).toHaveLength(1))
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ locale: 'pt-BR' })

  // Back to automatic: the browser's English.
  await pickInAccount(/^Automático/)
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
  const menu = await openAccount()
  expect(checked(within(menu).getByRole('group', { name: 'Language' }))).toEqual(['Deutsch'])
  expect(screen.getByRole('navigation', { name: 'Sections' })).toBeTruthy()
  await act(() => setPreferences({ user: null }))
})

test('a refusal that goes back to a language loaded on demand is said once, all in that language', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, user: { ...me.user, locale: 'pt-BR' } } },
    'PATCH /api/me': { status: 500, body: { error: 'internal error', code: 'internal' } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Seções' })
  await pickInAccount('English')
  // Back to Portuguese, whose file comes when it is asked for: the line and the server's reason
  // are both in it, said once.
  const said = await screen.findByText(/^Não foi salvo: /)
  expect(said.textContent).toBe(
    'Não foi salvo: Algo deu errado no servidor. Tente de novo; se continuar acontecendo, o log do servidor diz por quê.',
  )
  const toasts = () => screen.queryAllByText(/^(Não foi salvo|Not saved): /)
  expect(toasts()).toHaveLength(1)
  await screen.findByRole('navigation', { name: 'Seções' })

  // The app's language changing again later (another workspace's default, the person's own pick
  // elsewhere) says nothing more.
  await act(() => setPreferences({ user: null }))
  await screen.findByRole('navigation', { name: 'Sections' })
  await act(() => setPreferences({ user: 'pt-BR' }))
  await screen.findByRole('navigation', { name: 'Seções' })
  expect(toasts()).toHaveLength(1)
  await act(() => setPreferences({ user: null }))
})

test('a language the server refuses to save is undone and said, so the browser and the account agree', async () => {
  window.history.replaceState(null, '', '/w/1/settings/about')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'PATCH /api/me': { status: 500, body: { error: 'internal error', code: 'internal' } },
    ...pages,
  })
  render(<App />)
  await pickInAccount('Português (Brasil)')
  expect(await screen.findByText(/^Not saved: /)).toBeTruthy()
  expect(screen.getByRole('navigation', { name: 'Sections' })).toBeTruthy()
  expect(checked(within(await openAccount()).getByRole('group', { name: 'Language' }))).toEqual(['Automatic: English'])
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
  const patches = () => calls.filter((c) => c.method === 'PATCH').map((c) => c.body)

  await pickInAccount('Português (Brasil)')
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }]))
  await screen.findByRole('navigation', { name: 'Seções' })
  await pickInAccount('English')
  await screen.findByRole('navigation', { name: 'Sections' })
  await pickInAccount(/^Automatic/)
  expect(patches()).toHaveLength(1) // the first is still in flight

  // The first answers; only the latest pick is sent next, the one in between never is.
  await act(async () => held.shift()?.())
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }, { locale: '' }]))
  await act(async () => held.shift()?.())
  expect(most).toBe(1)
  expect(checked(within(await openAccount()).getByRole('group', { name: 'Language' }))).toEqual(['Automatic: English'])
  expect(screen.queryByText(/^Not saved: /)).toBeNull()
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
  const patches = () => calls.filter((c) => c.method === 'PATCH').map((c) => c.body)

  await pickInAccount('Português (Brasil)')
  await waitFor(() => expect(patches()).toEqual([{ locale: 'pt-BR' }]))
  // /api/me again, from before the pick: the pick stays shown, and isn't sent over.
  rerender(<Shell me={account('')} onLogout={() => {}} />)
  await act(async () => held.shift()?.())
  await screen.findByRole('navigation', { name: 'Seções' })
  expect(patches()).toEqual([{ locale: 'pt-BR' }])
  expect(checked(within(await openAccount()).getByRole('group', { name: 'Idioma' }))).toEqual(['Português (Brasil)'])
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
  // The documents are a tree: the folder, then the document in it.
  fireEvent.click(await screen.findByRole('button', { name: /^notes/ }))
  fireEvent.click(screen.getByRole('button', { name: /^plan\.md/ }))
  expect((await screen.findByRole('heading', { name: 'Plan' })).tagName).toBe('H1')

  // The switcher lists the person's workspaces, the current one checked.
  const menu = await openSwitcher('Home')
  expect(checked(menu)).toEqual(['Home'])
  fireEvent.click(within(menu).getByRole('menuitemradio', { name: 'Team' }))
  expect(window.location.pathname).toBe('/w/4/knowledge')
  expect(localStorage.getItem('taskto.workspace')).toBe('4')
  await screen.findByRole('button', { name: 'Workspace: Team' })

  // Back goes to the previous workspace's page.
  window.history.back()
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/knowledge'))
  await screen.findByRole('button', { name: 'Workspace: Home' })
})

// A page with a draft, which asks before it is left.
function Draft({ ask }: { ask: BeforeLeave }) {
  useBeforeLeave(ask)
  return null
}

test('a page that asks before it is left is asked when another workspace is picked', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  // A page with a draft: it holds the move until the person answers.
  let leave: (() => void) | null = null
  const ask = vi.fn<BeforeLeave>((go) => {
    leave = go
  })
  render(
    <>
      <Shell me={me as unknown as Me} onLogout={() => {}} />
      <Draft ask={ask} />
    </>,
  )
  fireEvent.click(within(await openSwitcher('Home')).getByRole('menuitemradio', { name: 'Team' }))
  expect(ask).toHaveBeenCalledTimes(1)
  expect(window.location.pathname).toBe('/w/1/conversations')
  act(() => leave?.())
  expect(window.location.pathname).toBe('/w/4/conversations')
  await screen.findByRole('button', { name: 'Workspace: Team' })
})

test('the workspace switcher works by keyboard and gives the focus back', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  const button = await screen.findByRole('button', { name: 'Workspace: Home' })
  expect(button.getAttribute('aria-haspopup')).toBe('menu')
  fireEvent.keyDown(button, { key: 'ArrowDown' })
  const menu = screen.getByRole('menu')
  expect(document.activeElement).toBe(within(menu).getByRole('menuitemradio', { name: 'Home' }))
  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  expect(screen.queryByRole('menuitemradio', { name: 'Home' })).toBeNull()
  expect(document.activeElement).toBe(button)
})

test('the inbox is in the sidebar, with its unread count in its name', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'GET /api/workspaces/1/notifications/count': { body: { unread: 3 } },
    'GET /api/workspaces/1/notifications': { body: { notifications: [], unread: 3 } },
    ...pages,
  })
  render(<App />)
  const nav = await screen.findByRole('navigation', { name: 'Sections' })
  const inbox = await within(nav).findByRole('link', { name: 'Notifications, 3 unread' })
  expect(inbox.getAttribute('href')).toBe('/w/1/notifications')
  expect(inbox.textContent).toBe('Notifications3')
  fireEvent.click(inbox)
  await waitFor(() => expect(inbox.getAttribute('aria-current')).toBe('page'))
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

test('the theme can be light, dark or the system one, in the account menu, and is remembered', async () => {
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  expect(checked(within(await openAccount()).getByRole('group', { name: 'Theme' }))).toEqual(['System'])
  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  expect(document.documentElement.dataset.theme).toBeUndefined()

  await pickInAccount('Dark')
  expect(document.documentElement.dataset.theme).toBe('dark')
  expect(localStorage.getItem('taskto.theme')).toBe('dark')
  expect(checked(within(await openAccount()).getByRole('group', { name: 'Theme' }))).toEqual(['Dark'])
  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })

  await pickInAccount('System')
  expect(document.documentElement.dataset.theme).toBeUndefined()
  expect(localStorage.getItem('taskto.theme')).toBeNull()
})

test("the theme picked in the app also sets the browser's theme-color, and the system's puts it back", async () => {
  // Added to the head, not in place of it: the test setup's styles for popovers are there.
  const metas = document.createElement('template')
  metas.innerHTML = `
    <meta name="theme-color" content="#faf8f4" media="(prefers-color-scheme: light)" />
    <meta name="theme-color" content="#111110" media="(prefers-color-scheme: dark)" />`
  const added = [...metas.content.children]
  document.head.append(...added)
  const colors = () => [...document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')].map((m) => m.content)
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)

  await pickInAccount('Dark')
  expect(colors()).toEqual(['#111110', '#111110'])
  await pickInAccount('Light')
  expect(colors()).toEqual(['#faf8f4', '#faf8f4'])
  await pickInAccount('System')
  expect(colors()).toEqual(['#faf8f4', '#111110'])
  for (const m of added) m.remove()
})

test('a long workspace name is cut in the switcher and whole in its tooltip; the phone bar has the menu, the mark and the section', async () => {
  const long = 'Operations and Field Services'
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, workspaces: [{ id: 1, name: long, role: 'admin' }] } },
    ...pages,
  })
  render(<App />)
  const switcher = await screen.findByRole('button', { name: `Workspace: ${long}` })
  // The name the stylesheet cuts with an ellipsis, and the tooltip with all of it.
  expect(switcher.querySelector('.ui-shell-switcher-name')?.textContent).toBe(long)
  const tip = document.getElementById(switcher.parentElement?.querySelector('[role="tooltip"]')?.id ?? '')
  expect(tip?.textContent).toBe(long)

  // The phone's bar: what stays in it, each reachable by its label. The logo is one image with the
  // product's name, which the stylesheet crops to its mark.
  const bar = screen.getByRole('banner')
  expect(within(bar).getByRole('button', { name: 'Open the menu' })).toBeTruthy()
  expect(within(bar).getByRole('img', { name: 'TaskTo.AI' })).toBeTruthy()
  expect(within(bar).getByText('Conversations')).toBeTruthy()
  // The page owns its title: no heading in the bar.
  expect(within(bar).queryByRole('heading')).toBeNull()
})

test('the menu button opens the sidebar as a drawer, and a link closes it', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  const menu = await screen.findByRole('button', { name: 'Open the menu' })
  const shell = shellOf(container)
  expect(shell.dataset.drawer).toBe('closed')
  fireEvent.click(menu)
  expect(shell.dataset.drawer).toBe('open')
  expect(menu.getAttribute('aria-expanded')).toBe('true')
  expect(menu.getAttribute('aria-controls')).toBe(container.querySelector('.ui-shell-sidebar')?.id)
  fireEvent.click(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Apps' }))
  expect(shell.dataset.drawer).toBe('closed')
})

// In a browser a person can click before React has run the effects of the first draw (they run
// after it, in a task of their own); a draw inside act() runs them at once, so draw outside it.
async function clickMenuAtFirstDraw(): Promise<string> {
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
  const before = env.IS_REACT_ACT_ENVIRONMENT
  env.IS_REACT_ACT_ENVIRONMENT = false
  const host = document.body.appendChild(document.createElement('div'))
  const root = createRoot(host)
  try {
    root.render(<Shell me={me as unknown as Me} onLogout={() => {}} />)
    // The observer's callback is a microtask of the commit, ahead of the effects' task.
    await new Promise<void>((resolve) => {
      const seen = new MutationObserver(() => {
        if (!host.querySelector('.ui-shell-menu-button')) return
        seen.disconnect()
        resolve()
      })
      seen.observe(host, { childList: true, subtree: true })
    })
    host.querySelector<HTMLButtonElement>('.ui-shell-menu-button')?.click()
    await new Promise((resolve) => setTimeout(resolve, 50))
    return shellOf(host).dataset.drawer ?? ''
  } finally {
    root.unmount()
    host.remove()
    env.IS_REACT_ACT_ENVIRONMENT = before
  }
}

test("a click on the menu button as soon as the shell is drawn isn't undone by the shell's first effects", async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  expect(await clickMenuAtFirstDraw()).toBe('open')
})

test('nor is it when the shell rewrites the address to its canonical form: a trailing slash, no page, an unknown workspace', async () => {
  for (const [opened, canonical] of [
    ['/w/1/conversations/', '/w/1/conversations'],
    ['/', '/w/1/conversations'],
    ['/w/9/boards', '/w/1/boards'],
  ]) {
    window.history.replaceState(null, '', opened)
    expect(await clickMenuAtFirstDraw(), opened).toBe('open')
    expect(window.location.pathname).toBe(canonical)
  }
})

test('a change of page closes the drawer: a link, back and forward; a hash or a query does not', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    ...pages,
    'GET /api/workspaces/1/spaces': { body: { spaces: [] } },
  })
  const { container } = render(<App />)
  const menu = await screen.findByRole('button', { name: 'Open the menu' })
  const shell = shellOf(container)
  const go = (path: string) => act(() => navigate(path))
  const back = (delta: number) =>
    act(async () => {
      // jsdom traverses the history in a task of its own and then fires popstate: wait for that.
      const traversed = new Promise<void>((resolve) =>
        window.addEventListener('popstate', () => resolve(), { once: true }),
      )
      window.history.go(delta)
      await traversed
    })

  await go('/w/1/boards')
  fireEvent.click(menu)
  expect(shell.dataset.drawer).toBe('open')
  await act(async () => {
    window.history.replaceState(null, '', '/w/1/boards?x=1#top')
    window.dispatchEvent(new PopStateEvent('popstate'))
  })
  expect(shell.dataset.drawer).toBe('open')
  await back(-1)
  expect(window.location.pathname).toBe('/w/1/conversations')
  expect(shell.dataset.drawer).toBe('closed')

  fireEvent.click(menu)
  expect(shell.dataset.drawer).toBe('open')
  await back(1)
  expect(window.location.pathname).toBe('/w/1/boards')
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
  expect(shellOf(container).dataset.drawer).toBe('closed')
  expect(document.activeElement).toBe(menu)
  expect(main.hasAttribute('inert')).toBe(false)
})

test('with a tooltip showing in the drawer, the first Escape closes only the tooltip, the next one the drawer', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  // A tooltip that can be seen (a window between the phone's and the rail's widths, say).
  sidebarTooltipsSeen = true
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  const link = within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Conversations' })
  fireEvent.focus(link)
  const tip = link.parentElement?.querySelector('[role="tooltip"]') as HTMLElement
  await waitFor(() => expect(tip.hasAttribute('data-open')).toBe(true))

  fireEvent.keyDown(link, { key: 'Escape' })
  expect(tip.hasAttribute('data-open')).toBe(false)
  expect(shellOf(container).dataset.drawer).toBe('open')

  fireEvent.keyDown(link, { key: 'Escape' })
  expect(shellOf(container).dataset.drawer).toBe('closed')
})

test('with the drawer and the account menu open, Escape closes the menu first and the next one the drawer', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  const button = screen.getByRole('button', { name: 'Open the menu' })
  const shell = shellOf(container)
  fireEvent.click(button)
  expect(shell.dataset.drawer).toBe('open')
  const account = screen.getByRole('button', { name: /^Account: Owner/ })
  await openAccount()

  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  expect(screen.queryByRole('menu')).toBeNull()
  expect(document.activeElement).toBe(account)
  expect(shell.dataset.drawer).toBe('open')

  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  expect(shell.dataset.drawer).toBe('closed')
  expect(document.activeElement).toBe(button)
})

test("an Escape in the new workspace's dialog, over the drawer, is the dialog's: the drawer stays", async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    ...pages,
  })
  const { container } = render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  const field = await screen.findByLabelText(/^Name of the new workspace/)
  expect(document.activeElement).toBe(field)
  fireEvent.keyDown(field, { key: 'Escape' })
  expect(shellOf(container).dataset.drawer).toBe('open')
})

test('a link of the drawer that a page stops (to ask first) still closes the drawer, so its question can be answered', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  // A page with a draft takes the click on the document, in the capture phase, to ask first.
  const stop = (e: MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
  }
  document.addEventListener('click', stop, true)
  try {
    fireEvent.click(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Boards' }))
    expect(window.location.pathname).toBe('/w/1/conversations')
    expect(shellOf(container).dataset.drawer).toBe('closed')
    expect((container.querySelector('main') as HTMLElement).hasAttribute('inert')).toBe(false)
  } finally {
    document.removeEventListener('click', stop, true)
  }
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
  expect(shellOf(container).firstElementChild).toBe(skip)
  fireEvent.click(skip)
  expect(document.activeElement).toBe(container.querySelector('main'))
  expect(window.location.pathname).toBe('/w/1/conversations')
})

test('the account menu shows who is signed in and leads to the install guidance', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  const account = await screen.findByRole('button', { name: /^Account: Owner/ })
  expect(account.textContent).toContain('owner@example.com')
  fireEvent.click(within(await openAccount()).getByRole('menuitem', { name: 'Install the app' }))
  await screen.findByRole('heading', { name: 'Install the app' })
  expect(window.location.pathname).toBe('/w/1/settings/about')
})

test('the focus leaving the account menu by Tab closes it', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  const account = screen.getByRole('button', { name: /^Account: Owner/ })
  const menu = await openAccount()
  fireEvent.keyDown(within(menu).getAllByRole('menuitemradio')[0], { key: 'Tab' })
  expect(screen.queryByRole('menu')).toBeNull()
  expect(account.getAttribute('aria-expanded')).toBe('false')
})

test('Escape closes the account menu and gives focus back to its button; a press outside closes it too', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } }, 'GET /api/me': { body: me }, ...pages })
  const { container } = render(<App />)
  await screen.findByText('Pick a conversation, or start a new one.')
  const account = screen.getByRole('button', { name: /^Account: Owner/ })
  const menu = await openAccount()
  // The menu opens on its first item, which the keyboard moves from.
  expect(document.activeElement).toBe(within(menu).getAllByRole('menuitemradio')[0])
  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  expect(screen.queryByRole('menu')).toBeNull()
  expect(document.activeElement).toBe(account)

  await openAccount()
  fireEvent.pointerDown(container.querySelector('main') as HTMLElement)
  expect(screen.queryByRole('menu')).toBeNull()
})

test('"New workspace" is offered only when the server lets the person, and switches to the one made', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  const created = {
    workspace: { id: 9, name: 'Side project', role: 'admin' },
    user: me.user,
    workspaces: [...me.workspaces, { id: 9, name: 'Side project', role: 'admin' }],
    can_create_workspace: true,
  }
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': { status: 201, body: created },
    'GET /api/workspaces/9/conversations': { body: { conversations: [] } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })

  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  const dialog = await screen.findByRole('dialog', { name: 'New workspace' })
  fireEvent.change(within(dialog).getByLabelText(/^Name of the new workspace/), {
    target: { value: '  Side project ' },
  })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Create workspace' }))

  await waitFor(() => expect(window.location.pathname).toBe('/w/9/conversations'))
  expect(calls.filter((c) => c.path === '/api/workspaces' && c.method === 'POST').map((c) => c.body)).toEqual([
    { name: 'Side project' },
  ])
  // The request says which account the form was opened for, so a session that became another's is refused.
  expect(calls.find((c) => c.path === '/api/workspaces' && c.method === 'POST')?.headers).toMatchObject({
    'x-taskto-expected-user': String(me.user.id),
  })
  // The switcher shows it and lists it.
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(checked(await openSwitcher('Side project'))).toEqual(['Side project'])
  expect(localStorage.getItem('taskto.workspace')).toBe('9')
})

test('"New workspace" says why a server refused, and is not offered to those it did not allow', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': {
      status: 403,
      body: { error: 'no', code: 'workspace_creation_refused' },
    },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  fireEvent.change(screen.getByLabelText(/^Name of the new workspace/), { target: { value: 'X' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create workspace' }))
  expect((await screen.findByRole('alert')).textContent).toBe(
    "This server doesn't let you create a workspace. Ask the person who runs it.",
  )
  expect(window.location.pathname).toBe('/w/1/conversations')

  cleanup()
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: false } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(within(await openSwitcher()).queryByRole('menuitem', { name: 'New workspace' })).toBeNull()
})

test('a workspace made after Cancel is said, with a way to open it, and the page stays; a failure after Cancel is not kept', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  const held: ((answer: { status: number; body: unknown }) => void)[] = []
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': () => new Promise((done) => held.push(done)),
    'GET /api/workspaces/9/conversations': { body: { conversations: [] } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  const create = async (name: string) => {
    fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
    const dialog = await screen.findByRole('dialog', { name: 'New workspace' })
    fireEvent.change(within(dialog).getByLabelText(/^Name of the new workspace/), { target: { value: name } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create workspace' }))
    await waitFor(() => expect(held).toHaveLength(1))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
  }

  // A failure after Cancel made nothing: the next opening starts clean.
  await create('Doomed')
  await act(async () => held.shift()?.({ status: 500, body: { error: 'internal error', code: 'internal' } }))
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  const again = await screen.findByRole('dialog', { name: 'New workspace' })
  expect(within(again).queryByRole('alert')).toBeNull()
  expect((within(again).getByLabelText(/^Name of the new workspace/) as HTMLInputElement).value).toBe('')
  fireEvent.click(within(again).getByRole('button', { name: 'Cancel' }))

  // A success after Cancel: the workspace exists, the switcher lists it, a toast says so, and the
  // person stays where they are until they open it.
  await create('Late')
  await act(async () =>
    held.shift()?.({
      status: 201,
      body: {
        workspace: { id: 9, name: 'Late', role: 'admin' },
        user: me.user,
        workspaces: [...me.workspaces, { id: 9, name: 'Late', role: 'admin' }],
        can_create_workspace: true,
      },
    }),
  )
  expect(await screen.findByText('“Late” was created.')).toBeTruthy()
  expect(window.location.pathname).toBe('/w/1/conversations')
  const switcher = await openSwitcher()
  expect(within(switcher).getByRole('menuitemradio', { name: 'Late' })).toBeTruthy()
  fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' })
  fireEvent.click(screen.getByRole('button', { name: 'Open it' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/9/conversations'))
})

test('a save that ends after Cancel and a new opening leaves the new opening alone', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  const held: ((answer: { status: number; body: unknown }) => void)[] = []
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': () => new Promise((done) => held.push(done)),
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  const openDialog = async () => {
    fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
    return screen.findByRole('dialog', { name: 'New workspace' })
  }
  const first = await openDialog()
  fireEvent.change(within(first).getByLabelText(/^Name of the new workspace/), { target: { value: 'Late' } })
  fireEvent.click(within(first).getByRole('button', { name: 'Create workspace' }))
  await waitFor(() => expect(held).toHaveLength(1))
  fireEvent.click(within(first).getByRole('button', { name: 'Cancel' }))

  // Opened again before the first save answers: a fresh form, not busy.
  const second = await openDialog()
  expect((within(second).getByLabelText(/^Name of the new workspace/) as HTMLInputElement).value).toBe('')
  expect(within(second).getByRole('button', { name: 'Create workspace' }).getAttribute('aria-busy')).toBeNull()

  await act(async () =>
    held.shift()?.({
      status: 201,
      body: {
        workspace: { id: 9, name: 'Late', role: 'admin' },
        user: me.user,
        workspaces: [...me.workspaces, { id: 9, name: 'Late', role: 'admin' }],
        can_create_workspace: true,
      },
    }),
  )
  // The first save says what it made; the opening that is there now stays, and so does the page.
  expect(await screen.findByText('“Late” was created.')).toBeTruthy()
  expect(screen.getByRole('dialog', { name: 'New workspace' })).toBeTruthy()
  expect(window.location.pathname).toBe('/w/1/conversations')
})

test('an error from a save stays until the dialog is opened again', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': { status: 403, body: { error: 'no', code: 'workspace_creation_refused' } },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  fireEvent.change(screen.getByLabelText(/^Name of the new workspace/), { target: { value: 'X' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create workspace' }))
  await screen.findByRole('alert')
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  await screen.findByRole('dialog', { name: 'New workspace' })
  expect(screen.queryByRole('alert')).toBeNull()
})

test('a workspace made after the person logged out does not bring the shell back', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, can_create_workspace: true } },
    'POST /api/workspaces': async () => {
      await held // the answer comes after the logout below
      return {
        status: 201,
        body: { workspace: { id: 9, name: 'Late', role: 'admin' }, user: me.user, workspaces: me.workspaces },
      }
    },
    'POST /api/logout': { status: 204 },
    ...pages,
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  fireEvent.click(within(await openSwitcher()).getByRole('menuitem', { name: 'New workspace' }))
  fireEvent.change(screen.getByLabelText(/^Name of the new workspace/), { target: { value: 'Late' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create workspace' }))
  fireEvent.click(within(await openAccount()).getByRole('menuitem', { name: 'Log out' }))
  await screen.findByRole('heading', { name: 'Log in' })
  await act(async () => {
    release()
    await held
  })
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
})

test('someone with no workspace is offered "New workspace" when the server lets them', async () => {
  window.history.replaceState(null, '', '/')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { user: me.user, workspaces: [], can_create_workspace: true } },
    'POST /api/workspaces': {
      status: 201,
      body: {
        workspace: { id: 9, name: 'Mine', role: 'admin' },
        user: me.user,
        workspaces: [{ id: 9, name: 'Mine', role: 'admin' }],
      },
    },
    ...pages,
  })
  render(<App />)
  // The screen is the page's heading, and the form opens in a dialog, which fits a phone.
  await screen.findByRole('heading', { level: 1, name: 'No workspace yet' })
  // Logging out is offered beside it.
  expect(screen.getByRole('button', { name: 'Log out' })).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'New workspace' }))
  const dialog = screen.getByRole('dialog', { name: 'New workspace' })
  fireEvent.change(within(dialog).getByLabelText(/Name of the new workspace/), { target: { value: ' Mine ' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Create workspace' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(calls.find((c) => c.method === 'POST' && c.path === '/api/workspaces')?.body).toEqual({ name: 'Mine' })
  expect(window.location.pathname).toBe('/w/9/conversations')

  cleanup()
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { user: me.user, workspaces: [], can_create_workspace: false } },
  })
  render(<App />)
  await screen.findByText(/member of any workspace/)
  expect(screen.queryByRole('button', { name: 'New workspace' })).toBeNull()
})
