import { type FormEvent, useRef, useState } from 'react'
import { api, type Comment, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Markdown } from '../markdown'
import { mentionNames, mentionOf } from './comments'
import type { Person } from './people'
import { authorLabel } from './text'

// The card's thread: its comments, oldest first, and a writer's box for a new one, with
// people to mention.
export function Thread({
  url,
  comments,
  canWrite,
  people,
  onAdded,
}: {
  url: string
  comments: Comment[]
  canWrite: boolean
  people: Person[] | undefined
  onAdded: (c: Comment) => void
}) {
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const f = useFormat()
  const field = useRef<HTMLTextAreaElement>(null)
  // Puts the person's mention where the cursor is, and the cursor after it.
  const mention = (id: string) => {
    const who = people?.find((p) => String(p.id) === id)
    if (!who) return
    const at = field.current
    const start = at?.selectionStart ?? body.length
    const end = at?.selectionEnd ?? start
    const before = body.slice(0, start)
    const token = `${before && !/\s$/.test(before) ? ' ' : ''}${mentionOf(who)} `
    setBody(before + token + body.slice(end))
    const caret = before.length + token.length
    requestAnimationFrame(() => {
      at?.focus()
      at?.setSelectionRange(caret, caret)
    })
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onAdded(await api<Comment>(url, { method: 'POST', body: { body } }))
      setBody('')
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="tt-thread" aria-label={t('boards.comments')}>
      <h3>{t('boards.comments')}</h3>
      {comments.length === 0 && <p className="tt-muted">{t('boards.noComments')}</p>}
      <ol className="tt-list">
        {comments.map((c) => (
          <li key={c.id} className="tt-comment">
            <span className="tt-muted tt-small">
              {[authorLabel(t, c.author), f.dateTime(c.created_at)].join(' · ')}
            </span>
            <Markdown text={c.body} mentions={mentionNames(c, t('boards.mentionUnknown'))} />
          </li>
        ))}
      </ol>
      {canWrite && (
        <form className="tt-form" onSubmit={submit}>
          <textarea
            ref={field}
            aria-label={t('boards.commentField')}
            rows={3}
            value={body}
            onChange={(e) => setBody(e.target.value)}
          />
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <div className="tt-actions">
            {people && people.length > 0 && (
              <select
                aria-label={t('boards.mention')}
                value=""
                onChange={(e) => mention(e.target.value)}
                disabled={busy}
              >
                <option value="">{t('boards.mentionPlaceholder')}</option>
                {people.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            )}
            <button type="submit" className="tt-button tt-primary" disabled={busy || !body.trim()}>
              {t('boards.commentSubmit')}
            </button>
          </div>
        </form>
      )}
    </section>
  )
}
