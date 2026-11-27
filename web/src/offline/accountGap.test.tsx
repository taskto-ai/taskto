// @vitest-environment jsdom

import { cleanup, render, screen } from '@testing-library/react'
import { useLayoutEffect } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { ACCOUNT_CHANGED_EVENT, sessionLostFor } from '../api'
import { resetSession, sessionUser } from '../session'
import { fakeFetch } from '../test/fakes'

const LAST_KEY = 'taskto:offline-last'

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
  onShellDrawn = () => queueMicrotask(anotherAccountSignsIn)
  render(<App />)
  await leavesTheFirstPersonsScreen()
})

test('a sign-in in another tab heard before App listened still ends this page’s session', async () => {
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
