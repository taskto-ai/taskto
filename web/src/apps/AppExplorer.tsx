// The data explorer of one launched app: its records a page at a time, which the server sorts,
// filters and searches, with the totals of everything that matches (never of the page alone). A table
// on a wide screen, cards on a phone; opening a record edits it, shows its history and, for a deleted
// one, restores it. The server decides what each person reads and may change: this only asks.

import { useId, useMemo, useState, useSyncExternalStore } from 'react'
import { type AppDefinition, type AppField, type AppRecord, api, type RecordPage } from '../api'
import { type Format, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import {
  actionOf,
  defaultSort,
  type Filter,
  firstDir,
  numberFields,
  opsFor,
  PAGE_SIZE,
  type Query,
  queryable,
  queryString,
  type Sort,
  sortable,
  type Tab,
  UPDATED,
} from './explorer'
import { RecordDetail } from './RecordDetail'
import { shown, who } from './RecordParts'

const NARROW = '(max-width: 48rem)'
// On the button that removes a filter, which is named for assistive technology by its label.
const REMOVE_MARK = '×'

// Whether the screen is a phone's: the explorer draws cards there and a table elsewhere.
export function useNarrow(): boolean {
  const media = useMemo(
    () => (typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(NARROW) : null),
    [],
  )
  return useSyncExternalStore(
    (notify) => {
      media?.addEventListener('change', notify)
      return () => media?.removeEventListener('change', notify)
    },
    () => media?.matches ?? false,
  )
}

export function AppExplorer({
  base,
  appId,
  definition,
  release,
  canChange,
  canRestore,
  onChanged,
}: {
  // …/spaces/{space}/apps/{app}
  base: string
  appId: number
  // The live release's definition: its fields and actions. An app that isn't published has none.
  definition: AppDefinition | undefined
  release?: number
  // Whether the caller may change records (the space lets them write and their roles allow it).
  canChange: boolean
  canRestore: boolean
  // A record was changed, deleted or restored: the app's screen is loaded again.
  onChanged: () => void
}) {
  const t = useT()
  const f = useFormat()
  const narrow = useNarrow()
  const fields = definition?.collection.fields ?? []
  const [tab, setTab] = useState<Tab>('live')
  const [filters, setFilters] = useState<Filter[]>([])
  const [search, setSearch] = useState('')
  const [searchDraft, setSearchDraft] = useState('')
  const [sort, setSort] = useState<Sort>(defaultSort('live'))
  // The cursors of the pages visited, the first page's being none: Previous goes back down the stack.
  const [cursors, setCursors] = useState<string[]>([''])
  const numbers = numberFields(fields)
  const [totalsField, setTotalsField] = useState<string | null>(numbers[0]?.name ?? null)
  const [open, setOpen] = useState<number | null>(null)
  const query: Query = {
    tab,
    filters,
    search,
    sort,
    cursor: cursors[cursors.length - 1],
    totals: totalsField,
  }
  const qs = queryString(query)
  const list = useLoad(() => api<RecordPage>(`${base}/records/query?${qs}`), [base, qs])

  // A new question starts from its first page.
  const ask = (change: () => void) => {
    change()
    setCursors([''])
    setOpen(null)
  }
  const chooseTab = (next: Tab) =>
    ask(() => {
      setTab(next)
      setSort(defaultSort(next))
    })
  const sortBy = (by: string) =>
    ask(() =>
      setSort((cur) => (cur.by === by ? { by, dir: cur.dir === 'asc' ? 'desc' : 'asc' } : { by, dir: firstDir(by) })),
    )

  const page = list.data
  const first = (cursors.length - 1) * PAGE_SIZE + 1
  const changed = () => {
    list.reload()
    onChanged()
  }

  if (open !== null) {
    return (
      <RecordDetail
        base={base}
        appId={appId}
        id={open}
        fields={fields}
        updateAction={actionOf(definition?.actions, 'update')}
        deleteAction={actionOf(definition?.actions, 'delete')}
        release={release}
        deleted={tab === 'deleted'}
        canChange={canChange}
        canRestore={canRestore}
        onClose={() => setOpen(null)}
        onChanged={changed}
      />
    )
  }

  return (
    <div className="tt-explorer">
      <p className="tt-muted tt-small">{t('apps.records.intro')}</p>
      <nav className="tt-records-tabs" aria-label={t('apps.records.tabs')}>
        {(['live', 'deleted'] as const).map((k) => (
          <button
            key={k}
            type="button"
            className={`tt-button${tab === k ? ' tt-primary' : ''}`}
            aria-pressed={tab === k}
            onClick={() => chooseTab(k)}
          >
            {t(`apps.records.${k}`)}
          </button>
        ))}
      </nav>

      <search>
        <form
          className="tt-explorer-search"
          onSubmit={(e) => {
            e.preventDefault()
            ask(() => setSearch(searchDraft))
          }}
        >
          <input
            type="search"
            aria-label={t('apps.explorer.search')}
            placeholder={t('apps.explorer.search')}
            value={searchDraft}
            onChange={(e) => setSearchDraft(e.target.value)}
          />
          <button type="submit" className="tt-button">
            {t('apps.explorer.searchGo')}
          </button>
        </form>
      </search>

      <FilterBar fields={queryable(fields)} filters={filters} onChange={(next) => ask(() => setFilters(next))} />

      {narrow && (
        <div className="tt-explorer-sort">
          <label>
            {t('apps.explorer.sortBy')}
            <select
              value={sort.by}
              onChange={(e) => ask(() => setSort({ by: e.target.value, dir: firstDir(e.target.value) }))}
            >
              <option value={tab === 'deleted' ? '_deleted' : UPDATED}>
                {t(tab === 'deleted' ? 'apps.explorer.deletedAt' : 'apps.explorer.changedAt')}
              </option>
              {sortable(fields).map((field) => (
                <option key={field.name} value={field.name}>
                  {field.name}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className="tt-button"
            onClick={() => ask(() => setSort({ ...sort, dir: sort.dir === 'asc' ? 'desc' : 'asc' }))}
          >
            {t(sort.dir === 'asc' ? 'apps.explorer.ascending' : 'apps.explorer.descending')}
          </button>
        </div>
      )}

      {list.error && (
        <p role="alert" className="tt-error">
          {list.error}
        </p>
      )}
      {!page && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
      {page && page.records.length === 0 && (
        <p className="tt-muted">
          {t(
            filters.length > 0 || search
              ? 'apps.explorer.noMatch'
              : tab === 'deleted'
                ? 'apps.records.noneDeleted'
                : 'apps.records.none',
          )}
        </p>
      )}
      {page && page.records.length > 0 && !narrow && (
        <RecordTable
          records={page.records}
          fields={fields}
          sort={sort}
          deleted={tab === 'deleted'}
          onSort={sortBy}
          onOpen={setOpen}
        />
      )}
      {page && page.records.length > 0 && narrow && (
        <RecordCards records={page.records} fields={fields} deleted={tab === 'deleted'} onOpen={setOpen} />
      )}

      {page && (
        <footer className="tt-explorer-foot">
          <p role="status" className="tt-small">
            {page.records.length > 0
              ? t('apps.explorer.showing', {
                  from: f.number(first),
                  to: f.number(first + page.records.length - 1),
                  total: f.number(page.total),
                })
              : t('apps.explorer.total', { total: f.number(page.total) })}
          </p>
          <div className="tt-record-actions">
            <button
              type="button"
              className="tt-button"
              disabled={cursors.length === 1}
              onClick={() => setCursors((c) => c.slice(0, -1))}
            >
              {t('apps.explorer.previous')}
            </button>
            <button
              type="button"
              className="tt-button"
              disabled={!page.next_cursor}
              onClick={() => page.next_cursor && setCursors((c) => [...c, page.next_cursor as string])}
            >
              {t('apps.explorer.next')}
            </button>
            {cursors.length > 1 && (
              <button type="button" className="tt-button" onClick={() => setCursors([''])}>
                {t('apps.explorer.first')}
              </button>
            )}
          </div>
          <Totals
            page={page}
            numbers={numbers}
            field={totalsField}
            onField={(name) => ask(() => setTotalsField(name))}
          />
        </footer>
      )}
    </div>
  )
}

// The totals of every record that matches, whichever page is shown.
function Totals({
  page,
  numbers,
  field,
  onField,
}: {
  page: RecordPage
  numbers: AppField[]
  field: string | null
  onField: (name: string | null) => void
}) {
  const t = useT()
  const f = useFormat()
  const id = useId()
  if (numbers.length === 0) return null
  const by = (kind: string) => page.aggregates.find((a) => a.kind === kind && a.field === field)
  const number = (kind: string) => {
    const v = by(kind)?.value
    return typeof v === 'number' ? f.number(v) : t('apps.explorer.empty')
  }
  return (
    <section className="tt-explorer-totals" aria-label={t('apps.explorer.totalsLabel')}>
      <label htmlFor={id}>{t('apps.explorer.totalsOf')}</label>
      <select id={id} value={field ?? ''} onChange={(e) => onField(e.target.value || null)}>
        <option value="">{t('apps.explorer.totalsNone')}</option>
        {numbers.map((n) => (
          <option key={n.name} value={n.name}>
            {n.name}
          </option>
        ))}
      </select>
      {field && (
        <>
          <p className="tt-small tt-muted">{t('apps.explorer.totalsScope', { total: f.number(page.total) })}</p>
          <dl className="tt-explorer-aggregates">
            {(['sum', 'avg', 'min', 'max'] as const).map((kind) => (
              <div key={kind}>
                <dt>{t(`apps.explorer.agg.${kind}`)}</dt>
                <dd>{number(kind)}</dd>
              </div>
            ))}
          </dl>
        </>
      )}
    </section>
  )
}

function FilterBar({
  fields,
  filters,
  onChange,
}: {
  fields: AppField[]
  filters: Filter[]
  onChange: (next: Filter[]) => void
}) {
  const t = useT()
  const f = useFormat()
  const [field, setField] = useState(fields[0]?.name ?? '')
  const type = fields.find((x) => x.name === field)
  const ops = type ? opsFor(type.type) : []
  const [chosen, setOp] = useState('eq')
  const [value, setValue] = useState('')
  const id = useId()
  // The operator offered follows the field's type.
  const op = ops.includes(chosen) ? chosen : 'eq'
  if (fields.length === 0) return null
  const add = (e: React.FormEvent) => {
    e.preventDefault()
    if (value === '' || !type) return
    onChange([...filters, { field, op, value }])
    setValue('')
  }
  const opLabel = (o: string) => t(`apps.explorer.op.${o}` as 'apps.explorer.op.eq')
  return (
    <div className="tt-explorer-filters">
      <form className="tt-explorer-filter-form" aria-label={t('apps.explorer.filters')} onSubmit={add}>
        <select aria-label={t('apps.explorer.filterField')} value={field} onChange={(e) => setField(e.target.value)}>
          {fields.map((x) => (
            <option key={x.name} value={x.name}>
              {x.name}
            </option>
          ))}
        </select>
        <select aria-label={t('apps.explorer.filterOp')} value={op} onChange={(e) => setOp(e.target.value)}>
          {ops.map((o) => (
            <option key={o} value={o}>
              {opLabel(o)}
            </option>
          ))}
        </select>
        <FilterValue id={`${id}-value`} field={type} value={value} onChange={setValue} />
        <button type="submit" className="tt-button" disabled={value === ''}>
          {t('apps.explorer.addFilter')}
        </button>
      </form>
      {filters.length > 0 && (
        <ul className="tt-explorer-chips" aria-label={t('apps.explorer.activeFilters')}>
          {filters.map((fl, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: filters have no identity but their place
            <li key={i}>
              <span>
                {fl.field} {opLabel(fl.op)} {chipValue(fl, fields, f, t)}
              </span>
              <button
                type="button"
                className="tt-button"
                aria-label={t('apps.explorer.removeFilter', {
                  filter: `${fl.field} ${opLabel(fl.op)} ${chipValue(fl, fields, f, t)}`,
                })}
                onClick={() => onChange(filters.filter((_, j) => j !== i))}
              >
                {REMOVE_MARK}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function chipValue(fl: Filter, fields: AppField[], f: Format, t: T): string {
  const type = fields.find((x) => x.name === fl.field)?.type
  if (type === 'boolean') return t(fl.value === 'true' ? 'apps.records.yes' : 'apps.records.no')
  if (type === 'number' && !Number.isNaN(Number(fl.value))) return f.number(Number(fl.value))
  return shown(fl.value, type, f, t)
}

function FilterValue({
  id,
  field,
  value,
  onChange,
}: {
  id: string
  field: AppField | undefined
  value: string
  onChange: (v: string) => void
}) {
  const t = useT()
  const label = t('apps.explorer.filterValue')
  if (field?.type === 'boolean' || field?.type === 'choice') {
    const options =
      field.type === 'boolean'
        ? [
            ['true', t('apps.records.yes')],
            ['false', t('apps.records.no')],
          ]
        : (field.options ?? []).map((o) => [o, o])
    return (
      <select id={id} aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{label}</option>
        {options.map(([v, l]) => (
          <option key={v} value={v}>
            {l}
          </option>
        ))}
      </select>
    )
  }
  const type = field?.type === 'number' || field?.type === 'date' || field?.type === 'time' ? field.type : 'text'
  return (
    <input
      id={id}
      type={type}
      step={type === 'number' ? 'any' : undefined}
      aria-label={label}
      placeholder={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

function RecordTable({
  records,
  fields,
  sort,
  deleted,
  onSort,
  onOpen,
}: {
  records: AppRecord[]
  fields: AppField[]
  sort: Sort
  deleted: boolean
  onSort: (by: string) => void
  onOpen: (id: number) => void
}) {
  const t = useT()
  const f = useFormat()
  const timeKey = deleted ? '_deleted' : UPDATED
  return (
    <div className="tt-table-scroll">
      <table className="tt-table tt-explorer-table">
        <thead>
          <tr>
            <th scope="col">{t('apps.explorer.id')}</th>
            {fields.map((field) =>
              sortable([field]).length > 0 ? (
                <Head key={field.name} by={field.name} text={field.name} sort={sort} onSort={onSort} />
              ) : (
                // What can't be put in order is a heading only: the server refuses to sort by it.
                <th key={field.name} scope="col">
                  {field.name}
                </th>
              ),
            )}
            <Head
              by={timeKey}
              text={t(deleted ? 'apps.explorer.deletedAt' : 'apps.explorer.changedAt')}
              sort={sort}
              onSort={onSort}
            />
          </tr>
        </thead>
        <tbody>
          {records.map((rec) => (
            <tr key={rec.id}>
              <th scope="row">
                <button
                  type="button"
                  className="tt-button"
                  aria-label={t('apps.explorer.openRecord', { id: String(rec.id) })}
                  onClick={() => onOpen(rec.id)}
                >
                  {rec.id}
                </button>
              </th>
              {fields.map((field) => (
                <td key={field.name} className={field.type === 'number' ? 'tt-num' : undefined}>
                  {shown(rec.data[field.name], field.type, f, t)}
                </td>
              ))}
              <td className="tt-small">{f.dateTime(deleted ? (rec.deleted_at ?? rec.updated_at) : rec.updated_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Head({ by, text, sort, onSort }: { by: string; text: string; sort: Sort; onSort: (by: string) => void }) {
  return (
    <th scope="col" aria-sort={sort.by === by ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className="tt-sort" onClick={() => onSort(by)}>
        {text}
        {sort.by === by && <span aria-hidden="true">{sort.dir === 'asc' ? ' ▲' : ' ▼'}</span>}
      </button>
    </th>
  )
}

// A phone's records: one card each, the first field its title, then the rest.
function RecordCards({
  records,
  fields,
  deleted,
  onOpen,
}: {
  records: AppRecord[]
  fields: AppField[]
  deleted: boolean
  onOpen: (id: number) => void
}) {
  const t = useT()
  const f = useFormat()
  return (
    <ul className="tt-record-list tt-explorer-cards">
      {records.map((rec) => {
        const [lead, ...rest] = fields
        const title = lead ? shown(rec.data[lead.name], lead.type, f, t) : ''
        return (
          <li key={rec.id} className="tt-record">
            <button
              type="button"
              className="tt-record-open"
              aria-label={t('apps.explorer.openRecord', { id: String(rec.id) })}
              onClick={() => onOpen(rec.id)}
            >
              <strong>{title || t('apps.records.record', { id: String(rec.id) })}</strong>
            </button>
            <dl className="tt-record-fields">
              {rest.map((field) => {
                const v = shown(rec.data[field.name], field.type, f, t)
                return v === '' ? null : (
                  <div key={field.name}>
                    <dt>{field.name}</dt>
                    <dd>{v}</dd>
                  </div>
                )
              })}
            </dl>
            <div className="tt-muted tt-small">
              {deleted
                ? t('apps.records.deletedBy', {
                    when: f.dateTime(rec.deleted_at ?? rec.updated_at),
                    name: who(rec.deleted_by, t),
                  })
                : t('apps.records.changed', { when: f.dateTime(rec.updated_at), name: who(rec.updated_by, t) })}
            </div>
          </li>
        )
      })}
    </ul>
  )
}
