// What the records explorer shows of a record: who did something, its data in a line, a field's
// value as the app's definition types it, and the versions of its history.

import type { AppField, RecordAuthor, RecordVersion } from '../api'
import { type Format, type T, useFormat, useT } from '../i18n'
import { coordinates, isLocation } from './capture'
import { photoIds } from './photos'

// The name of who did something, and that it was through the agent. A change the server made on its
// own is an integration's: named as far as the server can name it ("Stripe · payments").
export function who(a: RecordAuthor | null, t: T): string {
  if (!a) return t('apps.records.unknownUser')
  if (a.system) {
    const { provider, name, removed } = a.system
    if (removed) return t('apps.records.system.removed')
    if (provider && name) return t('apps.records.system.providerNamed', { provider, name })
    if (provider || name) return t('apps.records.system.named', { name: provider || name || '' })
    return t('apps.records.system.generic')
  }
  return a.agent ? t('apps.records.agent', { name: a.name }) : a.name
}

// A date as the server stores it, YYYY-MM-DD, on the calendar day it names, whatever the time zone.
function calendarDay(s: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s)
  return m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : null
}

// How a stored option of a choice field reads: its label, or the option itself where it has none.
export function optionLabel(field: Pick<AppField, 'option_labels'> | undefined, option: string): string {
  const labels = field?.option_labels
  return labels && Object.hasOwn(labels, option) ? labels[option] : option
}

// One value in the person's language: numbers, dates and yes/no as they read, and a choice by its
// label (field is the field it is the value of, where there is one).
export function shown(
  v: unknown,
  type: AppField['type'] | undefined,
  f: Format,
  t: T,
  field?: Pick<AppField, 'option_labels'>,
): string {
  if (v === undefined || v === null || v === '') return ''
  if (type === 'choice' && typeof v === 'string') return optionLabel(field, v)
  // A captured location, in coordinates; a signature, as what it is (the picture is shown where there is room).
  if (type === 'location' && isLocation(v)) return coordinates(v, f)
  if (type === 'signature' && typeof v === 'string') return t('apps.signature.signed')
  if (typeof v === 'number') return f.number(v)
  if (typeof v === 'boolean') return t(v ? 'apps.records.yes' : 'apps.records.no')
  // A photo field holds attachment ids: what a person reads is how many, never the ids.
  if (Array.isArray(v) && photoIds(v).length === v.length && v.length > 0)
    return t('apps.photos.count', { count: v.length })
  if (type === 'date' && typeof v === 'string') {
    const day = calendarDay(v)
    return day ? f.date(day) : v
  }
  return String(v)
}

// A record's fields in one line: "name: value" for each.
export function preview(data: Record<string, unknown>, f: Format, t: T, fields?: AppField[]): string {
  const parts = Object.entries(data).map(([name, v]) => {
    const field = fields?.find((x) => x.name === name)
    return `${name}: ${shown(v, field?.type, f, t, field)}`
  })
  return parts.length > 0 ? parts.join(' · ') : t('apps.records.empty')
}

// A record's versions, newest first.
export function HistoryList({
  id,
  versions,
  truncated,
  fields,
}: {
  id: number
  versions: RecordVersion[]
  truncated: boolean
  fields?: AppField[]
}) {
  const t = useT()
  const f = useFormat()
  const operation = (v: RecordVersion) =>
    t(v.operation === 'restore' ? 'apps.records.restoreOp' : `apps.records.${v.operation}`)
  return (
    <>
      <ol className="tt-history" aria-label={t('apps.records.historyNamed', { id: String(id) })}>
        {versions.map((v) => (
          <li key={v.revision}>
            <div className="tt-small">
              {t('apps.records.version', {
                operation: operation(v),
                revision: String(v.revision),
                when: f.dateTime(v.at),
                name: who(v.by, t),
              })}
            </div>
            <div className="tt-muted tt-small tt-record-data">{preview(v.data, f, t, fields)}</div>
            {v.synthetic && <div className="tt-muted tt-small">{t('apps.records.synthetic')}</div>}
          </li>
        ))}
      </ol>
      {truncated && (
        <p className="tt-muted tt-small">{t('apps.records.versionsTruncated', { count: f.number(versions.length) })}</p>
      )}
    </>
  )
}
