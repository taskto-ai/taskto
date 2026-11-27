import { type ReactNode, useRef } from 'react'
import type { Space } from '../api'
import { useT } from '../i18n'
import { usePageNarrow } from '../media'
import { spaceLabel } from '../spaces'
import { Field } from '../ui/Field'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Select } from '../ui/Select'
import './Apps.css'

export type Where = 'private' | 'shared'

export function writableSpaces(spaces: Space[] | undefined) {
  const writable = (spaces ?? []).filter((s) => s.access === 'write' && !s.archived_at)
  return { mine: writable.find((s) => s.kind === 'private'), shared: writable.filter((s) => s.kind === 'shared') }
}

export function WhereChoice({
  spaces,
  where,
  onWhere,
  shared,
  onShared,
  children,
}: {
  spaces: Space[] | undefined
  where: Where
  onWhere: (where: Where) => void
  shared: string
  onShared: (id: string) => void
  children?: ReactNode
}) {
  const t = useT()
  const { mine, shared: sharedSpaces } = writableSpaces(spaces)
  const label = t('apps.builder.where')
  const root = useRef<HTMLDivElement>(null)
  const narrow = usePageNarrow(root)
  const choose = (next: Where) => {
    onWhere(next)
    if (next === 'shared' && !shared) onShared(String(sharedSpaces[0]?.id ?? ''))
  }
  const options = [
    { value: 'private' as const, label: t('apps.builder.wherePrivate'), disabled: !mine },
    { value: 'shared' as const, label: t('apps.builder.whereShared'), disabled: sharedSpaces.length === 0 },
  ]
  return (
    <div ref={root} className="apps-where">
      {narrow ? (
        <Field label={label}>
          <Select value={where} onChange={(e) => choose(e.target.value as Where)}>
            {options.map((o) => (
              <option key={o.value} value={o.value} disabled={o.disabled}>
                {o.label}
              </option>
            ))}
          </Select>
        </Field>
      ) : (
        <>
          <span className="apps-where-label" aria-hidden="true">
            {label}
          </span>
          <SegmentedControl<Where> label={label} value={where} onChange={choose} options={options} />
        </>
      )}
      {where === 'shared' && (
        <Field label={t('apps.builder.sharedSpace')}>
          <Select value={shared} onChange={(e) => onShared(e.target.value)} required>
            {sharedSpaces.map((s) => (
              <option key={s.id} value={s.id}>
                {spaceLabel(s)}
              </option>
            ))}
          </Select>
        </Field>
      )}
      <p className="apps-note">{t(where === 'private' ? 'apps.builder.privateNote' : 'apps.builder.sharedNote')}</p>
      {sharedSpaces.length === 0 && spaces && <p className="apps-note">{t('apps.builder.noSharedSpaces')}</p>}
      {children}
    </div>
  )
}
