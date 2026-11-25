// One conversation: its head, the transcript over the events stream with earlier messages on
// demand, the apps the agent saved in it, the model it runs on, and the composer, which steers or
// stops the agent while it works. Each is a part of its own; this view holds what they share.
//
// A conversation on a CLI worker shows the CLI's work as it streams, as plain text, and when
// the user's login to that CLI has expired, a banner to sign in again right here: the turn
// waits for it and goes on by itself.

import { useCallback, useEffect, useState } from 'react'
import {
  ApiError,
  api,
  BROWSER,
  type BrowserStatus,
  CLI_ACCOUNTS,
  type CliAccounts,
  type Conversation,
  cliOfProvider,
  ws,
} from '../api'
import { BrowserPanel } from '../browser/BrowserPanel'
import { HelpCard } from '../browser/HelpCard'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { ChatTranscript } from './ChatTranscript'
import { CliPermissionsSwitch, CliQuestions } from './CliPermissions'
import { Composer } from './Composer'
import { ConversationHead } from './ConversationHead'
import './Conversations.css'
import { type History, useHistory } from './history'
import { ModelPicker, type ModelProvider, type ModelRef } from './ModelPicker'
import { ReconnectBanner, reconnectAccount } from './ReconnectBanner'
import { lastSavedApp, liveApps } from './savedApps'
import { type AppRef, pendingHelp, type ToolUse, transcript } from './transcript'
import { type EventSourceFactory, useConversationStream } from './useConversationStream'

// The answers that say the caller can't read this conversation.
const REFUSED = new Set([403, 404, 410])

export function ConversationView({
  workspace,
  id,
  backHref,
  onChanged,
  streams,
  buildHref,
  embedApps = true,
  onSavedApp,
  savedAfter,
}: {
  workspace: number
  id: number
  backHref: string
  // The conversation was sent to, renamed or archived: the list is out of date.
  onChanged: () => void
  streams?: EventSourceFactory
  // Where to build an app in this conversation (the builder); no link without it.
  buildHref?: string
  // Whether the apps the agent saved show launched in the transcript. The builder shows the app in
  // its own pane instead, and the transcript only links to it.
  embedApps?: boolean
  // Called with the app the agent saved last, and the tool call that saved it (which changes with
  // each save, so the caller knows to read the app again).
  onSavedApp?: (app: AppRef, save: string) => void
  // Only an app saved in an entry after this one is told to onSavedApp.
  savedAfter?: number
}) {
  const t = useT()
  const base = `${ws(workspace)}/conversations/${id}`
  // Whether the server refused the conversation (403, 404 or 410) on its last answer: then it
  // can't be read. Any other failure (a server restarting, the network) is shown with a way to try
  // again, and the composer stays usable.
  const [refused, setRefused] = useState(false)
  const meta = useLoad(
    () =>
      api<Conversation>(base).then(
        (c) => {
          setRefused(false)
          return c
        },
        (e: unknown) => {
          setRefused(e instanceof ApiError && REFUSED.has(e.status))
          throw e
        },
      ),
    [base],
  )
  const { view, status } = useConversationStream(`${base}/events`, streams)
  // Nothing can be sent: the server refused it and nothing of it is shown, or the stream found it gone.
  const unreadable = (refused && !meta.data) || status === 'gone'
  // When the stream connects again after a failed read, the conversation is read again too.
  const reloadMeta = meta.reload
  const metaFailed = Boolean(meta.error)
  // biome-ignore lint/correctness/useExhaustiveDependencies: read again when the stream comes back
  useEffect(() => {
    if (status === 'live' && metaFailed) reloadMeta()
  }, [status])
  const loadPage = useCallback((before: number) => api<History>(`${base}/entries?before=${before}`), [base])
  const older = useHistory(view, loadPage)
  const { messages, running, truncated } = transcript(
    view && older.history ? { ...view, entries: older.history.entries, before: older.history.before } : view,
  )
  const archived = Boolean(meta.data?.archived_at)
  const apps = embedApps ? liveApps(messages) : new Set<ToolUse>()
  const saved = lastSavedApp(messages, savedAfter)
  // biome-ignore lint/correctness/useExhaustiveDependencies: told again for each new save only
  useEffect(() => {
    if (saved) onSavedApp?.(saved.app, saved.save)
  }, [saved?.save])

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

  const last = messages.at(-1)

  return (
    <section className="ui-chat" aria-label={t('conversations.conversation')}>
      <ConversationHead
        backHref={backHref}
        conversation={meta.data}
        failed={Boolean(meta.error) && !meta.data}
        url={base}
        onChanged={changed}
        buildHref={buildHref}
        browserEnabled={Boolean(browser.data?.enabled)}
        browserOpen={browserOpen}
        onToggleBrowser={() => {
          setBrowserOpen(!browserOpen)
          setTakeOver(false)
          browser.reload()
        }}
        status={status}
      />
      <div className="ui-chat-notices">
        {meta.error && (
          <Notice
            tone="danger"
            action={
              !refused && (
                <Button size="sm" onClick={meta.reload}>
                  {t('app.tryAgain')}
                </Button>
              )
            }
          >
            {meta.error}
          </Notice>
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
      </div>
      {/* Screen readers hear when the agent starts and when it has replied, not each word of a
          reply as it streams in. */}
      <VisuallyHidden>
        <span role="status">
          {running
            ? t('conversations.agentWorking')
            : last?.role === 'assistant' && !last.pending
              ? t('conversations.agentReplied')
              : ''}
        </span>
      </VisuallyHidden>
      <ChatTranscript
        workspace={workspace}
        messages={messages}
        started={Boolean(view)}
        running={running}
        truncated={truncated}
        apps={apps}
        onLoadEarlier={older.loadEarlier}
        loadingEarlier={older.loading}
        earlierError={older.error}
      />
      <Composer
        workspace={workspace}
        base={base}
        archived={archived}
        unreadable={unreadable}
        running={running}
        onSent={onChanged}
      >
        {providers.data && model && (
          <ModelPicker
            workspace={workspace}
            url={`${base}/model`}
            providers={providers.data}
            current={model}
            cli={cli.data}
            onChosen={setPicked}
            disabled={archived || unreadable}
          />
        )}
      </Composer>
    </section>
  )
}
