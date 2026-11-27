// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Me, Workspace } from '../api'
import { beginSession, endSession } from '../session'
import { fakeFetch } from '../test/fakes'
import { resetToasts } from '../ui/Toast'

const files = vi.hoisted(() => ({ release: [] as (() => void)[] }))
vi.mock('../i18n', async (original) => ({
  ...(await original<typeof import('../i18n')>()),
  setPreferences: () => new Promise<void>((done) => files.release.push(done)),
}))

const { SettingsPage } = await import('./SettingsPage')

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }

afterEach(() => {
  act(() => resetToasts())
  cleanup()
  files.release.length = 0
})

for (const ending of ['the person leaves the tab', 'the session ends'] as const) {
  test(`"Saved." isn't said when ${ending} while the language's file loads`, async () => {
    fakeFetch({ 'PATCH /api/workspaces/1': { body: { ...admin, locale: 'pt-BR' } } })
    beginSession(1)
    const { unmount } = render(<SettingsPage me={me} workspace={admin} />)
    fireEvent.change(await screen.findByRole('combobox', { name: 'Default language' }), { target: { value: 'pt-BR' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await vi.waitFor(() => expect(files.release).toHaveLength(1))
    if (ending === 'the person leaves the tab') unmount()
    else endSession()
    await act(async () => files.release.shift()?.())
    expect(screen.queryByText('Saved.')).toBeNull()
  })
}
