import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { ApiError, errorText } from '../api'
import { optionLabel } from '../apps/RecordParts'
import { type Format, useFormat, useT } from '../i18n'
import { useChanged } from '../memo'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Field as LabelledField } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Select } from '../ui/Select'
import {
  type Deleted,
  findDeleted,
  keepServer,
  problemsOf,
  REVIEW_AGAIN,
  readCurrent,
  reapply,
  refreshDefinition,
  restoreAndApply,
  type ServerRecord,
  saveEdit,
} from './choices'
import { DiscardButton } from './DiscardButton'
import './Resolve.css'
import { fieldsOf, parseUpdate } from './local'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem, type Problem, seenOf, versionOf } from './store'

type Field = Download['definition']['collection']['fields'][number]

function ValueText({ value, f, field }: { value: unknown; f: Format; field?: Field }) {
  const t = useT()
  if (value === undefined || value === null || value === '')
    return <span className="ui-resolve-empty">{t('offline.resolve.empty')}</span>
  if (typeof value === 'number') return <>{f.number(value)}</>
  if (typeof value === 'boolean') return <>{value ? t('offline.resolve.yes') : t('offline.resolve.no')}</>
  if (Array.isArray(value)) return <>{t('offline.resolve.photos', { count: value.length })}</>
  if (field?.type === 'choice' && typeof value === 'string') return <>{optionLabel(field, value)}</>
  return <>{String(value)}</>
}

function mine(download: Download | undefined, item: OutboxItem): Record<string, unknown> {
  const context = item.action.context
  try {
    if (download) {
      const changes = parseUpdate(download.definition, context)
      return Object.fromEntries(Object.entries(changes).map(([k, v]) => [k, v === null ? undefined : v]))
    }
  } catch {}
  const { id: _id, ...rest } = context
  return rest
}

export function Resolve({
  runtime,
  item,
  download,
  name,
  onClose,
  onDiscardClosed,
}: {
  runtime: Runtime
  item: OutboxItem
  download: Download | undefined
  name: string
  onClose: () => void
  onDiscardClosed?: () => void
}) {
  const t = useT()
  const f = useFormat()
  const kind = kindOf(item)
  const reason = item.attention
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState(false)
  const needsLive = reason === 'schema' || reason === 'refused' || reason === 'outdated'
  const [live, setLive] = useState<Download | undefined>(needsLive ? undefined : download)
  const [deleted, setDeleted] = useState<Deleted | null | 'loading'>(reason === 'deleted' ? 'loading' : null)
  const [server, setServer] = useState<ServerRecord | 'loading' | 'failed'>('loading')
  const [check, setCheck] = useState(() => ({ runtime, item }))
  const latest = useRef({ runtime, item })
  useLayoutEffect(() => {
    latest.current = { runtime, item }
  })
  const itemChanged = useChanged(JSON.stringify([item.id, item.operation, versionOf(item)]))
  if (itemChanged) setCheck({ runtime, item })

  useEffect(() => {
    const { runtime, item } = check
    const reason = item.attention
    const kind = kindOf(item)
    const needsLive = reason === 'schema' || reason === 'refused' || reason === 'outdated'
    let current = true
    if (reason === 'conflict') {
      setServer('loading')
      readCurrent(runtime, item).then(
        (r) => current && setServer(r),
        (e) => {
          if (!current) return
          setServer('failed')
          setError(errorText(e))
        },
      )
    }
    if (needsLive && kind !== 'delete') {
      refreshDefinition(runtime, item).then(
        (d) => current && setLive(d),
        (e) => current && setError(errorText(e)),
      )
    }
    if (reason === 'deleted' && kind === 'update') {
      setDeleted('loading')
      findDeleted(runtime, item).then(
        (d) => current && setDeleted(d),
        (e) => {
          if (!current) return
          setDeleted(null)
          setError(errorText(e))
        },
      )
    }
    return () => {
      current = false
    }
  }, [check])

  const run = async (go: () => Promise<boolean>) => {
    setBusy(true)
    setError(null)
    try {
      await go()
      onClose()
    } catch (e) {
      if (e instanceof ApiError && (e.code === REVIEW_AGAIN || e.code === 'offline_copy_changed')) {
        setCheck({ ...latest.current })
      }
      setError(errorText(e))
      setBusy(false)
    }
  }

  const loaded = server !== 'loading' && server !== 'failed' ? server : null
  const current = loaded?.current ?? null
  const def = (live ?? download)?.definition
  const fields = def?.collection.fields ?? []
  const base = kind === 'update' ? (current ? fieldsOf(current) : (item.original ?? {})) : {}
  const changed = mine(live ?? download, item)
  const problems: Problem[] =
    needsLive && live ? problemsOf(live.definition, kind, item.action.context, base) : (item.problems ?? [])

  return (
    <section className="ui-resolve" aria-label={t('offline.resolve.title', { name })}>
      {error && <Notice tone="danger">{error}</Notice>}

      {reason === 'conflict' && !editing && (
        <>
          <Heading level={3} size="subsection">
            {kind === 'delete' ? t('offline.resolve.conflict.titleDelete') : t('offline.resolve.conflict.title')}
          </Heading>
          <p className="ui-resolve-note">{t('offline.resolve.conflict.intro')}</p>
          {server === 'loading' && <p className="ui-resolve-note">{t('offline.resolve.conflict.checking')}</p>}
          {current ? (
            <ConflictTable fields={fields} item={item} current={current} changed={changed} f={f} />
          ) : (
            loaded && (
              <p role="status" className="ui-resolve-text">
                {loaded.deleted ? t('offline.resolve.conflict.nowDeleted') : t('offline.resolve.conflict.unreadable')}
              </p>
            )
          )}
          <div className="ui-resolve-actions">
            {current && (
              <Button
                size="sm"
                variant="primary"
                disabled={busy}
                onClick={() => void run(() => reapply(runtime, item, current, seenOf(item)))}
              >
                {kind === 'delete' ? t('offline.resolve.deleteAnyway') : t('offline.resolve.reapply')}
              </Button>
            )}
            <Button
              size="sm"
              disabled={busy || server === 'loading'}
              onClick={() => void run(() => keepServer(runtime, item, seenOf(item)))}
            >
              {t('offline.resolve.keepServer')}
            </Button>
            {current && kind === 'update' && def && (
              <Button size="sm" disabled={busy} onClick={() => setEditing(true)}>
                {t('offline.resolve.edit')}
              </Button>
            )}
            <Button size="sm" variant="ghost" onClick={onClose}>
              {t('offline.resolve.close')}
            </Button>
          </div>
        </>
      )}

      {reason === 'deleted' && (
        <>
          <Heading level={3} size="subsection">
            {t('offline.resolve.deleted.title')}
          </Heading>
          <p className="ui-resolve-text">
            {kind === 'delete' ? t('offline.resolve.deleted.bodyDelete') : t('offline.resolve.deleted.body')}
          </p>
          {kind === 'update' && deleted === 'loading' && !error && (
            <p className="ui-resolve-note">{t('offline.resolve.deleted.checking')}</p>
          )}
          {kind === 'update' && deleted && deleted !== 'loading' && (
            <p className="ui-resolve-note">
              {deleted.by && deleted.at
                ? t('offline.resolve.deleted.byAt', { by: deleted.by, when: f.dateTime(deleted.at) })
                : deleted.at
                  ? t('offline.resolve.deleted.at', { when: f.dateTime(deleted.at) })
                  : null}
            </p>
          )}
          {kind === 'update' && deleted === null && !error && (
            <p className="ui-resolve-note">{t('offline.resolve.deleted.unknown')}</p>
          )}
          <ChangeSummary fields={fields} changed={changed} f={f} />
          <div className="ui-resolve-actions">
            {kind === 'update' && deleted && deleted !== 'loading' && (
              <Button
                size="sm"
                variant="primary"
                disabled={busy}
                onClick={() => void run(() => restoreAndApply(runtime, item, deleted, seenOf(item)))}
              >
                {t('offline.resolve.deleted.restore')}
              </Button>
            )}
            <Button size="sm" disabled={busy} onClick={() => void run(() => keepServer(runtime, item, seenOf(item)))}>
              {kind === 'delete' ? t('offline.resolve.deleted.clear') : t('offline.resolve.deleted.discard')}
            </Button>
            <Button size="sm" variant="ghost" onClick={onClose}>
              {t('offline.resolve.close')}
            </Button>
          </div>
        </>
      )}

      {needsLive && kind !== 'delete' && !editing && (
        <>
          <Heading level={3} size="subsection">
            {t('offline.resolve.schema.title')}
          </Heading>
          <p className="ui-resolve-note">{t('offline.resolve.schema.intro')}</p>
          {!live && !error && <p className="ui-resolve-note">{t('offline.resolve.schema.checking')}</p>}
          {problems.length > 0 && (
            <ul className="ui-resolve-problems">
              {problems.map((p) => (
                <li key={`${p.field}:${p.code}`}>
                  <strong>{p.field}</strong>{' '}
                  {errorText(new ApiError(422, p.code, { code: p.code, params: { field: p.field, ...p.params } }))}
                </li>
              ))}
            </ul>
          )}
          <ChangeSummary fields={fields} changed={kind === 'insert' ? stripId(item.action.context) : changed} f={f} />
          <div className="ui-resolve-actions">
            {live && (
              <Button size="sm" variant="primary" disabled={busy} onClick={() => setEditing(true)}>
                {t('offline.resolve.schema.fix')}
              </Button>
            )}
            <Button size="sm" variant="ghost" onClick={onClose}>
              {t('offline.resolve.close')}
            </Button>
          </div>
        </>
      )}

      {needsLive && kind === 'delete' && (
        <>
          <Heading level={3} size="subsection">
            {t('offline.resolve.refusedDelete.title')}
          </Heading>
          <p className="ui-resolve-note">{t('offline.resolve.refusedDelete.body')}</p>
          <div className="ui-resolve-actions">
            <Button size="sm" variant="ghost" onClick={onClose}>
              {t('offline.resolve.close')}
            </Button>
          </div>
        </>
      )}

      {editing && def && (live ?? download) && (
        <EditForm
          fields={fields}
          initial={kind === 'update' ? { ...base, ...changed } : stripId(item.action.context)}
          busy={busy || server === 'loading'}
          onCancel={() => setEditing(false)}
          onSave={(form, touched) =>
            void run(() =>
              saveEdit(
                runtime,
                item,
                form,
                (live ?? download) as Download,
                current,
                seenOf(item),
                kind === 'update' ? [...new Set([...Object.keys(changed), ...touched])] : undefined,
              ),
            )
          }
        />
      )}

      <div className="ui-resolve-actions">
        <DiscardButton
          runtime={runtime}
          item={item}
          name={name}
          onDone={onClose}
          onError={setError}
          onClosed={onDiscardClosed}
        />
      </div>
    </section>
  )
}

const stripId = (context: Record<string, unknown>) => {
  const { id: _id, ...rest } = context
  return rest
}

const as = (role: 'table' | 'rowgroup' | 'row' | 'columnheader' | 'rowheader' | 'cell') => ({ role })

function ConflictTable({
  fields,
  item,
  current,
  changed,
  f,
}: {
  fields: Field[]
  item: OutboxItem
  current: Record<string, unknown>
  changed: Record<string, unknown>
  f: Format
}) {
  const t = useT()
  const original = item.original ?? {}
  const server = fieldsOf(current)
  const del = kindOf(item) === 'delete'
  const shown = fields.filter((field) => field.name in changed || !same(original[field.name], server[field.name]))
  return (
    <table className="ui-resolve-table" {...as('table')}>
      <thead {...as('rowgroup')}>
        <tr {...as('row')}>
          <th scope="col" {...as('columnheader')}>
            {t('offline.resolve.column.field')}
          </th>
          <th scope="col" {...as('columnheader')}>
            {t('offline.resolve.column.original')}
          </th>
          <th scope="col" {...as('columnheader')}>
            {del ? t('offline.resolve.column.deleted') : t('offline.resolve.column.mine')}
          </th>
          <th scope="col" {...as('columnheader')}>
            {t('offline.resolve.column.server')}
          </th>
        </tr>
      </thead>
      <tbody {...as('rowgroup')}>
        {shown.map((field) => (
          <tr key={field.name} {...as('row')}>
            <th scope="row" {...as('rowheader')}>
              {field.name}
            </th>
            <td {...as('cell')} data-label={t('offline.resolve.column.original')}>
              <ValueText value={original[field.name]} f={f} field={field} />
            </td>
            <td
              {...as('cell')}
              data-label={del ? t('offline.resolve.column.deleted') : t('offline.resolve.column.mine')}
            >
              {del ? (
                <span className="ui-resolve-empty">{t('offline.resolve.deletedMark')}</span>
              ) : (
                <ValueText
                  value={field.name in changed ? changed[field.name] : original[field.name]}
                  f={f}
                  field={field}
                />
              )}
            </td>
            <td {...as('cell')} data-label={t('offline.resolve.column.server')}>
              <ValueText value={server[field.name]} f={f} field={field} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

const same = (a: unknown, b: unknown) => (a ?? '') === (b ?? '') || JSON.stringify(a) === JSON.stringify(b)

function ChangeSummary({ fields, changed, f }: { fields: Field[]; changed: Record<string, unknown>; f: Format }) {
  const t = useT()
  const names = Object.keys(changed)
  if (names.length === 0) return null
  const known = new Set(fields.map((x) => x.name))
  return (
    <dl className="ui-resolve-mine" aria-label={t('offline.resolve.mine')}>
      {names.map((n) => (
        <div key={n}>
          <dt>{n}</dt>
          <dd>
            <ValueText value={changed[n]} f={f} field={fields.find((x) => x.name === n)} />
            {!known.has(n) && <span className="ui-resolve-note"> {t('offline.resolve.notInApp')}</span>}
          </dd>
        </div>
      ))}
    </dl>
  )
}

function EditForm({
  fields,
  initial,
  busy,
  onSave,
  onCancel,
}: {
  fields: Field[]
  initial: Record<string, unknown>
  busy: boolean
  onSave: (form: Record<string, unknown>, touched: string[]) => void
  onCancel: () => void
}) {
  const t = useT()
  const uid = useId()
  const [edits, setEdits] = useState<Record<string, unknown>>({})
  const form = Object.fromEntries(
    fields
      .filter((x) => x.type !== 'attachment')
      .map((x) => [x.name, x.name in edits ? edits[x.name] : (initial[x.name] ?? (x.type === 'boolean' ? false : ''))]),
  )
  const set = (name: string, value: unknown) => setEdits((prev) => ({ ...prev, [name]: value }))
  return (
    <form
      className="ui-resolve-form"
      aria-label={t('offline.resolve.edit')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave(form, Object.keys(edits))
      }}
    >
      <Heading level={3} size="subsection">
        {t('offline.resolve.edit')}
      </Heading>
      {fields
        .filter((x) => x.type !== 'attachment')
        .map((x) =>
          x.type === 'boolean' ? (
            <Checkbox
              key={x.name}
              label={x.name}
              checked={form[x.name] === true}
              onChange={(e) => set(x.name, e.target.checked)}
            />
          ) : (
            <LabelledField key={x.name} id={`${uid}-${x.name}`} label={x.name}>
              {x.type === 'choice' ? (
                <Select value={String(form[x.name] ?? '')} onChange={(e) => set(x.name, e.target.value)}>
                  <option value="" />
                  {(x.options ?? []).map((o) => (
                    <option key={o} value={o}>
                      {optionLabel(x, o)}
                    </option>
                  ))}
                </Select>
              ) : (
                <Input
                  type={x.type === 'date' ? 'date' : x.type === 'time' ? 'time' : 'text'}
                  inputMode={x.type === 'number' ? 'decimal' : undefined}
                  value={String(form[x.name] ?? '')}
                  onChange={(e) => set(x.name, e.target.value)}
                />
              )}
            </LabelledField>
          ),
        )}
      {fields.some((x) => x.type === 'attachment') && (
        <p className="ui-resolve-note">{t('offline.resolve.photosKept')}</p>
      )}
      <div className="ui-resolve-actions">
        <Button size="sm" variant="primary" type="submit" disabled={busy}>
          {t('offline.resolve.save')}
        </Button>
        <Button size="sm" disabled={busy} onClick={onCancel}>
          {t('offline.resolve.cancel')}
        </Button>
      </div>
    </form>
  )
}
