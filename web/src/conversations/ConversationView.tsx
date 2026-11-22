// One conversation: its title (renamed in place), archiving, the transcript over the events
// stream with earlier messages on demand, the apps the agent saved in it, the model it runs on,
// and the composer, which steers or stops the agent while it works.
//
// A conversation on a CLI worker shows the CLI's work as it streams, as plain text, and when
// the user's login to that CLI has expired, a banner to sign in again right here: the turn
// waits for it and goes on by itself.

import {
  type FormEvent,
  type KeyboardEvent,
  lazy,
  type ReactNode,
  Suspense,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from 'react'
import {
  api,
  BROWSER,
  type BrowserStatus,
  CLI_ACCOUNTS,
  CLI_PRODUCTS,
  type CliAccount,
  type CliAccounts,
  type CliName,
  type CliQuestion,
  type Conversation,
  cliAccountFor,
  cliAccountTitle,
  cliOfProvider,
  codeText,
  errorText,
  ws,
} from '../api'
import { appPath } from '../apps/route'
import { BrowserPanel } from '../browser/BrowserPanel'
import { HelpCard } from '../browser/HelpCard'
import { browserLocale, format, type T, t as translate, useT } from '../i18n'
import { useLoad } from '../load'
import { Markdown } from '../markdown'
import { Link, routePath } from '../router'
import { CliLoginFlow } from '../settings/CliAccountsTab'
import { Icon } from '../shell/Icon'
import { useDictation } from './Dictation'
import { type History, useHistory } from './history'
import { type ChatMessage, CLI_TOOL, HELP_TOOL, pendingHelp, type ToolUse, transcript } from './transcript'
import { type EventSourceFactory, type StreamStatus, useConversationStream } from './useConversationStream'

// Its title, or for an untitled one, when it began. Outside a hook, for lists elsewhere too: a
// component that shows it also calls useT(), so it re-renders when the language changes.
export function conversationTitle(c: Pick<Conversation, 'title' | 'created_at'>): string {
  return c.title.trim() || translate('conversations.untitled', { date: format().dateTime(c.created_at) })
}

// How near the bottom the reader must be for the transcript to follow new text.
const FOLLOW_PX = 48

// The A2UI renderer is most of the app's code: loaded when a conversation shows an app.
const AppPreview = lazy(() => import('../apps/AppPreview').then((m) => ({ default: m.AppPreview })))

// How many apps a conversation shows launched, the most recently saved: each one launches its
// app on the server, so a conversation that built many doesn't launch them all.
export const LIVE_APPS = 3

// The tool uses that show their app launched: each app at its latest save only, so the one copy
// shown is the app as it is now and its actions leave no stale twin, for the LIVE_APPS most
// recently saved apps. The others link to theirs.
function liveApps(messages: ChatMessage[]): Set<ToolUse> {
  const live = new Set<ToolUse>()
  const seen = new Set<string>()
  for (const m of [...messages].reverse()) {
    for (const t of [...m.tools].reverse()) {
      if (!t.app || seen.has(`${t.app.space}-${t.app.app}`)) continue
      seen.add(`${t.app.space}-${t.app.app}`)
      if (seen.size > LIVE_APPS) return live
      live.add(t)
    }
  }
  return live
}

const STATUS_LABEL = {
  connecting: 'conversations.status.connecting',
  live: 'conversations.status.live',
  reconnecting: 'conversations.status.reconnecting',
  gone: 'conversations.status.gone',
} as const satisfies Record<StreamStatus, string>

export function ConversationView({
  workspace,
  id,
  backHref,
  onChanged,
  streams,
}: {
  workspace: number
  id: number
  backHref: string
  // The conversation was sent to, renamed or archived: the list is out of date.
  onChanged: () => void
  streams?: EventSourceFactory
}) {
  const t = useT()
  const base = `${ws(workspace)}/conversations/${id}`
  const meta = useLoad(() => api<Conversation>(base), [base])
  const { view, status } = useConversationStream(`${base}/events`, streams)
  const loadPage = useCallback((before: number) => api<History>(`${base}/entries?before=${before}`), [base])
  const older = useHistory(view, loadPage)
  const { messages, running, truncated } = transcript(
    view && older.history ? { ...view, entries: older.history.entries, before: older.history.before } : view,
  )
  const archived = Boolean(meta.data?.archived_at)
  const apps = liveApps(messages)

  // The model it runs on, and what the caller may choose; with a CLI among either, the
  // caller's CLI accounts, for the picker's hints and the Reconnect banner.
  const providers = useLoad(
    () => api<{ providers: ModelProvider[] }>(`${ws(workspace)}/models`).then((r) => r.providers),
    [workspace],
  )
  const loadedModel = useLoad(() => api<ModelRef>(`${base}/model`), [base])
  const [picked, setPicked] = useState<ModelRef | null>(null)
  const model = picked ?? loadedModel.data
  const usesCli = Boolean(cliOfProvider(model?.provider) || providers.data?.some((p) => p.scope === 'cli'))
  const cli = useLoad(() => (usesCli ? api<CliAccounts>(CLI_ACCOUNTS) : Promise.resolve(null)), [usesCli])
  // A turn that finds the login expired says so in its output: the account is read again then,
  // and when a run starts or ends.
  const asks = messages.reduce((n, m) => n + m.tools.filter((t) => t.cli && t.output?.includes('⚠')).length, 0)
  const reloadCli = cli.reload
  // biome-ignore lint/correctness/useExhaustiveDependencies: read again when these change
  useEffect(() => {
    if (cliOfProvider(model?.provider)) reloadCli()
  }, [asks, running])
  const reconnect = reconnectAccount(model, cli.data)
  const cliName = cliOfProvider(model?.provider)
  // A CLI's question shows in its output ("? Claude Code asks to use …") and its answer
  // ("✓" / "✗"): the questions are read again then, and when a run starts or ends.
  const questionLines = messages.reduce(
    (n, m) => n + m.tools.reduce((k, t) => k + (t.cli ? (t.output?.match(/^(\? | {2}[✓✗] )/gm)?.length ?? 0) : 0), 0),
    0,
  )

  // The user's own browser (docs/agent-browser.md): its panel, and the agent's request to take it
  // over (browser_help) until the user answers it.
  const browser = useLoad(() => api<BrowserStatus>(BROWSER), [])
  const [browserOpen, setBrowserOpen] = useState(false)
  const [takeOver, setTakeOver] = useState(false)
  const help = pendingHelp(messages)
  const reloadBrowser = browser.reload
  // biome-ignore lint/correctness/useExhaustiveDependencies: read again when a request comes or a run ends
  useEffect(() => {
    if (help) reloadBrowser()
  }, [help?.message, running])
  const changed = () => {
    meta.reload()
    onChanged()
  }

  // Follows the newest text as it grows, but only while the reader is at the bottom: someone
  // who scrolled up to re-read stays where they are. Only the transcript scrolls, never the page.
  const scroller = useRef<HTMLDivElement>(null)
  const atBottom = useRef(true)
  const last = messages.at(-1)
  // biome-ignore lint/correctness/useExhaustiveDependencies: follows the newest text as it grows
  useEffect(() => {
    const el = scroller.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [last?.key, last?.text])

  // Earlier messages go in above what the reader is looking at, which stays where it is.
  const fromBottom = useRef<number | null>(null)
  const loadEarlier = () => {
    const el = scroller.current
    if (el) fromBottom.current = el.scrollHeight - el.scrollTop
    older.loadEarlier()
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs when earlier messages arrive
  useLayoutEffect(() => {
    const el = scroller.current
    if (el && fromBottom.current !== null) el.scrollTop = el.scrollHeight - fromBottom.current
    fromBottom.current = null
  }, [messages[0]?.key])

  return (
    <section className="tt-chat" aria-label={t('conversations.conversation')}>
      <div className="tt-pane-head">
        <Link href={backHref} className="tt-icon-button tt-back" aria-label={t('conversations.allConversations')}>
          <Icon name="back" />
        </Link>
        {meta.data ? (
          <TitleBar conversation={meta.data} url={base} onChanged={changed} />
        ) : (
          <h2 className="tt-chat-title">{meta.error ? t('conversations.conversation') : '…'}</h2>
        )}
        {browser.data?.enabled && (
          <button
            type="button"
            className="tt-icon-button"
            aria-label={browserOpen ? t('conversations.hideBrowser') : t('conversations.showBrowser')}
            aria-pressed={browserOpen}
            onClick={() => {
              setBrowserOpen(!browserOpen)
              setTakeOver(false)
              browser.reload()
            }}
          >
            <Icon name="browser" />
          </button>
        )}
        <span className="tt-status" data-status={status}>
          {t(STATUS_LABEL[status])}
        </span>
      </div>
      {meta.error && (
        <p role="alert" className="tt-error">
          {meta.error}
        </p>
      )}
      {reconnect && <ReconnectBanner account={reconnect} onConnected={cli.reload} />}
      {cliName && <CliQuestions url={base} cli={cliName} trigger={`${questionLines}-${running}`} running={running} />}
      {cliName && meta.data && !archived && (
        <CliPermissionsSwitch conversation={meta.data} url={base} cli={cliName} onChanged={meta.reload} />
      )}
      {help && !archived && (
        <HelpCard
          help={help}
          conversation={base}
          canTakeOver={Boolean(browser.data?.enabled)}
          onTakeOver={() => {
            setBrowserOpen(true)
            setTakeOver(true)
            browser.reload()
          }}
          onAnswered={() => {
            setTakeOver(false)
            onChanged()
          }}
        />
      )}
      {browserOpen && (
        <BrowserPanel
          status={browser}
          interactive={takeOver}
          onInteractive={setTakeOver}
          onClose={() => {
            setBrowserOpen(false)
            setTakeOver(false)
          }}
        />
      )}
      {/* Screen readers hear when the agent starts and when it has replied, not each word of a
          reply as it streams in. */}
      <p className="tt-sr-only" role="status">
        {running
          ? t('conversations.agentWorking')
          : last?.role === 'assistant' && !last.pending
            ? t('conversations.agentReplied')
            : ''}
      </p>
      <div
        className="tt-transcript"
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_PX
        }}
      >
        {truncated && (
          <div className="tt-earlier">
            <button type="button" className="tt-button" onClick={loadEarlier} disabled={older.loading}>
              {older.loading ? t('app.loading') : t('conversations.loadEarlier')}
            </button>
            {older.error && <p className="tt-error tt-small">{older.error}</p>}
          </div>
        )}
        {view && messages.length === 0 && <p className="tt-muted tt-center-text">{t('conversations.start')}</p>}
        {messages.map((m) => (
          <Message key={m.key} message={m} workspace={workspace} apps={apps} />
        ))}
        {running && !last?.pending && <p className="tt-muted tt-small">{t('conversations.agentWorkingLive')}</p>}
      </div>
      <Composer workspace={workspace} base={base} archived={archived} running={running} onSent={onChanged}>
        {providers.data && model && (
          <ModelPicker
            workspace={workspace}
            url={`${base}/model`}
            providers={providers.data}
            current={model}
            cli={cli.data}
            onChosen={setPicked}
            disabled={archived}
          />
        )}
      </Composer>
    </section>
  )
}

function TitleBar({
  conversation,
  url,
  onChanged,
}: {
  conversation: Conversation
  url: string
  onChanged: () => void
}) {
  const t = useT()
  const [editing, setEditing] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const archived = Boolean(conversation.archived_at)

  const patch = async (body: { title?: string; archived?: boolean }) => {
    setBusy(true)
    setError(null)
    try {
      await api(url, { method: 'PATCH', body })
      setEditing(false)
      onChanged()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const save = (e: FormEvent) => {
    e.preventDefault()
    patch({ title })
  }

  if (editing) {
    return (
      <form className="tt-chat-title tt-rename" onSubmit={save}>
        <input
          aria-label={t('conversations.titleField')}
          value={title}
          maxLength={200}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') setEditing(false)
          }}
          // biome-ignore lint/a11y/noAutofocus: the field the reader just asked for
          autoFocus
        />
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {t('conversations.save')}
        </button>
        <button type="button" className="tt-button" onClick={() => setEditing(false)}>
          {t('common.cancel')}
        </button>
        {error && (
          <p role="alert" className="tt-error tt-small">
            {error}
          </p>
        )}
      </form>
    )
  }
  return (
    <>
      <h2 className="tt-chat-title">{conversationTitle(conversation)}</h2>
      {error && (
        <p role="alert" className="tt-error tt-small">
          {error}
        </p>
      )}
      <button
        type="button"
        className="tt-icon-button"
        aria-label={t('conversations.rename')}
        title={t('conversations.rename')}
        onClick={() => {
          setTitle(conversation.title)
          setError(null)
          setEditing(true)
        }}
      >
        <Icon name="edit" />
      </button>
      <button
        type="button"
        className="tt-icon-button"
        aria-label={archived ? t('conversations.unarchive') : t('conversations.archive')}
        title={archived ? t('conversations.unarchive') : t('conversations.archive')}
        aria-pressed={archived}
        disabled={busy}
        onClick={() => patch({ archived: !archived })}
      >
        <Icon name="archive" />
      </button>
    </>
  )
}

function Message({ message, workspace, apps }: { message: ChatMessage; workspace: number; apps: Set<ToolUse> }) {
  const t = useT()
  return (
    <article className="tt-message" data-role={message.role} aria-busy={message.pending || undefined}>
      <span className="tt-sr-only">{message.role === 'user' ? t('conversations.you') : t('conversations.agent')}</span>
      {/* What the user typed shows as typed; the agent's replies are Markdown. */}
      {message.text &&
        (message.role === 'assistant' ? (
          <Markdown text={message.text} headingShift={2} lineBreaks />
        ) : (
          <div className="tt-message-text">{message.text}</div>
        ))}
      {message.tools.map((tool) => (
        <div key={tool.id} className="tt-tool-use">
          <p className="tt-tool">
            {toolLabel(t, tool)}
            {tool.app && !apps.has(tool) && (
              <>
                {' · '}
                <Link href={appPath(workspace, tool.app.space, tool.app.app)}>{t('conversations.openApp')}</Link>
              </>
            )}
          </p>
          {/* A CLI's work is plain text lines, shown as such: never Markdown, never HTML. */}
          {tool.output && <pre className="tt-cli-output">{tool.output}</pre>}
          {tool.app && apps.has(tool) && (
            <Suspense fallback={<p className="tt-muted tt-small">{t('app.loading')}</p>}>
              <AppPreview workspace={workspace} space={tool.app.space} app={tool.app.app} />
            </Suspense>
          )}
        </div>
      ))}
      {message.pending && !message.text && <span className="tt-muted">{'…'}</span>}
      {message.error && <p className="tt-error">{message.error}</p>}
    </article>
  )
}

type ModelRef = { provider: string; model_id: string }
type ModelProvider = { provider: string; name: string; scope: string; models: { id: string; name: string }[] }

const modelKey = (m: ModelRef) => JSON.stringify([m.provider, m.model_id])

// What a tool call shows as: a CLI's turn as that CLI's work, any other as the tool it used.
function toolLabel(t: T, tool: ToolUse): string {
  if (tool.name === HELP_TOOL) return t('conversations.tool.help')
  if (tool.name !== CLI_TOOL) return t('conversations.tool.used', { name: tool.name })
  if (!tool.cli) return tool.running ? t('conversations.tool.someCliWorking') : t('conversations.tool.someCliWorked')
  const cli = CLI_PRODUCTS[tool.cli]
  return tool.running ? t('conversations.tool.cliWorking', { cli }) : t('conversations.tool.cliWorked', { cli })
}

// The caller's account for the conversation's CLI when it needs them to sign in again; null
// for a conversation on any other model, or a CLI account that is fine.
export function reconnectAccount(
  model: ModelRef | null | undefined,
  cli: CliAccounts | null | undefined,
): CliAccount | null {
  const name = cliOfProvider(model?.provider)
  if (!name || !cli?.enabled) return null
  const account = cliAccountFor(cli.accounts, name)
  return account?.state === 'reconnect' ? account : null
}

// The CLI's questions waiting for the owner's answer (docs/agent-safety.md): what it wants to
// run, edit or fetch, as plain text, with Allow and Deny. The CLI waits meanwhile.
export const QUESTIONS_POLL_MS = 10_000

function CliQuestions({
  url,
  cli,
  trigger,
  running,
}: {
  url: string
  cli: CliName
  trigger: string
  running: boolean
}) {
  const t = useT()
  const questions = useLoad(() => api<{ requests: CliQuestion[] }>(`${url}/cli-permissions`), [url])
  const reload = questions.reload
  // biome-ignore lint/correctness/useExhaustiveDependencies: read again when the trigger changes
  useEffect(() => reload(), [trigger])
  // And now and then while the CLI works, in case a question's line didn't come through (a
  // restart in between): reload is stable, so this is one timer per running turn.
  useEffect(() => {
    if (!running) return
    const timer = window.setInterval(reload, QUESTIONS_POLL_MS)
    return () => window.clearInterval(timer)
  }, [running, reload])
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const answer = async (q: CliQuestion, allow: boolean) => {
    setBusy(q.id)
    setError(null)
    try {
      await api(`${url}/cli-permissions/${encodeURIComponent(q.id)}`, { method: 'POST', body: { allow } })
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(null)
      reload()
    }
  }
  const list = questions.data?.requests ?? []
  if (list.length === 0 && !error) return null
  return (
    <>
      {error && (
        <p role="alert" className="tt-error tt-cli-banner">
          {error}
        </p>
      )}
      {list.map((q) => (
        <section
          key={q.id}
          className="tt-conflict tt-cli-banner"
          aria-label={t('conversations.question.label', { cli: CLI_PRODUCTS[cli], tool: q.tool })}
        >
          <p>
            {t.rich('conversations.question.asks', {
              cli: CLI_PRODUCTS[cli],
              tool: (text) => <strong>{text}</strong>,
            })}
          </p>
          {/* What the CLI wants to do, as plain text: never Markdown, never HTML. */}
          {q.input && <pre className="tt-cli-output">{q.input}</pre>}
          <div className="tt-actions">
            <button
              type="button"
              className="tt-button tt-primary"
              disabled={busy === q.id}
              onClick={() => answer(q, true)}
            >
              {t('conversations.question.allow')}
            </button>
            <button type="button" className="tt-button" disabled={busy === q.id} onClick={() => answer(q, false)}>
              {t('conversations.question.deny')}
            </button>
          </div>
        </section>
      ))}
    </>
  )
}

// Whether the CLI asks before it acts in this conversation. Turning the questions off is the
// owner's choice for this conversation alone, and says what it means first.
function CliPermissionsSwitch({
  conversation,
  url,
  cli,
  onChanged,
}: {
  conversation: Conversation
  url: string
  cli: CliName
  onChanged: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const allowAll = conversation.cli_permissions === 'allow_all'
  const set = async (on: boolean) => {
    if (on && !window.confirm(t('conversations.allowAll.confirm', { cli: CLI_PRODUCTS[cli] }))) {
      return
    }
    setBusy(true)
    setError(null)
    try {
      await api(url, { method: 'PATCH', body: { cli_permissions: on ? 'allow_all' : 'ask' } })
      onChanged()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="tt-cli-banner tt-small">
      <label>
        <input type="checkbox" checked={allowAll} disabled={busy} onChange={(e) => set(e.target.checked)} />{' '}
        {t('conversations.allowAll.label', { cli: CLI_PRODUCTS[cli] })}
      </label>
      {allowAll && <p>{t('conversations.allowAll.on')}</p>}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
    </div>
  )
}

function ReconnectBanner({ account, onConnected }: { account: CliAccount; onConnected: () => void }) {
  const t = useT()
  return (
    <section
      className="tt-conflict tt-cli-banner"
      aria-label={t('conversations.reconnect.label', { name: account.name })}
    >
      <p>{t('conversations.reconnect.text', { account: cliAccountTitle(account) })}</p>
      <CliLoginFlow
        account={account.id}
        name={account.name}
        label={t('conversations.reconnect.button')}
        onConnected={onConnected}
      />
    </section>
  )
}

// The conversation's model, chosen among what the caller may use. Hidden when either can't be
// read (the agent runtime is off, say): the conversation still works on the model it has.
function ModelPicker({
  workspace,
  url,
  providers,
  current,
  cli,
  onChosen,
  disabled,
}: {
  workspace: number
  url: string
  providers: ModelProvider[]
  current: ModelRef
  cli: CliAccounts | null | undefined
  onChosen: (m: ModelRef) => void
  disabled: boolean
}) {
  const t = useT()
  const [chosen, setChosen] = useState<ModelRef | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const selected = chosen ?? current
  const known = providers.some(
    (p) => p.provider === selected.provider && p.models.some((m) => m.id === selected.model_id),
  )
  const account = (provider: string) => {
    const name = cliOfProvider(provider)
    return name && cli ? cliAccountFor(cli.accounts, name) : undefined
  }
  const cliProviders = providers.filter((p) => p.scope === 'cli')
  const groups: (ModelProvider | 'cli')[] = []
  for (const p of providers) {
    if (p.scope !== 'cli') groups.push(p)
    else if (!groups.includes('cli')) groups.push('cli')
  }

  const choose = async (key: string) => {
    const [provider, model_id] = JSON.parse(key) as [string, string]
    const model = { provider, model_id }
    setChosen(model)
    setBusy(true)
    setError(null)
    try {
      await api(url, { method: 'PUT', body: model })
      onChosen(model)
    } catch (e) {
      setChosen(null)
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  const option = (p: ModelProvider, m: { id: string; name: string }, label: string) => (
    <option key={`${p.provider} ${m.id}`} value={modelKey({ provider: p.provider, model_id: m.id })}>
      {label}
    </option>
  )
  const selectedCli = cliOfProvider(selected.provider)
  const selectedAccount = account(selected.provider)
  const settings = (text: ReactNode) => (
    <Link href={routePath({ workspace, section: 'settings', item: 'cli-accounts' })}>{text}</Link>
  )

  return (
    <div className="tt-model">
      <label htmlFor="tt-model">{t('conversations.model.label')}</label>
      <select
        id="tt-model"
        value={modelKey(selected)}
        onChange={(e) => choose(e.target.value)}
        disabled={disabled || busy}
      >
        {!known && (
          <option value={modelKey(selected)}>
            {t('conversations.model.unavailable', { model: selected.model_id, provider: selected.provider })}
          </option>
        )}
        {groups.map((g) =>
          g === 'cli' ? (
            // The CLI workers are one group, whatever their place in the list: they run on the
            // user's own subscription, not on a provider account.
            <optgroup key="cli" label={t('conversations.model.cliGroup')}>
              {cliProviders.flatMap((p) => {
                const a = account(p.provider)
                const label =
                  cli && a?.state !== 'connected'
                    ? 'conversations.model.cliOptionNotConnected'
                    : 'conversations.model.cliOption'
                return p.models.map((m) => option(p, m, t(label, { provider: p.name, model: m.name })))
              })}
            </optgroup>
          ) : (
            <optgroup key={g.provider} label={g.name}>
              {g.models.map((m) => option(g, m, m.name))}
            </optgroup>
          ),
        )}
      </select>
      {error && (
        <span role="alert" className="tt-error">
          {error}
        </span>
      )}
      {selectedCli && cli && !cli.enabled && cli.reason && (
        <span className="tt-hint">{codeText(cli.reason_code, undefined, cli.reason)}</span>
      )}
      {selectedCli && cli?.enabled && selectedAccount?.state !== 'connected' && (
        <span className="tt-hint">
          {t.rich(
            selectedAccount?.state === 'reconnect'
              ? 'conversations.model.signInAgain'
              : 'conversations.model.notConnected',
            { cli: CLI_PRODUCTS[selectedCli], link: settings },
          )}
        </span>
      )}
    </div>
  )
}

function requestId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

type WhenBusy = 'steer' | 'follow_up'

function Composer({
  workspace,
  base,
  archived,
  running,
  onSent,
  children,
}: {
  workspace: number
  base: string
  archived: boolean
  running: boolean
  onSent: () => void
  // Below the message: the model picker.
  children?: ReactNode
}) {
  const t = useT()
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // One ID per message, so a retry after a lost answer doesn't send it twice.
  const pendingId = useRef<string | null>(null)
  const box = useRef<HTMLTextAreaElement>(null)

  // Dictated text goes where the cursor is, with a space on either side as needed, to edit
  // before sending.
  const insert = (spoken: string) => {
    const el = box.current
    setText((current) => {
      const start = el ? Math.min(el.selectionStart, current.length) : current.length
      const end = el ? Math.min(el.selectionEnd, current.length) : current.length
      const before = current.slice(0, start)
      const after = current.slice(end)
      const pad = (s: string, re: RegExp) => (s && !re.test(s) ? ' ' : '')
      return before + pad(before, /\s$/) + spoken + pad(after, /^\s/) + after
    })
    pendingId.current = null
    el?.focus()
  }
  const dictation = useDictation({ workspace, disabled: archived, onText: insert })

  // While the agent works, a message steers it (joins the running work at its next step), or
  // waits for the work to end (follow_up). Otherwise it starts new work.
  const send = async (whenBusy?: WhenBusy) => {
    const content = text.trim()
    if (!content || busy) return
    const draft = text
    pendingId.current ??= requestId()
    setBusy(true)
    setError(null)
    try {
      // The agent replies in the user's language; the server weighs the browser's against
      // the user's own pick and the workspace's default (docs/i18n.md).
      const body = {
        content,
        request_id: pendingId.current,
        browser_locale: browserLocale(),
        ...(whenBusy ? { when_busy: whenBusy } : {}),
      }
      await api(`${base}/messages`, { method: 'POST', body })
      pendingId.current = null
      // Only what was sent: text typed while it was sending stays.
      setText((current) => (current === draft ? '' : current))
      onSent()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  const submit = (e: FormEvent) => {
    e.preventDefault()
    send(running ? 'steer' : undefined)
  }
  const keyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      send(running ? 'steer' : undefined)
    }
  }
  const stop = async () => {
    setStopping(true)
    setError(null)
    try {
      await api(`${base}/stop`, { method: 'POST', body: {} })
    } catch (err) {
      setError(errorText(err))
    } finally {
      setStopping(false)
    }
  }
  const empty = !text.trim()

  return (
    <form className="tt-composer" onSubmit={submit}>
      {archived && <p className="tt-muted tt-small">{t('conversations.composer.archived')}</p>}
      {running && !archived && <p className="tt-muted tt-small">{t('conversations.composer.workingHint')}</p>}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {dictation.message}
      <div className="tt-composer-row">
        <label className="tt-sr-only" htmlFor="tt-message">
          {t('conversations.composer.message')}
        </label>
        <textarea
          id="tt-message"
          ref={box}
          rows={2}
          value={text}
          placeholder={running ? t('conversations.composer.steerPlaceholder') : t('conversations.composer.placeholder')}
          onChange={(e) => {
            setText(e.target.value)
            pendingId.current = null
          }}
          onKeyDown={keyDown}
          disabled={archived}
        />
        {running && (
          <>
            {!archived && (
              <button
                type="button"
                className="tt-button"
                disabled={busy || empty}
                onClick={() => send('follow_up')}
                title={t('conversations.composer.queueTitle')}
              >
                {t('conversations.composer.queue')}
              </button>
            )}
            <button
              type="button"
              className="tt-button"
              onClick={stop}
              disabled={stopping}
              aria-label={t('conversations.composer.stop')}
              title={t('conversations.composer.stopTitle')}
            >
              <Icon name="stop" />
            </button>
          </>
        )}
        {dictation.controls}
        <button
          type="submit"
          className="tt-button tt-primary"
          disabled={archived || busy || empty}
          aria-label={running ? t('conversations.composer.steer') : t('conversations.composer.send')}
          title={running ? t('conversations.composer.steerTitle') : t('conversations.composer.send')}
        >
          <Icon name="send" />
        </button>
      </div>
      {children}
    </form>
  )
}
