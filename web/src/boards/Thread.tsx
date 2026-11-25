import { type FormEvent, useRef, useState } from 'react'
import { api, type Comment, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Markdown } from '../markdown'
import { AgentDot } from '../ui/AgentDot'
import { Avatar } from '../ui/Avatar'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Menu, MenuButton, MenuItem, MenuList } from '../ui/Menu'
import { Textarea } from '../ui/Textarea'
import { mentionNames, mentionOf } from './comments'
import { useDraft } from './drafts'
import type { Person } from './people'
import { authorLabel } from './text'

// The card's thread: its comments, oldest first, each with who wrote it (the agent's marked by
// its dot) and when.
export function Thread({ comments }: { comments: Comment[] }) {
  const t = useT()
  const f = useFormat()
  return (
    <section className="ui-board-section" aria-label={t('boards.comments')}>
      <Heading level={3} size="subsection">
        {t('boards.comments')}
      </Heading>
      {comments.length === 0 && <p className="ui-board-muted">{t('boards.noComments')}</p>}
      <ol className="ui-board-thread">
        {comments.map((c) => (
          <li key={c.id} className="ui-board-comment">
            <span className="ui-board-comment-who">
              {c.author?.agent ? <AgentDot /> : <Avatar size="sm" name={authorLabel(t, c.author)} decorative />}
            </span>
            <div className="ui-board-comment-body">
              <p className="ui-board-comment-head">
                <strong>{authorLabel(t, c.author)}</strong>
                {' · '}
                <time dateTime={c.created_at} title={f.dateTime(c.created_at)}>
                  {f.relative(c.created_at)}
                </time>
              </p>
              <Markdown text={c.body} mentions={mentionNames(c, t('boards.mentionUnknown'))} />
            </div>
          </li>
        ))}
      </ol>
    </section>
  )
}

// A writer's box for a new comment, at the foot of the open card, with people to mention.
export function Composer({
  url,
  people,
  onAdded,
}: {
  url: string
  people: Person[] | undefined
  onAdded: (c: Comment) => void
}) {
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const field = useRef<HTMLTextAreaElement>(null)
  useDraft(body.trim() !== '')
  // Puts the person's mention where the cursor is, and the cursor after it.
  const mention = (who: Person) => {
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
      // Sent, the button can't be pressed again (the box is empty): the focus goes back to the box
      // for the next comment, rather than to the page.
      field.current?.focus()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="ui-board-composer" aria-label={t('boards.newComment')} onSubmit={submit}>
      <Textarea
        ref={field}
        aria-label={t('boards.commentField')}
        rows={2}
        value={body}
        invalid={Boolean(error)}
        onChange={(e) => setBody(e.target.value)}
      />
      {error && (
        <p role="alert" className="ui-board-error">
          {error}
        </p>
      )}
      <div className="ui-board-actions">
        {people && people.length > 0 && (
          <Menu align="start">
            <MenuButton size="sm" icon="at" label={t('boards.mention')} disabled={busy} />
            <MenuList>
              {people.map((p) => (
                <MenuItem key={p.id} onSelect={() => mention(p)}>
                  <span className="ui-board-value">
                    <Avatar size="sm" name={p.name} decorative />
                    {p.name}
                  </span>
                </MenuItem>
              ))}
            </MenuList>
          </Menu>
        )}
        <Button type="submit" size="sm" loading={busy} disabled={!body.trim()} className="ui-board-send">
          {t('boards.commentSubmit')}
        </Button>
      </div>
    </form>
  )
}
