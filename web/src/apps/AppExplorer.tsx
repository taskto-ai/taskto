import { useId, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { type AppDefinition, type AppField, type AppRecord, api, type RecordPage } from '../api'
import { type Format, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { useBeforeLeave } from '../router'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Skeleton } from '../ui/Skeleton'
import { type Column, Table } from '../ui/Table'
import { Tabs, tabPanelProps } from '../ui/Tabs'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import {
  actionOf,
  defaultSort,
  type Filter,
  filterable,
  firstDir,
  numberFields,
  opsFor,
  PAGE_SIZE,
  type Query,
  queryString,
  type Sort,
  sortable,
  type Tab,
  UPDATED,
} from './explorer'
import { RecordDetail } from './RecordDetail'
import { optionLabel, shown, who } from './RecordParts'
import './AppExplorer.css'
import { byPlace } from '../byPlace'

const NARROW = '(max-width: 48rem)'

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
  leaveRef,
}: {
  base: string
  appId: number
  definition: AppDefinition | undefined
  release?: number
  canChange: boolean
  canRestore: boolean
  onChanged: () => void
  leaveRef?: { current: ((go: () => void) => void) | null }
}) {
  const t = useT()
  const f = useFormat()
  const narrow = useNarrow()
  const tabsId = useId()
  const fields = definition?.collection.fields ?? []
  const [tab, setTab] = useState<Tab>('live')
  const [filters, setFilters] = useState<Filter[]>([])
  const [search, setSearch] = useState('')
  const [searchDraft, setSearchDraft] = useState('')
  const [sort, setSort] = useState<Sort>(defaultSort('live'))
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

  const unsaved = useRef(false)
  const [leaveDialog, confirmLeave] = useConfirm()
  const askToLeave = () =>
    confirmLeave({
      title: t('apps.explorer.leave.title'),
      body: t('apps.explorer.leave.body'),
      confirmLabel: t('apps.explorer.leave.discard'),
      cancelLabel: t('apps.explorer.leave.keep'),
      tone: 'danger',
    })
  const leaving = (go: () => void) => {
    if (!unsaved.current) return go()
    void askToLeave().then((discard) => {
      if (discard) go()
    })
  }
  if (leaveRef) leaveRef.current = leaving
  useBeforeLeave((go, whole) =>
    leaving(() => {
      if (unsaved.current && !whole) setOpen(null)
      go()
    }),
  )
  const count = useRef<HTMLParagraphElement>(null)
  const [opening, setOpening] = useState(0)
  const openingNow = useRef(opening)
  openingNow.current = opening
  const show = (id: number | null) => {
    setOpen(id)
    if (id !== null) setOpening((n) => n + 1)
  }
  const closeRecord = (how?: 'gone', from?: number) => {
    if (how === 'gone') {
      if (from !== openingNow.current) return
      setOpen(null)
      setTimeout(() => count.current?.focus(), 0)
      return
    }
    leaving(() => setOpen(null))
  }
  const openRecord = (id: number) => {
    if (id !== open) leaving(() => show(id))
  }

  const ask = (change: () => void) =>
    leaving(() => {
      change()
      setCursors([''])
      setOpen(null)
    })
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
  const deleted = tab === 'deleted'

  return (
    <div className="ui-explorer">
      <p className="ui-explorer-intro">{t('apps.records.intro')}</p>
      <Tabs
        id={tabsId}
        label={t('apps.records.tabs')}
        items={(['live', 'deleted'] as const).map((k) => ({ id: k, label: t(`apps.records.${k}`) }))}
        value={tab}
        onChange={(k) => chooseTab(k as Tab)}
      />
      <div className="ui-explorer-panel" {...tabPanelProps(tabsId, tab)}>
        <search>
          <form
            className="ui-explorer-search"
            onSubmit={(e) => {
              e.preventDefault()
              ask(() => setSearch(searchDraft))
            }}
          >
            <Input
              type="search"
              aria-label={t('apps.explorer.search')}
              placeholder={t('apps.explorer.search')}
              value={searchDraft}
              onChange={(e) => setSearchDraft(e.target.value)}
            />
            <Button type="submit" icon="search">
              {t('apps.explorer.searchGo')}
            </Button>
          </form>
        </search>

        <FilterBar
          fields={filterable(fields)}
          filters={filters}
          onChange={(next, applied) =>
            ask(() => {
              setFilters(next)
              applied?.()
            })
          }
        />

        {narrow && (
          <div className="ui-explorer-sort">
            <Field label={t('apps.explorer.sortBy')}>
              <Select
                value={sort.by}
                onChange={(e) => {
                  const by = e.target.value
                  ask(() => setSort({ by, dir: firstDir(by) }))
                }}
              >
                <option value={deleted ? '_deleted' : UPDATED}>
                  {t(deleted ? 'apps.explorer.deletedAt' : 'apps.explorer.changedAt')}
                </option>
                {sortable(fields).map((field) => (
                  <option key={field.name} value={field.name}>
                    {field.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Button
              icon={sort.dir === 'asc' ? 'up' : 'down'}
              onClick={() => ask(() => setSort({ ...sort, dir: sort.dir === 'asc' ? 'desc' : 'asc' }))}
            >
              {t(sort.dir === 'asc' ? 'apps.explorer.ascending' : 'apps.explorer.descending')}
            </Button>
          </div>
        )}

        {list.error && <Notice tone="danger">{list.error}</Notice>}
        {!page && !list.error && (
          <div className="ui-explorer-loading" role="status">
            <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} shape="block" height="var(--control-md)" />
            ))}
          </div>
        )}
        {page && page.records.length === 0 && (
          <EmptyState
            title={t(
              filters.length > 0 || search
                ? 'apps.explorer.noMatch'
                : deleted
                  ? 'apps.records.noneDeleted'
                  : 'apps.records.none',
            )}
          />
        )}
        {page && page.records.length > 0 && !narrow && (
          <RecordTable
            records={page.records}
            fields={fields}
            sort={sort}
            deleted={deleted}
            onSort={sortBy}
            onOpen={openRecord}
          />
        )}
        {page && page.records.length > 0 && narrow && (
          <RecordCards records={page.records} fields={fields} deleted={deleted} onOpen={openRecord} />
        )}

        {page && (
          <footer className="ui-explorer-foot">
            <p role="status" className="ui-explorer-count" ref={count} tabIndex={-1}>
              {page.records.length > 0
                ? t('apps.explorer.showing', {
                    from: f.number(first),
                    to: f.number(first + page.records.length - 1),
                    total: f.number(page.total),
                  })
                : t('apps.explorer.total', { total: f.number(page.total) })}
            </p>
            <div className="ui-explorer-pager">
              {cursors.length > 1 && (
                <Button size="sm" variant="ghost" onClick={() => setCursors([''])}>
                  {t('apps.explorer.first')}
                </Button>
              )}
              <Button
                size="sm"
                icon="chevron-start"
                disabled={cursors.length === 1}
                onClick={() => setCursors((c) => c.slice(0, -1))}
              >
                {t('apps.explorer.previous')}
              </Button>
              <Button
                size="sm"
                disabled={!page.next_cursor}
                onClick={() => page.next_cursor && setCursors((c) => [...c, page.next_cursor as string])}
              >
                {t('apps.explorer.next')}
              </Button>
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
      {open !== null && (
        <RecordDetail
          key={`${open}-${opening}`}
          base={base}
          appId={appId}
          id={open}
          fields={fields}
          updateAction={actionOf(definition?.actions, 'update')}
          deleteAction={actionOf(definition?.actions, 'delete')}
          release={release}
          deleted={deleted}
          canChange={canChange}
          canRestore={canRestore}
          onClose={(how) => closeRecord(how, opening)}
          onChanged={changed}
          onUnsaved={(now) => {
            unsaved.current = now
          }}
        />
      )}
      {leaveDialog}
    </div>
  )
}

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
  if (numbers.length === 0) return null
  const by = (kind: string) => page.aggregates.find((a) => a.kind === kind && a.field === field)
  const number = (kind: string) => {
    const v = by(kind)?.value
    return typeof v === 'number' ? f.number(v) : t('apps.explorer.empty')
  }
  return (
    <section className="ui-explorer-totals" aria-label={t('apps.explorer.totalsLabel')}>
      <div className="ui-explorer-totals-head">
        <Field label={t('apps.explorer.totalsOf')}>
          <Select value={field ?? ''} onChange={(e) => onField(e.target.value || null)}>
            <option value="">{t('apps.explorer.totalsNone')}</option>
            {numbers.map((n) => (
              <option key={n.name} value={n.name}>
                {n.name}
              </option>
            ))}
          </Select>
        </Field>
        {field && <p className="ui-explorer-note">{t('apps.explorer.totalsScope', { total: f.number(page.total) })}</p>}
      </div>
      {field && (
        <dl className="ui-explorer-aggregates">
          {(['sum', 'avg', 'min', 'max'] as const).map((kind) => (
            <div key={kind} className="ui-stat">
              <dt className="ui-stat-label">{t(`apps.explorer.agg.${kind}`)}</dt>
              <dd className="ui-stat-value">{number(kind)}</dd>
            </div>
          ))}
        </dl>
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
  onChange: (next: Filter[], applied?: () => void) => void
}) {
  const t = useT()
  const f = useFormat()
  const [field, setField] = useState(fields[0]?.name ?? '')
  const type = fields.find((x) => x.name === field)
  const ops = type ? opsFor(type.type) : []
  const [chosen, setOp] = useState('eq')
  const [value, setValue] = useState('')
  const id = useId()
  const op = ops.includes(chosen) ? chosen : 'eq'
  if (fields.length === 0) return null
  const add = (e: React.FormEvent) => {
    e.preventDefault()
    if (value === '' || !type) return
    onChange([...filters, { field, op, value }], () => setValue(''))
  }
  const opLabel = (o: string) => t(`apps.explorer.op.${o}` as 'apps.explorer.op.eq')
  return (
    <div className="ui-explorer-filters">
      <form className="ui-explorer-filter-form" aria-label={t('apps.explorer.filters')} onSubmit={add}>
        <Select aria-label={t('apps.explorer.filterField')} value={field} onChange={(e) => setField(e.target.value)}>
          {fields.map((x) => (
            <option key={x.name} value={x.name}>
              {x.name}
            </option>
          ))}
        </Select>
        <Select aria-label={t('apps.explorer.filterOp')} value={op} onChange={(e) => setOp(e.target.value)}>
          {ops.map((o) => (
            <option key={o} value={o}>
              {opLabel(o)}
            </option>
          ))}
        </Select>
        <FilterValue id={`${id}-value`} field={type} value={value} onChange={setValue} />
        <Button type="submit" icon="filter" disabled={value === ''}>
          {t('apps.explorer.addFilter')}
        </Button>
      </form>
      {filters.length > 0 && (
        <ul className="ui-explorer-chips" aria-label={t('apps.explorer.activeFilters')}>
          {byPlace(filters).map(({ key, item: fl, place: i }) => {
            const text = `${fl.field} ${opLabel(fl.op)} ${chipValue(fl, fields, f, t)}`
            return (
              <li key={key} className="ui-explorer-chip">
                <span>{text}</span>
                <IconButton
                  size="sm"
                  icon="close"
                  label={t('apps.explorer.removeFilter', { filter: text })}
                  onClick={() => onChange(filters.filter((_, j) => j !== i))}
                />
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

function chipValue(fl: Filter, fields: AppField[], f: Format, t: T): string {
  const field = fields.find((x) => x.name === fl.field)
  const type = field?.type
  if (type === 'boolean') return t(fl.value === 'true' ? 'apps.records.yes' : 'apps.records.no')
  if (type === 'number' && !Number.isNaN(Number(fl.value))) return f.number(Number(fl.value))
  return shown(fl.value, type, f, t, field)
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
        : (field.options ?? []).map((o) => [o, optionLabel(field, o)])
    return (
      <Select id={id} aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{label}</option>
        {options.map(([v, l]) => (
          <option key={v} value={v}>
            {l}
          </option>
        ))}
      </Select>
    )
  }
  const type = field?.type === 'number' || field?.type === 'date' || field?.type === 'time' ? field.type : 'text'
  return (
    <Input
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
  const sortOf = (by: string) => ({
    direction:
      sort.by === by ? (sort.dir === 'asc' ? ('ascending' as const) : ('descending' as const)) : ('none' as const),
    onSort: () => onSort(by),
  })
  const columns: Column<AppRecord>[] = [
    {
      key: '_id',
      header: t('apps.explorer.id'),
      rowHeader: true,
      cell: (rec) => (
        <Button
          size="sm"
          variant="ghost"
          aria-label={t('apps.explorer.openRecord', { id: String(rec.id) })}
          onClick={() => onOpen(rec.id)}
        >
          {rec.id}
        </Button>
      ),
    },
    ...fields.map(
      (field): Column<AppRecord> => ({
        key: field.name,
        header: field.name,
        numeric: field.type === 'number',
        sort: sortable([field]).length > 0 ? sortOf(field.name) : undefined,
        cell: (rec) => shown(rec.data[field.name], field.type, f, t, field),
      }),
    ),
    {
      key: timeKey,
      header: t(deleted ? 'apps.explorer.deletedAt' : 'apps.explorer.changedAt'),
      sort: sortOf(timeKey),
      cell: (rec) => (
        <span className="ui-explorer-when">
          {f.dateTime(deleted ? (rec.deleted_at ?? rec.updated_at) : rec.updated_at)}
        </span>
      ),
    },
  ]
  return <Table caption={t('apps.records.open')} columns={columns} rows={records} rowKey={(rec) => rec.id} />
}

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
    <Rows label={t('apps.records.open')}>
      {records.map((rec) => {
        const [lead, ...rest] = fields
        const title = lead ? shown(rec.data[lead.name], lead.type, f, t, lead) : ''
        const values = rest
          .map((field) => [field.name, shown(rec.data[field.name], field.type, f, t, field)] as const)
          .filter(([, v]) => v !== '')
        return (
          <Row
            key={rec.id}
            title={title || t('apps.records.record', { id: String(rec.id) })}
            detail={
              <>
                {values.length > 0 && (
                  <span className="ui-explorer-card-fields">
                    {values.map(([name, v]) => `${name}: ${v}`).join(' · ')}
                  </span>
                )}
                <span className="ui-explorer-when">
                  {deleted
                    ? t('apps.records.deletedBy', {
                        when: f.dateTime(rec.deleted_at ?? rec.updated_at),
                        name: who(rec.deleted_by, t),
                      })
                    : t('apps.records.changed', { when: f.dateTime(rec.updated_at), name: who(rec.updated_by, t) })}
                </span>
              </>
            }
            actions={
              <IconButton
                icon="chevron-end"
                label={t('apps.explorer.openRecord', { id: String(rec.id) })}
                onClick={() => onOpen(rec.id)}
              />
            }
          />
        )
      })}
    </Rows>
  )
}
