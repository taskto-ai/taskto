// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Me, ProviderAccount, Workspace } from '../api'
import { fakeFetch } from '../test/fakes'
import { chooseFromMenu } from '../test/ui'
import { SettingsPage } from './SettingsPage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }

const mine: ProviderAccount = {
  id: 9,
  scope: 'user',
  provider: 'u1-local',
  name: 'local',
  display_name: 'Local model',
  kind: 'openai-compatible',
  base_url: 'http://localhost:11434/v1',
  api_key: 'not set',
  models: [{ id: 'small-model' }],
  created_at: '',
  updated_at: '',
}

test('after removing an account, the keyboard goes to its section’s title', async () => {
  let list = [mine]
  fakeFetch({
    'GET /api/workspaces/1/providers': { body: { providers: [] } },
    'GET /api/me/providers': () => ({ body: { providers: list } }),
    'DELETE /api/me/providers/9': () => {
      list = []
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="providers" />)
  await screen.findByText('Local model')
  chooseFromMenu('More actions for Local model', 'Remove')
  fireEvent.click(await screen.findByRole('button', { name: 'Yes, remove' }))
  const heading = screen.getByRole('heading', { level: 2, name: 'Your own accounts' })
  await waitFor(() => expect(document.activeElement).toBe(heading))
})
