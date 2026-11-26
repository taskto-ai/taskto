// @vitest-environment jsdom

// A session that begins with no form submitted (a page that loads already signed in) ends a deletion
// notice that was waiting for someone to see it: it was not for the person whose session this is.

import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { sessionLostFor } from '../api'
import { resetSession } from '../session'
import { fakeFetch } from '../test/fakes'
import { ackDeletion, peekDeletion, rememberDeletion } from './deleted'

// The shell is not what is tested here.
vi.mock('../shell/Shell', () => ({ Shell: () => <p>the shell</p> }))

const { App } = await import('../App')

const PURGE = '2026-12-08T10:00:00.000Z'
const second = { user: { id: 2, email: 'next@example.com', name: 'Next' }, workspaces: [] }

beforeEach(() => {
  sessionStorage.clear()
  ackDeletion()
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  resetSession()
  ackDeletion()
})

test('a page that loads signed in as another account does not tell them the deletion waiting for the first one', async () => {
  // The first account deleted itself in a tab nobody was looking at: the notice waits for a page seen.
  let state: DocumentVisibilityState = 'hidden'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
  rememberDeletion(PURGE)
  // The second account signed in in another tab; this one reloads, and is theirs through load().
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: second },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
  })
  render(<App />)
  await screen.findByText('the shell')
  // Their session ends here, and the sign-in screen comes up on a page that is seen now.
  state = 'visible'
  act(() => {
    window.dispatchEvent(sessionLostFor(2))
  })
  await screen.findByLabelText('Password')
  expect(screen.queryByText(/Your account was deleted/)).toBeNull()
  await waitFor(() => expect(peekDeletion()).toBeNull())
})
