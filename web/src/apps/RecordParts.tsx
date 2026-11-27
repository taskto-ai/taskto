import type { AppField, RecordAuthor, RecordVersion } from '../api'
import { type Format, type T, useFormat, useT } from '../i18n'
import { coordinates, isLocation } from './capture'
import { photoIds } from './photos'

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

function calendarDay(s: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s)
  return m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : null
}

export function optionLabel(field: Pick<AppField, 'option_labels'> | undefined, option: string): string {
  const labels = field?.option_labels
  return labels && Object.hasOwn(labels, option) ? labels[option] : option
}

export function shown(
  v: unknown,
  type: AppField['type'] | undefined,
  f: Format,
  t: T,
  field?: Pick<AppField, 'option_labels'>,
): string {
  if (v === undefined || v === null || v === '') return ''
  if (type === 'choice' && typeof v === 'string') return optionLabel(field, v)
  if (type === 'location' && isLocation(v)) return coordinates(v, f)
  if (type === 'signature' && typeof v === 'string') return t('apps.signature.signed')
  if (typeof v === 'number') return f.number(v)
  if (typeof v === 'boolean') return t(v ? 'apps.records.yes' : 'apps.records.no')
  if (Array.isArray(v) && photoIds(v).length === v.length && v.length > 0)
    return t('apps.photos.count', { count: v.length })
  if (type === 'date' && typeof v === 'string') {
    const day = calendarDay(v)
    return day ? f.date(day) : v
  }
  return String(v)
}

export function preview(data: Record<string, unknown>, f: Format, t: T, fields?: AppField[]): string {
  const parts = Object.entries(data).map(([name, v]) => {
    const field = fields?.find((x) => x.name === name)
    return `${name}: ${shown(v, field?.type, f, t, field)}`
  })
  return parts.length > 0 ? parts.join(' · ') : t('apps.records.empty')
}

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
      <ol className="ui-timeline" aria-label={t('apps.records.historyNamed', { id: String(id) })}>
        {versions.map((v) => (
          <li key={v.revision} className="ui-timeline-item">
            <p className="ui-timeline-title">
              {t('apps.records.version', {
                operation: operation(v),
                revision: String(v.revision),
                when: f.dateTime(v.at),
                name: who(v.by, t),
              })}
            </p>
            <p className="ui-timeline-detail">{preview(v.data, f, t, fields)}</p>
            {v.synthetic && <p className="ui-timeline-detail">{t('apps.records.synthetic')}</p>}
          </li>
        ))}
      </ol>
      {truncated && (
        <p className="ui-timeline-detail">
          {t('apps.records.versionsTruncated', { count: f.number(versions.length) })}
        </p>
      )}
    </>
  )
}
