import { useCallback, useEffect, useEffectEvent, useRef, useState } from 'react'
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
import { useChanged } from '../memo'
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
  onChanged: () => void
  streams?: EventSourceFactory
  buildHref?: string
  embedApps?: boolean
  onSavedApp?: (app: AppRef, save: string) => void
  savedAfter?: number
}) {
  const t = useT()
  const base = `${ws(workspace)}/conversations/${id}`
  const [refused, setRefused] = useState(false)
  const reads = useRef(0)
  const meta = useLoad(() => {
    const read = ++reads.current
    return api<Conversation>(base).then(
      (c) => {
        if (read === reads.current) setRefused(false)
        return c
      },
      (e: unknown) => {
        if (read === reads.current) setRefused(e instanceof ApiError && REFUSED.has(e.status))
        throw e
      },
    )
  }, [base])
  const { view, status } = useConversationStream(`${base}/events`, streams)
  const unreadable = (refused && !meta.data) || status === 'gone'
  const reloadMeta = meta.reload
  const metaFailed = Boolean(meta.error)
  const retryMeta = useEffectEvent(() => {
    if (metaFailed) reloadMeta()
  })
  useEffect(() => {
    if (status === 'live') retryMeta()
  }, [status])
  const loadPage = useCallback((before: number) => api<History>(`${base}/entries?before=${before}`), [base])
  const older = useHistory(view, loadPage)
  const { messages, running, truncated } = transcript(
    view && older.history ? { ...view, entries: older.history.entries, before: older.history.before } : view,
  )
  const archived = Boolean(meta.data?.archived_at)
  const readOnly = Boolean(meta.data) && meta.data?.role !== 'owner'
  const apps = embedApps ? liveApps(messages) : new Set<ToolUse>()
  const saved = lastSavedApp(messages, savedAfter)
  const savedKey = saved?.save
  const tellSaved = useEffectEvent(() => {
    if (saved) onSavedApp?.(saved.app, saved.save)
  })
  useEffect(() => {
    if (savedKey !== undefined) tellSaved()
  }, [savedKey])

  const providers = useLoad(
    () => api<{ providers: ModelProvider[] }>(`${ws(workspace)}/models`).then((r) => r.providers),
    [workspace],
  )
  const loadedModel = useLoad(() => api<ModelRef>(`${base}/model`), [base])
  const [picked, setPicked] = useState<ModelRef | null>(null)
  const model = picked ?? loadedModel.data
  const usesCli = !readOnly && Boolean(cliOfProvider(model?.provider) || providers.data?.some((p) => p.scope === 'cli'))
  const cli = useLoad(() => (usesCli ? api<CliAccounts>(CLI_ACCOUNTS) : Promise.resolve(null)), [usesCli])
  const asks = messages.reduce((n, m) => n + m.tools.filter((t) => t.cli && t.output?.includes('⚠')).length, 0)
  const cliName = readOnly ? undefined : cliOfProvider(model?.provider)
  const cliTurnChanged = useChanged(`${asks}:${running}`)
  if (cliTurnChanged && cliName) cli.reload()
  const reconnect = reconnectAccount(model, cli.data)
  const questionLines = messages.reduce(
    (n, m) => n + m.tools.reduce((k, t) => k + (t.cli ? (t.output?.match(/^(\? | {2}[✓✗] )/gm)?.length ?? 0) : 0), 0),
    0,
  )

  const browser = useLoad(() => api<BrowserStatus>(BROWSER), [])
  const [browserOpen, setBrowserOpen] = useState(false)
  const [takeOver, setTakeOver] = useState(false)
  const help = readOnly ? null : pendingHelp(messages)
  const helpChanged = useChanged(JSON.stringify([help?.message ?? null, running]))
  if (helpChanged && help) browser.reload()
  const changed = () => {
    meta.reload()
    onChanged()
  }

  const last = messages.at(-1)

  return (
    <section className="ui-chat" aria-label={t('conversations.conversation')}>
      <ConversationHead
        workspace={workspace}
        backHref={backHref}
        conversation={meta.data}
        failed={Boolean(meta.error) && !meta.data}
        url={base}
        onChanged={changed}
        buildHref={buildHref}
        browserEnabled={Boolean(browser.data?.enabled) && !readOnly}
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
        {readOnly && meta.data && <Notice>{t('conversations.sharedReadOnly', { owner: meta.data.owner.name })}</Notice>}
        {reconnect && !readOnly && <ReconnectBanner account={reconnect} onConnected={cli.reload} />}
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
        {browserOpen && !readOnly && (
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
      {!readOnly && (
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
      )}
    </section>
  )
}
