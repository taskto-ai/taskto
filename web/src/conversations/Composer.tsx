import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useEffectEvent,
  useId,
  useRef,
  useState,
} from 'react'
import { api, errorText } from '../api'
import { browserLocale, useT } from '../i18n'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Textarea } from '../ui/Textarea'
import { Tooltip } from '../ui/Tooltip'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { draftKey, readDraft, writeDraft } from '../uiState'
import { useDictation } from './Dictation'

function requestId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

type WhenBusy = 'steer' | 'follow_up'

type Mode = 'message' | 'note'

export function Composer({
  workspace,
  base,
  archived,
  unreadable = false,
  running,
  onSent,
  children,
}: {
  workspace: number
  base: string
  archived: boolean
  unreadable?: boolean
  running: boolean
  onSent: () => void
  children?: ReactNode
}) {
  const t = useT()
  const id = useId()
  const off = archived || unreadable
  const messageKey = draftKey('message', base)
  const [text, setText] = useState(() => readDraft(messageKey) ?? '')
  const keepText = useEffectEvent((text: string) => writeDraft(messageKey, text.trim() ? text : null))
  useEffect(() => keepText(text), [text])
  const [mode, setMode] = useState<Mode>('message')
  const noting = mode === 'note'
  const [busy, setBusy] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const pendingId = useRef<string | null>(null)
  const box = useRef<HTMLTextAreaElement>(null)

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
  const dictation = useDictation({ workspace, disabled: off, onText: insert })

  const send = async (whenBusy?: WhenBusy) => {
    const content = text.trim()
    if (!content || busy) return
    const draft = text
    pendingId.current ??= requestId()
    setBusy(true)
    setError(null)
    try {
      if (noting) {
        await api(`${base}/notes`, { method: 'POST', body: { content, request_id: pendingId.current } })
      } else {
        const body = {
          content,
          request_id: pendingId.current,
          browser_locale: browserLocale(),
          ...(whenBusy ? { when_busy: whenBusy } : {}),
        }
        await api(`${base}/messages`, { method: 'POST', body })
      }
      pendingId.current = null
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
    send(running && !noting ? 'steer' : undefined)
  }
  const keyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      send(running && !noting ? 'steer' : undefined)
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
  const steering = running && !noting
  const hint = unreadable
    ? t('conversations.composer.unreadable')
    : archived
      ? t('conversations.composer.archived')
      : noting
        ? t('conversations.note.hint')
        : running
          ? t('conversations.composer.workingHint')
          : null
  const sendLabel = noting
    ? t('conversations.note.send')
    : running
      ? t('conversations.composer.steer')
      : t('conversations.composer.send')

  return (
    <form className="ui-chat-composer" onSubmit={submit}>
      <div className="ui-chat-composer-column">
        {hint && (
          <p className="ui-chat-composer-hint" id={`${id}-hint`}>
            {hint}
          </p>
        )}
        {error && <Notice tone="danger">{error}</Notice>}
        {dictation.message}
        <div className="ui-chat-box" data-disabled={off || undefined} data-mode={mode}>
          <VisuallyHidden>
            <label htmlFor={id}>{t('conversations.composer.message')}</label>
          </VisuallyHidden>
          <Textarea
            id={id}
            ref={box}
            rows={2}
            className="ui-chat-input"
            value={text}
            placeholder={
              noting
                ? t('conversations.note.placeholder')
                : running
                  ? t('conversations.composer.steerPlaceholder')
                  : t('conversations.composer.placeholder')
            }
            onChange={(e) => {
              setText(e.target.value)
              pendingId.current = null
            }}
            onKeyDown={keyDown}
            disabled={off}
            aria-describedby={hint ? `${id}-hint` : undefined}
          />
          <div className="ui-chat-box-row">
            {!off && (
              <SegmentedControl
                size="sm"
                label={t('conversations.note.toggle')}
                value={mode}
                onChange={(next) => {
                  setMode(next)
                  pendingId.current = null
                }}
                options={[
                  { value: 'message', label: t('conversations.note.modeMessage') },
                  { value: 'note', label: t('conversations.note.modeNote') },
                ]}
              />
            )}
            {children}
            <span className="ui-chat-box-spacer" />
            {steering && !off && (
              <Tooltip content={t('conversations.composer.queueTitle')}>
                <Button size="sm" variant="ghost" disabled={busy || empty} onClick={() => send('follow_up')}>
                  {t('conversations.composer.queue')}
                </Button>
              </Tooltip>
            )}
            {running && (
              <IconButton icon="stop" label={t('conversations.composer.stop')} onClick={stop} disabled={stopping} />
            )}
            {dictation.controls}
            <Tooltip content={steering ? t('conversations.composer.steerTitle') : sendLabel} describes={steering}>
              <Button
                type="submit"
                variant="primary"
                icon="send"
                className="ui-chat-send"
                disabled={off || busy || empty}
                aria-label={sendLabel}
              />
            </Tooltip>
          </div>
        </div>
      </div>
    </form>
  )
}
