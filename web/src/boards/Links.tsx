import { type FormEvent, useState } from 'react'
import { api, type Card, type Conversation, ws } from '../api'
import { conversationTitle } from '../conversations/title'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'

// What the card links to: documents of its space, by path, and conversations. A conversation
// is private to its owner: only they can link it or open it, and the others see only that the
// card links one. Writers may unlink any.
export function Links({
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
  onChange: (fields: (base: Card) => Partial<Pick<Card, 'docs' | 'conversations'>>) => Promise<boolean>
}) {
  const needed = canWrite || card.conversations.length > 0
  // The caller's own conversations, archived ones too: the ones they may open or link.
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
  const [path, setPath] = useState('')
  const [chosen, setChosen] = useState('')
  const t = useT()
  const own = (id: string) => mine.data?.find((c) => String(c.id) === id)
  const linkable = (mine.data ?? []).filter((c) => !card.conversations.includes(String(c.id)))

  const linkDoc = async (e: FormEvent) => {
    e.preventDefault()
    const p = path.trim()
    if (await onChange((base) => ({ docs: [...base.docs, p] }))) setPath('')
  }
  const linkConversation = async (e: FormEvent) => {
    e.preventDefault()
    if (await onChange((base) => ({ conversations: [...base.conversations, chosen] }))) setChosen('')
  }

  return (
    <section className="tt-links" aria-label={t('boards.links')}>
      <h3>{t('boards.documents')}</h3>
      {card.docs.length === 0 && <p className="tt-muted">{t('boards.noDocuments')}</p>}
      {card.docs.length > 0 && (
        <ul className="tt-link-list">
          {card.docs.map((p) => (
            <li key={p}>
              <code>{p}</code>
              {canWrite && (
                <button
                  type="button"
                  className="tt-button"
                  aria-label={t('boards.unlinkNamed', { name: p })}
                  disabled={busy}
                  onClick={() => onChange((base) => ({ docs: base.docs.filter((x) => x !== p) }))}
                >
                  {t('boards.unlink')}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {canWrite && (
        <form className="tt-search" aria-label={t('boards.linkDocument')} onSubmit={linkDoc}>
          <input
            aria-label={t('boards.documentPath')}
            placeholder={t('boards.documentPlaceholder')}
            value={path}
            onChange={(e) => setPath(e.target.value)}
          />
          <button type="submit" className="tt-button" disabled={busy || !path.trim()}>
            {t('boards.link')}
          </button>
        </form>
      )}

      <h3>{t('boards.conversations')}</h3>
      {card.conversations.length === 0 && <p className="tt-muted">{t('boards.noConversations')}</p>}
      {card.conversations.length > 0 && (
        <ul className="tt-link-list">
          {card.conversations.map((id) => {
            const c = own(id)
            const title = c ? conversationTitle(c) : t('boards.conversation')
            return (
              <li key={id}>
                {c ? (
                  <Link href={routePath({ workspace, section: 'conversations', item: id })}>{title}</Link>
                ) : (
                  <span className="tt-muted">
                    {mine.data ? t('boards.othersConversation') : t('boards.conversationLoading')}
                  </span>
                )}
                {canWrite && (
                  <button
                    type="button"
                    className="tt-button"
                    aria-label={c ? t('boards.unlinkNamed', { name: title }) : t('boards.unlinkConversation')}
                    disabled={busy}
                    onClick={() => onChange((base) => ({ conversations: base.conversations.filter((x) => x !== id) }))}
                  >
                    {t('boards.unlink')}
                  </button>
                )}
              </li>
            )
          })}
        </ul>
      )}
      {canWrite && linkable.length > 0 && (
        <form className="tt-search" aria-label={t('boards.linkConversation')} onSubmit={linkConversation}>
          <select aria-label={t('boards.conversation')} value={chosen} onChange={(e) => setChosen(e.target.value)}>
            <option value="">{t('boards.chooseConversation')}</option>
            {linkable.map((c) => (
              <option key={c.id} value={String(c.id)}>
                {conversationTitle(c)}
              </option>
            ))}
          </select>
          <button type="submit" className="tt-button" disabled={busy || !chosen}>
            {t('boards.link')}
          </button>
        </form>
      )}
    </section>
  )
}
