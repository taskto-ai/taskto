// The conversations page in a real browser with no server: a fake API and a replayed events
// stream, for looking at the page and capturing it where the agent runtime can't run (see
// capture.mjs). Development only: the Vite dev server serves it at /e2e/conversation/, and no
// build includes it. Sample data only.
//
// ?s= picks the scenario: chat (default), running, cli, cli-allow (a question with "allow all"
// on), browser (the agent asks to take over a running browser), long (many messages), flaky (the
// first read of the conversation fails with a 502), list, empty, failed. ?lang=pt-BR or ?dir=rtl
// to check another language or the right-to-left layout.

import '@fontsource-variable/bricolage-grotesque/wght.css'
import '@fontsource-variable/inter/wght.css'
import '@fontsource-variable/jetbrains-mono/wght.css'
import '../../src/ui/index.css'
import '../../src/styles.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { ConversationsPage } from '../../src/conversations/ConversationsPage'
import type { EventSourceFactory } from '../../src/conversations/useConversationStream'
import { initLocale } from '../../src/i18n'

const params = new URLSearchParams(window.location.search)
const scenario = params.get('s') ?? 'chat'
if (params.get('lang')) localStorage.setItem('taskto.locale', params.get('lang') as string)
if (params.get('dir') === 'rtl') document.documentElement.dir = 'rtl'

const now = Date.now()
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString()

const conv = (id: number, title: string, minutes: number) => ({
  id,
  workspace_id: 1,
  title,
  archived_at: null,
  created_at: ago(minutes + 30),
  updated_at: ago(minutes),
  cli_permissions: 'ask',
})
const list = [
  {
    ...conv(7, scenario.startsWith('cli') ? 'Fix the failing tests' : 'Plan the launch week', 2),
    cli_permissions: scenario === 'cli-allow' ? 'allow_all' : 'ask',
  },
  conv(6, 'Summarise the weekly notes', 60 * 26),
  conv(5, 'Expense tracker with receipts', 60 * 24 * 3),
  conv(4, 'Onboarding checklist copy', 60 * 24 * 5),
]

// Entries of the conversation as the agent runtime keeps them.
let n = 100
const user = (text: string) => ({ id: n++, kind: 'pi.user', model: [{ role: 'user', content: text }] })
const said = (text: string, calls: { name: string; args?: unknown }[] = []) => ({
  id: n++,
  kind: 'pi.assistant',
  model: [
    {
      role: 'assistant',
      content: [
        ...(text ? [{ type: 'text', text }] : []),
        ...calls.map((c, i) => ({ type: 'toolCall', id: `c${n}-${i}`, name: c.name, arguments: c.args ?? {} })),
      ],
      stopReason: calls.length ? 'toolUse' : 'stop',
    },
  ],
})
const result = (call: string, name: string, text: string, details?: unknown) => ({
  id: n++,
  kind: 'pi.tool',
  model: [
    {
      role: 'toolResult',
      toolCallId: call,
      toolName: name,
      content: [{ type: 'text', text }],
      details,
      isError: false,
    },
  ],
})
// An assistant entry that calls one tool, and its result.
const tool = (name: string, text: string, args: unknown = {}, details?: unknown) => {
  const call = said('', [{ name, args }])
  const id = (call.model[0].content[0] as { id: string }).id
  return [call, result(id, name, text, details)]
}

const savedApp = {
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
  access: {
    roles: ['builder', 'publisher', 'user', 'data_manager', 'reader'],
    restricted: false,
    view: true,
    design: true,
    publish: true,
    submit: true,
    manage_roles: true,
    read_records: 'all',
    modify_records: 'all',
  },
  live: {
    number: 1,
    definition_version: 1,
    requires: ['component.Text'],
    published_at: ago(10),
    published_by: null,
    live: true,
  },
  unpublished_changes: false,
  draft: { definition_version: 1, requires: ['component.Text'] },
}

function chatEntries() {
  return [
    user('List the documents in my knowledge base and summarise the launch plan.'),
    ...tool('list_documents', '6 documents'),
    ...tool('read_document', '# Launch plan', { path: 'launch/plan.md' }),
    said(
      'You have six documents. The launch plan sets the beta for **December 8** and has three steps left:\n\n- Finish onboarding\n- Write the launch post\n- Record the demo',
    ),
    user('Make me a small app to track expenses with receipts.'),
    ...tool('create_app', JSON.stringify(savedApp)),
    said('Done. **Expenses** is saved in Private and published: add a row to try it.'),
    user('Add a card to my board: “Book the launch call”, due next Friday.'),
    said('', [{ name: 'create_card' }]),
    {
      id: n++,
      kind: 'pi.assistant',
      model: [
        {
          role: 'assistant',
          content: [],
          stopReason: 'error',
          errorMessage: 'The provider is rate limited. Try again in a minute.',
        },
      ],
    },
  ]
}

const cliCall = said('', [{ name: 'cli_turn', args: { cli: 'claude', model: 'default' } }])
const cliCallId = (cliCall.model[0].content[0] as { id: string }).id

const cliView = {
  entries: [user('Fix the failing tests in the importer.'), cliCall],
  docs: {
    'pi.live': {
      run: { taskId: 4 },
      tools: [
        {
          callId: cliCallId,
          name: 'cli_turn',
          status: 'running',
          output: '$ go test ./internal/importer/...\n--- FAIL: TestDates (0.01s)\n? Claude Code asks to use Bash',
        },
      ],
    },
  },
}

// Sixteen messages, for a transcript longer than the window.
function longEntries() {
  const out: unknown[] = []
  for (let i = 1; i <= 8; i++) {
    out.push(user(`Question ${i}: what is left on the launch plan?`))
    out.push(
      said(`Answer ${i}. Three steps are left:\n\n- Finish onboarding\n- Write the launch post\n- Record the demo`),
    )
  }
  return out
}

type View = { entries: unknown[]; before?: number; docs?: Record<string, unknown> }
const views: Record<string, View | undefined> = {
  'cli-allow': cliView,
  long: { entries: longEntries() },
  flaky: { entries: chatEntries().slice(0, 4) },
  browser: {
    entries: [
      user('Download my statements from the bank.'),
      said('', [
        {
          name: 'browser_help',
          args: { tab: 'T1', reason: 'login', message: 'Sign in to the bank, then press Done.' },
        },
      ]),
    ],
  },
  chat: { entries: chatEntries(), before: 99 },
  running: {
    entries: [
      user('Draft the launch post from the plan, in a friendly tone.'),
      ...tool('read_document', '# Launch plan', { path: 'launch/plan.md' }),
    ],
    docs: {
      'pi.live': {
        run: { taskId: 3 },
        generation: {
          message: {
            role: 'assistant',
            content: [
              { type: 'text', text: 'Here is a first draft.\n\n**We are launching on December 8.** After months of' },
            ],
            stopReason: 'pending',
          },
        },
      },
    },
  },
  cli: cliView,
}

const providers = [
  { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
  {
    provider: 'cli-claude',
    name: 'Claude Code',
    scope: 'cli',
    models: [{ id: 'default', name: 'its default model' }],
  },
]

const routes: Record<string, { status?: number; body?: unknown }> = {
  'GET /api/workspaces/1/conversations': {
    body: { conversations: scenario === 'empty' ? [] : list },
  },
  'GET /api/workspaces/1/conversations?archived=true': { body: { conversations: [] } },
  'GET /api/workspaces/1/conversations/7':
    scenario === 'failed' ? { status: 404, body: { error: 'not found', code: 'not_found' } } : { body: list[0] },
  'GET /api/workspaces/1/models': { body: { providers } },
  'GET /api/workspaces/1/conversations/7/model': {
    body: scenario.startsWith('cli')
      ? { provider: 'cli-claude', model_id: 'default' }
      : { provider: 'script', model_id: 'script-1' },
  },
  'GET /api/me/cli-accounts': {
    body: {
      enabled: true,
      mode: 'local',
      accounts: [
        {
          id: 'claude',
          cli: 'claude',
          slot: 1,
          label: '',
          name: 'Claude Code',
          available: true,
          state: scenario.startsWith('cli') ? 'reconnect' : 'connected',
          auth_method: '',
          limits: [],
          limits_at: null,
          login: null,
        },
      ],
    },
  },
  'GET /api/workspaces/1/conversations/7/cli-permissions': {
    body: {
      requests: [
        {
          id: 'q1',
          turn: 't1',
          cli: 'claude',
          tool: 'Bash',
          input: 'go test ./internal/importer/... -run TestDates',
          asked_at: ago(0),
        },
      ],
    },
  },
  'GET /api/me/browser': {
    body: {
      enabled: true,
      browsers: { personal: { available: true, running: scenario === 'browser', live: scenario === 'browser' } },
    },
  },
  'GET /api/workspaces/1/dictation': {
    body: {
      engine: 'api',
      settings: { engine: 'api', language: '', api: { base_url: '', model: '', api_key: 'set' }, accepted_notice: '' },
      local: null,
      addons: [],
      credits: [],
      max_seconds: 600,
    },
  },
  'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp },
  'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  'GET /api/workspaces/1/conversations/7/entries?before=99': {
    status: 503,
    body: { error: 'The agent runtime is off.', code: 'agent_off' },
  },
}

let failedOnce = false
window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
  const url = new URL(String(input), window.location.origin)
  const method = init?.method ?? 'GET'
  // The flaky scenario's first read of the conversation fails, as a restarting server's would.
  if (scenario === 'flaky' && method === 'GET' && url.pathname === '/api/workspaces/1/conversations/7' && !failedOnce) {
    failedOnce = true
    return new Response(JSON.stringify({ error: 'bad gateway' }), {
      status: 502,
      headers: { 'Content-Type': 'application/json' },
    })
  }
  const route = routes[`${method} ${url.pathname}${url.search}`] ?? routes[`${method} ${url.pathname}`]
  const reply = route ?? (method === 'GET' ? { status: 404, body: { error: 'not found' } } : { body: {} })
  return new Response(JSON.stringify(reply.body ?? {}), {
    status: reply.status ?? 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

// The stream: opens at once and sends the scenario's view; a failed conversation never opens.
const streams: EventSourceFactory = () => {
  const es = {
    readyState: 0,
    onopen: null as ((ev: Event) => unknown) | null,
    onmessage: null as ((ev: MessageEvent) => unknown) | null,
    onerror: null as ((ev: Event) => unknown) | null,
    close() {
      es.readyState = 2
    },
  }
  const view = views[scenario] ?? { entries: [] }
  if (scenario !== 'failed')
    setTimeout(
      () => {
        es.readyState = 1
        es.onopen?.(new Event('open'))
        es.onmessage?.(new MessageEvent('message', { data: JSON.stringify({ type: 'value', value: view }) }))
      },
      scenario === 'flaky' ? 800 : 50,
    )
  return es as unknown as ReturnType<EventSourceFactory>
}

await initLocale()
const open = scenario === 'list' || scenario === 'empty' ? undefined : '7'
history.replaceState(null, '', `/w/1/conversations${open ? `/${open}` : ''}${window.location.search}`)
const root = document.getElementById('root') as HTMLElement
root.style.height = '100dvh'
createRoot(root).render(
  <StrictMode>
    <main className="tt-page" style={{ height: '100%' }}>
      <ConversationsPage workspace={1} open={open} streams={streams} />
    </main>
  </StrictMode>,
)
