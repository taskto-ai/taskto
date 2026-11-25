import { useState } from 'react'
import type { Card } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Textarea } from '../ui/Textarea'

// The card's description, rendered as Markdown (a task list's boxes are checkboxes), and the
// editor of its title and description.
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
  return (
    <section className="ui-board-section" data-focus-group>
      <div className="ui-board-section-head">
        <Heading level={3} size="subsection">
          {t('boards.description')}
        </Heading>
        {canWrite && !editing && (
          <Button size="sm" variant="ghost" icon="edit" onClick={onEdit}>
            {t('boards.edit')}
          </Button>
        )}
      </div>
      {editing ? (
        <EditCard
          // Per card, not per version: a due date or label saved meanwhile keeps the draft.
          key={card.id}
          card={card}
          busy={busy}
          onSave={onSave}
          onCancel={onCancel}
        />
      ) : card.body.trim() ? (
        <div className="ui-board-reading">
          <Markdown text={card.body} />
        </div>
      ) : (
        <p className="ui-board-muted">{canWrite ? t('boards.noDescriptionYet') : t('boards.noDescription')}</p>
      )}
    </section>
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
      className="ui-board-form"
      aria-label={t('boards.editCard')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave({ title: title.trim(), body })
      }}
    >
      <Field label={t('boards.cardTitle')}>
        <Input value={title} maxLength={200} onChange={(e) => setTitle(e.target.value)} autoFocus />
      </Field>
      <SegmentedControl
        size="sm"
        label={t('boards.view')}
        value={tab}
        onChange={setTab}
        options={[
          { value: 'write', label: t('boards.write') },
          { value: 'preview', label: t('boards.preview') },
        ]}
      />
      {tab === 'write' ? (
        <Textarea
          aria-label={t('boards.description')}
          value={body}
          rows={8}
          onChange={(e) => setBody(e.target.value)}
        />
      ) : (
        <div className="ui-board-reading ui-board-preview">
          <Markdown text={body} />
        </div>
      )}
      <div className="ui-board-actions">
        <Button type="submit" size="sm" disabled={busy || !title.trim()}>
          {t('boards.save')}
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          {t('common.cancel')}
        </Button>
      </div>
    </form>
  )
}
