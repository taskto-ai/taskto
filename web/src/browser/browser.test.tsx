// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { BrowserStatus } from '../api'
import { ConversationsPage } from '../conversations/ConversationsPage'
import { pendingHelp, transcript } from '../conversations/transcript'
import { setPreferences } from '../i18n'
import { parseRoute, usePath } from '../router'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { BrowserPanel } from './BrowserPanel'
import { doneMessage, notNowMessage } from './HelpCard'
import { LiveView } from './LiveView'
import { liveURL, type RFB, type RFBConstructor } from './novnc'

const conversation = {
  id: 7,
  workspace_id: 1,
  title: 'Bank statements',
  archived_at: null,
  created_at: '2026-11-19T10:00:00.000Z',
  updated_at: '2026-11-19T10:00:00.000Z',
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations/7'))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const ON: BrowserStatus = {
  enabled: true,
  browsers: {
    personal: { available: true, running: true, live: false, tabs: [] },
    testing: { available: true, running: false, live: false },
  },
}

const helpEntries = (message: string) => [
  { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'Download my bank statements' }] },
  {
    id: 2,
    kind: 'pi.assistant',
    model: [
      {
        role: 'assistant',
        content: [
          { type: 'toolCall', id: 'c1', name: 'browser_help', arguments: { tab: 'T1', reason: 'login', message } },
        ],
        stopReason: 'toolUse',
      },
    ],
  },
  {
    id: 3,
    kind: 'pi.toolResult',
    model: [
      {
        role: 'toolResult',
        toolCallId: 'c1',
        toolName: 'browser_help',
        content: [{ type: 'text', text: '{"status":"waiting_for_user"}' }],
      },
    ],
  },
  {
    id: 4,
    kind: 'pi.assistant',
    model: [{ role: 'assistant', content: [{ type: 'text', text: 'Please sign in to your bank.' }] }],
  },
]

function Page({ streams }: { streams: ReturnType<typeof fakeStreams>['factory'] }) {
  const route = parseRoute(usePath())
  return <ConversationsPage workspace={1} open={route?.item} streams={streams} />
}

async function open(entries: unknown[], browser: BrowserStatus = ON) {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'GET /api/me/browser': { body: browser },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: {} },
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send({ type: 'value', value: { entries, docs: { 'pi.live': {} } } })
  })
  return calls
}

test("the agent's request to take over waits for the user's answer", () => {
  const t = transcript({ entries: helpEntries('Sign in, then press Done.'), docs: { 'pi.live': {} } })
  expect(t.messages[1].tools[0].help).toEqual({ reason: 'login', message: 'Sign in, then press Done.' })
  expect(pendingHelp(t.messages)).toEqual({ reason: 'login', message: 'Sign in, then press Done.' })
  const answered = transcript({
    entries: [...helpEntries('x'), { id: 5, kind: 'pi.user', model: [{ role: 'user', content: 'Done' }] }],
    docs: { 'pi.live': {} },
  })
  expect(pendingHelp(answered.messages)).toBeNull()
})

test('the help card shows the request as plain text, and Done answers the agent', async () => {
  const message = '<img src=x onerror=alert(1)> Sign in to **your bank**.'
  const calls = await open(helpEntries(message))
  const card = await screen.findByRole('region', { name: 'The agent needs you in the browser' })
  expect(card.textContent).toContain('The agent needs you to sign in')
  const said = within(card).getByText(message)
  expect(said.textContent).toBe(message)
  expect(said.querySelector('img, strong')).toBeNull()
  expect(card.querySelector('img')).toBeNull()
  expect(screen.getByText('Asked you to take over the browser')).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Take over' }))
  const panel = await screen.findByRole('region', { name: 'Your browser' })
  expect(panel.textContent).toContain('You have the browser')
  expect(screen.getByRole('button', { name: 'Just watch' })).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Done' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/messages'))).toBe(true))
  const sent = calls.find((c) => c.method === 'POST' && c.path.endsWith('/messages'))
  expect(sent?.body).toEqual({ content: doneMessage(), request_id: expect.any(String) })
})

test('Not now answers the agent too', async () => {
  const calls = await open(helpEntries('Solve the captcha'))
  fireEvent.click(await screen.findByRole('button', { name: 'Not now' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/messages'))).toBe(true))
  const sent = calls.find((c) => c.method === 'POST' && c.path.endsWith('/messages'))
  expect(sent?.body).toEqual({ content: notNowMessage(), request_id: expect.any(String) })
})

test("with the browser off, there is no browser button, and the card can't take over", async () => {
  await open(helpEntries('Sign in'), {
    enabled: false,
    reason: 'the agent browser is off on this server (-browser)',
    browsers: {},
  })
  const takeOver = await screen.findByRole('button', { name: 'Take over' })
  expect((takeOver as HTMLButtonElement).disabled).toBe(true)
  fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
  expect(await screen.findByRole('menuitem', { name: 'Build an app in this conversation' })).toBeTruthy()
  expect(screen.queryByRole('menuitem', { name: 'Show your browser' })).toBeNull()
})

test('no card when nothing is asked', async () => {
  await open(helpEntries('Sign in').slice(0, 1))
  fireEvent.click(await screen.findByRole('button', { name: 'More actions' }))
  await screen.findByRole('menuitem', { name: 'Show your browser' })
  expect(screen.queryByRole('region', { name: 'The agent needs you in the browser' })).toBeNull()
})

const loaded = (data: BrowserStatus) => ({ data, error: null, reload: vi.fn() })

test('the panel says why the browser is off, and starts it on request', async () => {
  const off = loaded({ enabled: false, reason: 'no OS boundary between users', browsers: {} })
  const { unmount } = render(
    <BrowserPanel status={off} interactive={false} onInteractive={vi.fn()} onClose={vi.fn()} />,
  )
  expect(screen.getByText('no OS boundary between users')).toBeTruthy()
  unmount()

  await act(() => setPreferences({ user: 'pt-BR' }))
  const coded = loaded({ enabled: false, reason: 'the agent browser is off', reason_code: 'browser_off', browsers: {} })
  const second = render(<BrowserPanel status={coded} interactive={false} onInteractive={vi.fn()} onClose={vi.fn()} />)
  expect(screen.getByText('O navegador do agente está desligado neste servidor.')).toBeTruthy()
  second.unmount()
  await act(() => setPreferences({ user: null }))

  const calls = fakeFetch({ 'POST /api/me/browser/start': { body: ON } })
  const stopped = loaded({ ...ON, browsers: { personal: { available: true, running: false, live: false } } })
  render(<BrowserPanel status={stopped} interactive={false} onInteractive={vi.fn()} onClose={vi.fn()} />)
  fireEvent.click(screen.getByRole('button', { name: 'Start the browser' }))
  await waitFor(() => expect(stopped.reload).toHaveBeenCalled())
  expect(calls[0]).toMatchObject({ method: 'POST', path: '/api/me/browser/start', body: { browser: 'personal' } })
})

function fakeRFB() {
  const made: { url: string; options: unknown; client: RFB & { disconnected: boolean } }[] = []
  class Fake extends EventTarget {
    viewOnly = false
    scaleViewport = false
    resizeSession = true
    focusOnClick = true
    background = ''
    disconnected = false
    constructor(_target: HTMLElement, url: string, options?: unknown) {
      super()
      made.push({ url, options, client: this as unknown as RFB & { disconnected: boolean } })
    }
    disconnect() {
      this.disconnected = true
    }
    focus() {}
  }
  return { made, load: () => Promise.resolve(Fake as unknown as RFBConstructor) }
}

test("the live view watches until the user takes over, on this server's WebSocket", async () => {
  const { made, load } = fakeRFB()
  const { rerender, unmount } = render(<LiveView interactive={false} load={load} />)
  await waitFor(() => expect(made).toHaveLength(1))
  expect(made[0].url).toBe(liveURL())
  expect(made[0].url).toBe(`ws://${window.location.host}/api/me/browser/live`)
  expect(made[0].options).toEqual({ wsProtocols: ['binary'] })
  const client = made[0].client
  act(() => {
    client.dispatchEvent(new Event('connect'))
  })
  expect(client.viewOnly).toBe(true)
  expect(client.scaleViewport).toBe(true)
  rerender(<LiveView interactive load={load} />)
  expect(client.viewOnly).toBe(false)
  unmount()
  expect(client.disconnected).toBe(true)
})

test('a view the server closed can reconnect; our own teardown reports no close', async () => {
  const { made, load } = fakeRFB()
  const onClosed = vi.fn()
  const { unmount } = render(<LiveView interactive={false} load={load} onClosed={onClosed} />)
  await waitFor(() => expect(made).toHaveLength(1))
  act(() => {
    made[0].client.dispatchEvent(new Event('disconnect'))
  })
  expect(onClosed).toHaveBeenCalledTimes(1)
  fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
  await waitFor(() => expect(made).toHaveLength(2))
  expect(made[0].client.disconnected).toBe(true)
  unmount()
  act(() => {
    made[1].client.dispatchEvent(new Event('disconnect'))
  })
  expect(onClosed).toHaveBeenCalledTimes(1)
})

test('the live view uses wss on https', () => {
  expect(liveURL({ protocol: 'https:', host: 'tasks.example.com' } as Location)).toBe(
    'wss://tasks.example.com/api/me/browser/live',
  )
})
