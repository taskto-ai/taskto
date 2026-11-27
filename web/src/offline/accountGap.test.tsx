// @vitest-environment jsdom
//
// Another account signs in on this browser (in another tab) right as this page draws the first
// account's screen. With no offline runtime here (no IndexedDB: a private window, storage refused),
// only App hears it, and it must hear it whenever it comes: the page leaves the first person's
// screen, and shows only that the account changed. The end of the session, likewise, is heard then.

import { cleanup, render, screen } from '@testing-library/react'
import { useLayoutEffect } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { ACCOUNT_CHANGED_EVENT, sessionLostFor } from '../api'
import { resetSession, sessionUser } from '../session'
import { fakeFetch } from '../test/fakes'

const LAST_KEY = 'taskto:offline-last'

// What the test does as the shell is drawn: in the shell's own layout effect, which runs in the
// same commit before App's.
let onShellDrawn: (() => void) | null = null
vi.mock('../shell/Shell', () => ({
  Shell: () => {
    useLayoutEffect(() => {
      onShellDrawn?.()
    }, [])
    return <p>the first person's shell</p>
  },
}))

const { App } = await import('../App')

const first = { user: { id: 7, email: 'pat@example.com', name: 'Pat' }, workspaces: [] }

// Another tab signed in as someone else: the marker it moved, and the event this tab hears.
function anotherAccountSignsIn() {
  const marker = JSON.stringify({ id: 8, name: 'Bob' })
  window.localStorage.setItem(LAST_KEY, marker)
  window.dispatchEvent(new StorageEvent('storage', { key: LAST_KEY, newValue: marker }))
}

beforeEach(() => {
  expect(globalThis.indexedDB).toBeUndefined()
  window.localStorage.clear()
  onShellDrawn = null
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: first },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
  })
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  resetSession()
})

async function leavesTheFirstPersonsScreen() {
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/The account changed/)
  expect(screen.queryByText("the first person's shell")).toBeNull()
  expect(sessionUser()).toBeNull()
}

test('a sign-in in another tab heard the moment after the shell is drawn ends this page’s session', async () => {
  // A microtask: after the commit that drew the shell, before any passive effect.
  onShellDrawn = () => queueMicrotask(anotherAccountSignsIn)
  render(<App />)
  await leavesTheFirstPersonsScreen()
})

test('a sign-in in another tab heard before App listened still ends this page’s session', async () => {
  // Synchronously, as the shell is drawn: App's listener isn't there yet, the marker says it.
  onShellDrawn = anotherAccountSignsIn
  render(<App />)
  await leavesTheFirstPersonsScreen()
})

test('a request refused as another account’s the moment after the shell is drawn ends this page’s session', async () => {
  onShellDrawn = () =>
    queueMicrotask(() => {
      window.dispatchEvent(new CustomEvent(ACCOUNT_CHANGED_EVENT, { detail: { expected: 7 } }))
    })
  render(<App />)
  await leavesTheFirstPersonsScreen()
})

test('the session ending the moment after the shell is drawn takes the page to the sign-in screen', async () => {
  onShellDrawn = () =>
    queueMicrotask(() => {
      window.dispatchEvent(sessionLostFor(7))
    })
  render(<App />)
  await screen.findByLabelText('Password')
  expect(screen.queryByText("the first person's shell")).toBeNull()
  expect(sessionUser()).toBeNull()
})

test('a page whose own account is the one signed in keeps its screen', async () => {
  render(<App />)
  await screen.findByText("the first person's shell")
  expect(JSON.parse(window.localStorage.getItem(LAST_KEY) ?? 'null')?.id).toBe(7)
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(sessionUser()).toBe(7)
})
