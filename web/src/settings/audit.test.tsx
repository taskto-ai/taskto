// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { AuditEntry, AuditPage, Me, Workspace } from '../api'
import { setPreferences } from '../i18n'
import en from '../i18n/locales/en.json'
import ptBR from '../i18n/locales/pt-BR.json'
import { fakeFetch } from '../test/fakes'
import { AUDIT_ACTIONS, AUDIT_DETAILS, AUDIT_TARGETS } from './AuditTab'
import { SettingsPage } from './SettingsPage'

const me: Me = { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [] }
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const members = {
  members: [
    { id: 1, email: 'owner@example.com', name: 'Owner', role: 'admin', joined_at: '2026-01-01T00:00:00.000Z' },
    { id: 2, email: 'bea@example.com', name: 'Bea', role: 'member', joined_at: '2026-01-01T00:00:00.000Z' },
  ],
}

function entry(over: Partial<AuditEntry>): AuditEntry {
  return {
    id: 1,
    at: '2026-03-05T14:30:00.000Z',
    action: 'space_created',
    actor_id: 1,
    actor: { name: 'Owner', email: 'owner@example.com' },
    target_type: 'space',
    target_id: 7,
    target_user: null,
    target_name: 'Team',
    details: {},
    client_addr: '203.0.113.9',
    ...over,
  }
}

const first: AuditPage = {
  next: 'cursor-1',
  events: [
    entry({ id: 3 }),
    entry({
      id: 2,
      action: 'member_removed',
      target_type: 'user',
      target_id: 9,
      target_name: null,
      details: { role: 'member' },
    }),
  ],
}

const log = () => within(screen.getByRole('list', { name: 'Audit log' }))

test('admins read the log: actions translated, names, dates in their format, and a former member by number', async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: members },
    'GET /api/workspaces/1/audit?limit=50': { body: first },
  })
  render(<SettingsPage me={me} workspace={admin} tab="audit" />)
  await screen.findByRole('list', { name: 'Audit log' })
  expect(log().getByText('Shared space made')).toBeTruthy()
  expect(log().getByText('Member removed')).toBeTruthy()
  expect(screen.getAllByText('By Owner').length).toBe(2)
  expect(screen.getByText('Shared space: Team')).toBeTruthy()
  // A member who has left has no name in the reply: the number is shown.
  expect(screen.getByText('Member number 9')).toBeTruthy()
  expect(screen.getByText('Role')).toBeTruthy()
  expect(screen.getAllByText('203.0.113.9').length).toBe(2)
  const time = screen.getAllByRole('time')[0]
  expect(time.getAttribute('datetime')).toBe('2026-03-05T14:30:00.000Z')
  expect(time.textContent).toMatch(/2026/)
  expect(screen.getByRole('button', { name: 'Show older events' })).toBeTruthy()
})

test('filters go to the server, and older events are asked for with the cursor', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: members },
    'GET /api/workspaces/1/audit?limit=50': { body: first },
    'GET /api/workspaces/1/audit?limit=50&cursor=cursor-1': {
      body: {
        next: '',
        events: [entry({ id: 1, action: 'invite_created', target_type: 'invite', target_name: null })],
      },
    },
    'GET /api/workspaces/1/audit?limit=50&action=member_removed&actor=2&target_type=user': {
      body: { next: '', events: [] },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="audit" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Show older events' }))
  expect(await log().findByText('Invite sent')).toBeTruthy()
  expect(log().getByText('Shared space made')).toBeTruthy() // the first page stays
  expect(screen.queryByRole('button', { name: 'Show older events' })).toBeNull()

  await screen.findByRole('option', { name: 'Bea' })
  fireEvent.change(screen.getByLabelText('Action'), { target: { value: 'member_removed' } })
  fireEvent.change(screen.getByLabelText('Done by'), { target: { value: '2' } })
  fireEvent.change(screen.getByLabelText('About'), { target: { value: 'user' } })
  expect(await screen.findByText('No events match these filters.')).toBeTruthy()
  expect(calls.map((c) => c.path).at(-1)).toBe(
    '/api/workspaces/1/audit?limit=50&action=member_removed&actor=2&target_type=user',
  )
  fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
  await screen.findByRole('list', { name: 'Audit log' })
  expect(log().getByText('Member removed')).toBeTruthy()
})

test('a date range is sent as the whole days, from the start of the first to the end of the last', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: members },
    'GET /api/workspaces/1/audit?limit=50': { body: { events: [], next: '' } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="audit" />)
  expect(await screen.findByText('Nothing has been recorded yet.')).toBeTruthy()
  fireEvent.change(screen.getByLabelText('From'), { target: { value: '2026-03-01' } })
  fireEvent.change(screen.getByLabelText('Until'), { target: { value: '2026-03-02' } })
  await screen.findByText('No events match these filters.').catch(() => undefined)
  const last = new URL(calls.at(-1)?.path ?? '', 'http://localhost').searchParams
  expect(last.get('from')).toBe(new Date(2026, 2, 1).toISOString())
  expect(last.get('to')).toBe(new Date(2026, 2, 3).toISOString())
})

test("members don't see the audit log, and don't ask for it", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="audit" />)
  expect(screen.queryByRole('link', { name: 'Audit log' })).toBeNull()
  expect(screen.queryByRole('heading', { name: 'Audit log' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('/audit'))).toEqual([])
})

test('the tab and the events are in the admin language', async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: members },
    'GET /api/workspaces/1/audit?limit=50': { body: first },
  })
  await act(() => setPreferences({ user: 'pt-BR' }))
  render(<SettingsPage me={me} workspace={admin} tab="audit" />)
  await screen.findByRole('list', { name: 'Registro de auditoria' })
  expect(
    within(screen.getByRole('list', { name: 'Registro de auditoria' })).getByText('Espaço compartilhado criado'),
  ).toBeTruthy()
  expect(screen.getByText('Espaço compartilhado: Team')).toBeTruthy()
  expect(within(screen.getByRole('navigation')).getByText('Auditoria')).toBeTruthy()
  await act(() => setPreferences({ user: null }))
})

// The server's lists, read from its source: a new action, target or detail there needs a label here.
function goNames(file: string, pattern: RegExp): string[] {
  const src = readFileSync(resolve(__dirname, '../../../internal/audit', file), 'utf8')
  return [...src.matchAll(pattern)].map((m) => m[1])
}

test("every action, target type and detail of the server's log has a label in both languages", () => {
  const actions = goNames('actions.go', /^\t\w+\s+= "([a-z_]+)"$/gm)
  const targets = actions.splice(actions.indexOf('user'))
  expect([...AUDIT_ACTIONS].sort()).toEqual([...actions].sort())
  expect([...AUDIT_TARGETS].sort()).toEqual([...targets].sort())

  const schema = readFileSync(resolve(__dirname, '../../../internal/audit/schema.go'), 'utf8')
  const details = new Set([...schema.matchAll(/"([a-z_]+)":\s/g)].map((m) => m[1]))
  for (const d of details) expect(AUDIT_DETAILS as readonly string[], `detail ${d}`).toContain(d)

  for (const [tag, catalog] of [
    ['en', en],
    ['pt-BR', ptBR],
  ] as const) {
    const audit = catalog.settings.audit as unknown as Record<string, Record<string, string>>
    for (const a of actions) expect(audit.actions[a], `${tag}: action ${a}`).toBeTruthy()
    for (const x of targets) expect(audit.targets[x], `${tag}: target ${x}`).toBeTruthy()
    for (const d of AUDIT_DETAILS) expect(audit.details[d], `${tag}: detail ${d}`).toBeTruthy()
  }
})
