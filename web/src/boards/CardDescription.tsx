import { useState } from 'react'
import type { Card } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'

// The card's description, and the editor of its title and description.
export function CardDescription({
  card,
  canWrite,
  editing,
  busy,
  onEdit,
  onSave,
  onCancel,
}: {
  card: Card
  canWrite: boolean
  editing: boolean
  busy: boolean
  onEdit: () => void
  onSave: (fields: { title: string; body: string }) => void
  onCancel: () => void
}) {
  const t = useT()
  if (editing)
    return (
      <EditCard
        // Per card, not per version: a due date or label saved meanwhile keeps the draft.
        key={card.id}
        card={card}
        busy={busy}
        onSave={onSave}
        onCancel={onCancel}
      />
    )
  return (
    <>
      {card.body.trim() ? (
        <Markdown text={card.body} />
      ) : (
        <p className="tt-muted">{canWrite ? t('boards.noDescriptionYet') : t('boards.noDescription')}</p>
      )}
      {canWrite && (
        <div className="tt-actions">
          <button type="button" className="tt-button" onClick={onEdit}>
            {t('boards.edit')}
          </button>
        </div>
      )}
    </>
  )
}

function EditCard({
  card,
  busy,
  onSave,
  onCancel,
}: {
  card: Card
  busy: boolean
  onSave: (fields: { title: string; body: string }) => void
  onCancel: () => void
}) {
  const [title, setTitle] = useState(card.title)
  const [body, setBody] = useState(card.body)
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  return (
    <form
      className="tt-editor"
      aria-label={t('boards.editCard')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave({ title: title.trim(), body })
      }}
    >
      <input
        aria-label={t('boards.cardTitle')}
        value={title}
        maxLength={200}
        onChange={(e) => setTitle(e.target.value)}
      />
      <fieldset className="tt-view-tabs" aria-label={t('boards.view')}>
        <button type="button" aria-pressed={tab === 'write'} onClick={() => setTab('write')}>
          {t('boards.write')}
        </button>
        <button type="button" aria-pressed={tab === 'preview'} onClick={() => setTab('preview')}>
          {t('boards.preview')}
        </button>
      </fieldset>
      {tab === 'write' ? (
        <textarea
          aria-label={t('boards.description')}
          value={body}
          rows={8}
          onChange={(e) => setBody(e.target.value)}
        />
      ) : (
        <div className="tt-preview">
          <Markdown text={body} />
        </div>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !title.trim()}>
          {t('boards.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
