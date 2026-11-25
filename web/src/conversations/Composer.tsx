// The composer: the message (kept as a draft in this browser), dictation into it, send, and while
// the agent works, steer, queue a follow-up or stop. The model picker goes below it.

import { type FormEvent, type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from 'react'
import { api, errorText } from '../api'
import { browserLocale, useT } from '../i18n'
import { Icon } from '../shell/Icon'
import { draftKey, readDraft, writeDraft } from '../uiState'
import { useDictation } from './Dictation'

function requestId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

type WhenBusy = 'steer' | 'follow_up'

export function Composer({
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
  // An unsent message is kept in this browser, so leaving and coming back finds it where it was.
  // The key names the signed-in user, so no other account's tab reads or writes it.
  const messageKey = draftKey('message', base)
  const [text, setText] = useState(() => readDraft(messageKey) ?? '')
  // biome-ignore lint/correctness/useExhaustiveDependencies: written when the text changes
  useEffect(() => writeDraft(messageKey, text.trim() ? text : null), [text])
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
