import type { Card, Space } from '../api'
import { useT } from '../i18n'
import { Icon } from '../shell/Icon'
import { Blocked } from './Blocked'
import { CardDescription } from './CardDescription'
import { CardMeta } from './CardMeta'
import { useCard } from './card'
import { Links } from './Links'
import { Planning } from './Planning'
import { usePeople } from './people'
import { Thread } from './Thread'

// The open card: its fields, its thread, and the changes a writer can make (useCard in card.ts
// says how a change is sent, and what a conflict does).
export function CardView({
  url,
  workspace,
  space,
  board,
  canWrite,
  moved,
  onChanged,
  onClose,
}: {
  url: string
  workspace: number
  space: Space
  board: Card[]
  canWrite: boolean
  moved: Card | null
  onChanged: () => void
  onClose: () => void
}) {
  const {
    card,
    loadError,
    busy,
    error,
    conflict,
    stopped,
    editing,
    setEditing,
    change,
    takeTheirs,
    addComment,
    patch,
    post,
  } = useCard(url, moved, onChanged)
  const people = usePeople(workspace, space, canWrite)
  const t = useT()
  // A change can't start while one is being sent, or while a conflict waits for the writer.
  const held = busy || conflict !== null

  return (
    <article className="tt-card-view" aria-label={card ? card.title : t('boards.card')}>
      <div className="tt-pane-head">
        <button type="button" className="tt-icon-button tt-back" aria-label={t('boards.back')} onClick={onClose}>
          <Icon name="back" />
        </button>
        <h2>{card?.title ?? t('boards.card')}</h2>
        <button type="button" className="tt-button" onClick={onClose}>
          {t('boards.close')}
        </button>
      </div>
      {loadError && !card && (
        <p role="alert" className="tt-error">
          {loadError}
        </p>
      )}
      {!card && !loadError && <p className="tt-muted">{t('app.loading')}</p>}
      {conflict && (
        <div role="alert" className="tt-conflict">
          <span>{t('boards.cardChanged')}</span>
          <button type="button" className="tt-button" onClick={() => takeTheirs(conflict.current)}>
            {t('boards.takeTheirs')}
          </button>
          <button
            type="button"
            className="tt-button tt-danger"
            disabled={busy}
            onClick={() => change(conflict.send, conflict.current)}
          >
            {t('boards.redoMine')}
          </button>
        </div>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {stopped && (
        <p role="alert" className="tt-error">
          {t('boards.pollStopped', { error: stopped })}
        </p>
      )}
      {card && (
        <>
          <CardMeta
            card={card}
            canWrite={canWrite}
            people={people.data}
            busy={held}
            onMove={(column) => change(post('move', { column }))}
            onAssign={(assignee) => change(patch({ assignee }))}
            onArchive={() => change(post('archive'))}
            onRestore={() => change(post('restore'))}
          />
          <CardDescription
            card={card}
            canWrite={canWrite}
            editing={editing}
            busy={held}
            onEdit={() => setEditing(true)}
            onSave={(fields) => change(patch(fields))}
            onCancel={() => setEditing(false)}
          />
          <Blocked
            key={`blocked-${card.version}`}
            card={card}
            board={board}
            canWrite={canWrite}
            busy={held}
            onSet={(reason, by) => change(patch({ blocked_reason: reason, blocked_by: by }))}
          />
          <Planning
            card={card}
            canWrite={canWrite}
            busy={held}
            onChange={(fields) => change(patch(fields), card, true)}
          />
          <Links
            workspace={workspace}
            card={card}
            canWrite={canWrite}
            busy={held}
            onChange={(fields) => change(patch(fields))}
          />
          <Thread
            url={`${url}/comments`}
            comments={card.comments}
            canWrite={canWrite}
            people={people.data}
            onAdded={addComment}
          />
        </>
      )}
    </article>
  )
}
