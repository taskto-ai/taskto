import { type FormEvent, useState } from 'react'
import { api, type Card, type Conversation, ws } from '../api'
import { conversationTitle } from '../conversations/title'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { Select } from '../ui/Select'
import { useDraft } from './drafts'

type LinkChange = (fields: (base: Card) => Partial<Pick<Card, 'docs' | 'conversations'>>) => Promise<boolean>

export function Documents({
  card,
  canWrite,
  busy,
  onChange,
}: {
  card: Card
  canWrite: boolean
  busy: boolean
  onChange: LinkChange
}) {
  const [path, setPath] = useState('')
  useDraft(path.trim() !== '')
  const t = useT()
  const linkDoc = async (e: FormEvent) => {
    e.preventDefault()
    const p = path.trim()
    if (await onChange((base) => ({ docs: [...base.docs, p] }))) setPath('')
  }
  return (
    <div className="ui-board-links">
      {card.docs.length === 0 && <span className="ui-board-muted">{t('boards.noDocuments')}</span>}
      {card.docs.length > 0 && (
        <ul className="ui-board-link-list">
          {card.docs.map((p) => (
            <li key={p} className="ui-board-link">
              <code className="ui-board-path">{p}</code>
              {canWrite && (
                <IconButton
                  icon="close"
                  size="sm"
                  label={t('boards.unlinkNamed', { name: p })}
                  disabled={busy}
                  onClick={() => onChange((base) => ({ docs: base.docs.filter((x) => x !== p) }))}
                />
              )}
            </li>
          ))}
        </ul>
      )}
      {canWrite && (
        <form className="ui-board-inline" aria-label={t('boards.linkDocument')} onSubmit={linkDoc}>
          <Input
            size="sm"
            className="ui-board-grow"
            aria-label={t('boards.documentPath')}
            placeholder={t('boards.documentPlaceholder')}
            value={path}
            onChange={(e) => setPath(e.target.value)}
          />
          <Button type="submit" size="sm" disabled={busy || !path.trim()}>
            {t('boards.link')}
          </Button>
        </form>
      )}
    </div>
  )
}

export function Conversations({
  workspace,
  card,
  canWrite,
  busy,
  onChange,
}: {
  workspace: number
  card: Card
  canWrite: boolean
  busy: boolean
  onChange: LinkChange
}) {
  const needed = canWrite || card.conversations.length > 0
  const mine = useLoad(
    () =>
      needed
        ? Promise.all(
            ['', '?archived=true'].map((q) =>
              api<{ conversations: Conversation[] }>(`${ws(workspace)}/conversations${q}`).then((r) => r.conversations),
            ),
          ).then((lists) => lists.flat())
        : Promise.resolve([]),
    [workspace, needed],
  )
  const [chosen, setChosen] = useState('')
  const t = useT()
  const own = (id: string) => mine.data?.find((c) => String(c.id) === id)
  const linkable = (mine.data ?? []).filter((c) => !card.conversations.includes(String(c.id)))

  const linkConversation = async (e: FormEvent) => {
    e.preventDefault()
    if (await onChange((base) => ({ conversations: [...base.conversations, chosen] }))) setChosen('')
  }

  return (
    <div className="ui-board-links">
      {card.conversations.length === 0 && <span className="ui-board-muted">{t('boards.noConversations')}</span>}
      {card.conversations.length > 0 && (
        <ul className="ui-board-link-list">
          {card.conversations.map((id) => {
            const c = own(id)
            const title = c ? conversationTitle(c) : t('boards.conversation')
            return (
              <li key={id} className="ui-board-link">
                {c ? (
                  <Link href={routePath({ workspace, section: 'conversations', item: id })} className="ui-board-anchor">
                    {title}
                  </Link>
                ) : (
                  <span className="ui-board-muted">
                    {mine.data ? t('boards.othersConversation') : t('boards.conversationLoading')}
                  </span>
                )}
                {canWrite && (
                  <IconButton
                    icon="close"
                    size="sm"
                    label={c ? t('boards.unlinkNamed', { name: title }) : t('boards.unlinkConversation')}
                    disabled={busy}
                    onClick={() => onChange((base) => ({ conversations: base.conversations.filter((x) => x !== id) }))}
                  />
                )}
              </li>
            )
          })}
        </ul>
      )}
      {canWrite && linkable.length > 0 && (
        <form className="ui-board-inline" aria-label={t('boards.linkConversation')} onSubmit={linkConversation}>
          <Select
            size="sm"
            className="ui-board-grow"
            aria-label={t('boards.conversation')}
            value={chosen}
            onChange={(e) => setChosen(e.target.value)}
          >
            <option value="">{t('boards.chooseConversation')}</option>
            {linkable.map((c) => (
              <option key={c.id} value={String(c.id)}>
                {conversationTitle(c)}
              </option>
            ))}
          </Select>
          <Button type="submit" size="sm" disabled={busy || !chosen}>
            {t('boards.link')}
          </Button>
        </form>
      )}
    </div>
  )
}
