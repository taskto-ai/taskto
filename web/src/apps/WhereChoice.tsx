// Where a new app lives, for the builder's start and a template's install: the person's private
// space unless they choose a shared one, with what each means. Only spaces the person may write are
// offered; the server checks the one chosen again.

import type { ReactNode } from 'react'
import type { Space } from '../api'
import { useT } from '../i18n'
import { NARROW, useMediaQuery } from '../media'
import { spaceLabel } from '../spaces'
import { Field } from '../ui/Field'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Select } from '../ui/Select'
import './Apps.css'

export type Where = 'private' | 'shared'

// The spaces a new app may go to: the person's private one, and the shared ones they may write.
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
  // The chosen shared space's id, as a string ('' for none yet).
  shared: string
  onShared: (id: string) => void
  // More to say under the choice (a template's roles, a better place for it).
  children?: ReactNode
}) {
  const t = useT()
  const { mine, shared: sharedSpaces } = writableSpaces(spaces)
  const label = t('apps.builder.where')
  const narrow = useMediaQuery(NARROW)
  return (
    <div className="apps-where">
      {/* The group is named by its own label; this is the same words, seen. */}
      <span className="apps-where-label" aria-hidden="true">
        {label}
      </span>
      <SegmentedControl<Where>
        label={label}
        // The two choices are a pill that can't wrap: on a phone it takes the small size, which
        // fits the narrowest dialog.
        size={narrow ? 'sm' : 'md'}
        value={where}
        onChange={(next) => {
          onWhere(next)
          if (next === 'shared' && !shared) onShared(String(sharedSpaces[0]?.id ?? ''))
        }}
        options={[
          { value: 'private', label: t('apps.builder.wherePrivate'), disabled: !mine },
          { value: 'shared', label: t('apps.builder.whereShared'), disabled: sharedSpaces.length === 0 },
        ]}
      />
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
