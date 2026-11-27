// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { DictationAddon, DictationStatus, Me, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const voz: DictationAddon = {
  id: 'desertant-voz',
  name: 'Voz',
  vendor: 'Desert Ant Labs',
  kind: 'speech-to-text',
  licence_name: 'Desert Ant Labs Source-Available License 1.1',
  licence_url: 'https://license.desertant.com/',
  notice: 'Its licence check contacts Desert Ant Labs every time the model loads.',
  attribution: {
    name: 'Voz',
    by: 'Desert Ant Labs',
    url: 'https://desertant.com',
    licence: 'Desert Ant Labs Source-Available License 1.1',
    licence_url: 'https://license.desertant.com/',
  },
  installed: true,
}

const status = (over: Partial<DictationStatus> = {}): DictationStatus => ({
  engine: 'local',
  settings: {
    engine: 'local',
    language: '',
    api: { base_url: 'https://api.openai.com/v1', model: 'whisper-1', api_key: 'not set' },
    accepted_notice: '',
  },
  local: { model: 'parakeet-v3', state: 'downloading', progress: 0.4 },
  addons: [voz],
  credits: [
    {
      name: 'Parakeet TDT 0.6B v3',
      by: 'NVIDIA',
      url: 'https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3',
      licence: 'CC-BY-4.0',
      licence_url: 'https://creativecommons.org/licenses/by/4.0/',
    },
  ],
  max_seconds: 600,
  ...over,
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const patches = (calls: Call[]) =>
  calls.filter((c) => c.method === 'PATCH' && c.path === '/api/workspaces/1/dictation').map((c) => c.body)

test('an admin sets an API key, which is never shown back', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/dictation': { body: status() },
    'PATCH /api/workspaces/1/dictation': { body: status() },
  })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  expect(await screen.findByText(/Dictation uses the local model, Parakeet v3/)).toBeTruthy()
  expect(screen.getByText(/downloading, 40%/)).toBeTruthy()

  fireEvent.change(screen.getByLabelText('Model'), { target: { value: 'whisper-large-v3' } })
  fireEvent.change(screen.getByLabelText('API key'), { target: { value: 'fake-speech-key' } })
  fireEvent.change(screen.getByLabelText('Language'), { target: { value: 'pt' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(patches(calls)).toHaveLength(1))
  expect(patches(calls)[0]).toEqual({
    engine: 'local',
    language: 'pt',
    api: { model: 'whisper-large-v3', api_key: 'fake-speech-key' },
  })
  await waitFor(() => expect((screen.getByLabelText('API key') as HTMLInputElement).value).toBe(''))
})

test('an add-on needs its notice read and confirmed first', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/dictation': { body: status() },
    'PATCH /api/workspaces/1/dictation': { body: status() },
  })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  fireEvent.change(await screen.findByLabelText('Engine'), { target: { value: 'desertant-voz' } })
  expect(screen.getByRole('note', { name: 'Voz notice' }).textContent).toContain('every time the model loads')
  expect(screen.getByRole('link', { name: 'Desert Ant Labs Source-Available License 1.1' }).getAttribute('href')).toBe(
    'https://license.desertant.com/',
  )
  const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
  expect(save.disabled).toBe(true)
  fireEvent.click(screen.getByRole('checkbox'))
  expect(save.disabled).toBe(false)
  fireEvent.click(save)
  await waitFor(() => expect(patches(calls)).toHaveLength(1))
  expect(patches(calls)[0]).toEqual({ engine: 'desertant-voz', language: '', accept_notice: 'desertant-voz' })
})

test('a member sees what is in use, and can change nothing', async () => {
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status({ engine: 'api' }) } })
  render(<SettingsPage me={me} workspace={member} tab="dictation" />)
  expect(await screen.findByText(/Dictation uses the hosted API/)).toBeTruthy()
  expect(screen.queryByRole('form', { name: 'Dictation settings' })).toBeNull()
  expect(screen.queryByLabelText('API key')).toBeNull()
})

test('About shows the credits of what the workspace uses', async () => {
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  render(<SettingsPage me={me} workspace={member} tab="about" />)
  const link = await screen.findByRole('link', { name: 'Parakeet TDT 0.6B v3' })
  expect(link.getAttribute('href')).toBe('https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3')
  expect(screen.getByRole('link', { name: 'CC-BY-4.0' }).getAttribute('href')).toBe(
    'https://creativecommons.org/licenses/by/4.0/',
  )
})

test('an admin removes the API key', async () => {
  const set = status({ engine: 'api' })
  set.settings.api.api_key = 'set'
  const calls = fakeFetch({
    'GET /api/workspaces/1/dictation': { body: set },
    'PATCH /api/workspaces/1/dictation': { body: status() },
  })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  expect((await screen.findByLabelText('API key')).getAttribute('placeholder')).toBe('set: type to replace it')
  fireEvent.click(screen.getByRole('button', { name: 'Remove the API key' }))
  await waitFor(() => expect(patches(calls)).toEqual([{ api: { api_key: null } }]))
})

test('About works on a server without dictation', async () => {
  fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="about" />)
  expect(await screen.findByText(/Nothing this workspace uses now asks for a credit/)).toBeTruthy()
  expect(screen.queryByRole('alert')).toBeNull()
})

test('the local model is the default engine, and a key alone never picks the API', async () => {
  const keyed = status()
  keyed.settings.api.api_key = 'set'
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: keyed } })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  const engine = (await screen.findByLabelText('Engine')) as HTMLSelectElement
  expect(engine.value).toBe('local')
  expect([...engine.options].map((o) => o.value)).toEqual(['local', 'api', 'desertant-voz'])
  expect(engine.options[0].textContent).toBe('The local model, on this server (default)')
  expect(screen.getByText(/setting a key doesn't switch to it/)).toBeTruthy()
})

test('where the operator turned the local model off, it is still listed, and only as the current choice', async () => {
  const off = status({ engine: '', local: null })
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: off } })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  const engine = (await screen.findByLabelText('Engine')) as HTMLSelectElement
  expect(engine.value).toBe('local')
  expect(engine.options[0].textContent).toBe('The local model (off on this server)')
  expect(engine.options[0].disabled).toBe(false)
  cleanup()

  const onAPI = status({ engine: 'api', local: null })
  onAPI.settings.engine = 'api'
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: onAPI } })
  render(<SettingsPage me={me} workspace={admin} tab="dictation" />)
  const again = (await screen.findByLabelText('Engine')) as HTMLSelectElement
  expect(again.value).toBe('api')
  expect(again.options[0].disabled).toBe(true)
})

test('the local model says On only once it is ready, Not ready yet before, and that it can’t run where it can’t; other engines are On', async () => {
  for (const [engine, state, badge] of [
    ['local', 'not_downloaded', 'Not ready yet'],
    ['local', 'downloading', 'Not ready yet'],
    ['local', 'failed', 'Not ready yet'],
    ['local', 'unavailable', 'Not available on this server'],
    ['local', 'ready', 'On'],
    ['api', 'not_downloaded', 'On'],
    ['api', 'unavailable', 'On'],
    ['desertant-voz', 'not_downloaded', 'On'],
  ] as const) {
    fakeFetch({
      'GET /api/workspaces/1/dictation': {
        body: status({ engine, local: { model: 'parakeet-v3', state, progress: 0.4 } }),
      },
    })
    const { unmount } = render(<SettingsPage me={me} workspace={member} tab="dictation" />)
    expect(await screen.findByText(badge, { exact: true }), `${engine} ${state}`).toBeTruthy()
    if (badge !== 'On') expect(screen.queryByText('On', { exact: true }), `${engine} ${state}`).toBeNull()
    unmount()
  }
})
