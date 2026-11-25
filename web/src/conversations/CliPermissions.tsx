// A CLI's questions to the conversation's owner (docs/agent-safety.md), and the owner's choice to
// let it act without asking in this conversation.

import { useEffect, useState } from 'react'
import { api, CLI_PRODUCTS, type CliName, type CliQuestion, type Conversation, errorText } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Notice } from '../ui/Notice'
import { Switch } from '../ui/Switch'

// The CLI's questions waiting for the owner's answer (docs/agent-safety.md): what it wants to
// run, edit or fetch, as plain text, with Allow and Deny. The CLI waits meanwhile.
export const QUESTIONS_POLL_MS = 10_000

export function CliQuestions({
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
      {error && <Notice tone="danger">{error}</Notice>}
      {list.map((q) => (
        <section
          key={q.id}
          className="ui-chat-question"
          aria-label={t('conversations.question.label', { cli: CLI_PRODUCTS[cli], tool: q.tool })}
        >
          <Notice
            tone="warning"
            title={t.rich('conversations.question.asks', {
              cli: CLI_PRODUCTS[cli],
              tool: q.tool,
              b: (text) => <strong key="tool">{text}</strong>,
            })}
            action={
              <span className="ui-chat-question-actions">
                <Button size="sm" loading={busy === q.id} onClick={() => answer(q, true)}>
                  {t('conversations.question.allow')}
                </Button>
                <Button size="sm" variant="ghost" disabled={busy === q.id} onClick={() => answer(q, false)}>
                  {t('conversations.question.deny')}
                </Button>
              </span>
            }
          >
            {/* What the CLI wants to do, as plain text: never Markdown, never HTML. */}
            {q.input && <pre className="ui-chat-output">{q.input}</pre>}
          </Notice>
        </section>
      ))}
    </>
  )
}

// Whether the CLI asks before it acts in this conversation. Turning the questions off is the
// owner's choice for this conversation alone, and says what it means first.
export function CliPermissionsSwitch({
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
  const [confirmDialog, confirm] = useConfirm()
  const allowAll = conversation.cli_permissions === 'allow_all'
  const set = async (on: boolean) => {
    // Turning the questions off says what it means first; turning them back on needs no question.
    if (
      on &&
      !(await confirm({
        title: t('conversations.allowAll.title', { cli: CLI_PRODUCTS[cli] }),
        body: t('conversations.allowAll.confirm', { cli: CLI_PRODUCTS[cli] }),
        confirmLabel: t('conversations.allowAll.confirmButton'),
        tone: 'danger',
      }))
    ) {
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
    <div className="ui-chat-permissions">
      <Switch
        label={t('conversations.allowAll.label', { cli: CLI_PRODUCTS[cli] })}
        checked={allowAll}
        disabled={busy}
        onChange={(e) => set(e.target.checked)}
      />
      {allowAll && <Notice tone="warning">{t('conversations.allowAll.on')}</Notice>}
      {error && <Notice tone="danger">{error}</Notice>}
      {confirmDialog}
    </div>
  )
}
