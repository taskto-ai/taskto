// The place a person settles a change that needs them (docs/offline.md, "Conflicts and what
// doesn't fit"): a conflict shows what they changed beside the record as the server has it now,
// a record deleted meanwhile says so, and what no longer fits the app can be fixed. Every choice
// is explicit; closing it changes nothing, and nothing is overwritten by being looked at.

import { useEffect, useId, useState } from 'react'
import { ApiError, errorText } from '../api'
import { optionLabel } from '../apps/RecordParts'
import { type Format, useFormat, useT } from '../i18n'
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
import { fieldsOf, parseUpdate } from './local'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem, type Problem, seenOf, versionOf } from './store'

type Field = Download['definition']['collection']['fields'][number]

// A value as it is shown: a number in the person's format, yes or no, a choice by its label, the text as it is.
function ValueText({ value, f, field }: { value: unknown; f: Format; field?: Field }) {
  const t = useT()
  if (value === undefined || value === null || value === '')
    return <span className="tt-muted">{t('offline.resolve.empty')}</span>
  if (typeof value === 'number') return <>{f.number(value)}</>
  if (typeof value === 'boolean') return <>{value ? t('offline.resolve.yes') : t('offline.resolve.no')}</>
  if (Array.isArray(value)) return <>{t('offline.resolve.photos', { count: value.length })}</>
  if (field?.type === 'choice' && typeof value === 'string') return <>{optionLabel(field, value)}</>
  return <>{String(value)}</>
}

// What the person's change sets, as the values it gives (an update's cleared field is none).
function mine(download: Download | undefined, item: OutboxItem): Record<string, unknown> {
  const context = item.action.context
  try {
    if (download) {
      const changes = parseUpdate(download.definition, context)
      return Object.fromEntries(Object.entries(changes).map(([k, v]) => [k, v === null ? undefined : v]))
    }
  } catch {
    // Shown as it was entered.
  }
  const { id: _id, ...rest } = context
  return rest
}

export function Resolve({
  runtime,
  item,
  download,
  name,
  onClose,
}: {
  runtime: Runtime
  item: OutboxItem
  download: Download | undefined
  name: string
  onClose: () => void
}) {
  const t = useT()
  const f = useFormat()
  const kind = kindOf(item)
  const reason = item.attention
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState(false)
  // The app as it is live now, for what no longer fits it (downloaded again), else the copy held.
  const needsLive = reason === 'schema' || reason === 'refused' || reason === 'outdated'
  const [live, setLive] = useState<Download | undefined>(needsLive ? undefined : download)
  const [deleted, setDeleted] = useState<Deleted | null | 'loading'>(reason === 'deleted' ? 'loading' : null)
  // The record as the server has it now, read when this opens (and again when the item changes, or
  // a choice found the record had moved on): what is shown beside the person's change is never
  // what an earlier answer was cached with.
  const [server, setServer] = useState<ServerRecord | 'loading' | 'failed'>('loading')
  const [again, setAgain] = useState(0)

  // Asked when it opens, and again for another version of the item or after a review again: the
  // app as it is now, the record, or the deletion's revision.
  // biome-ignore lint/correctness/useExhaustiveDependencies: for this item as it was shown
  useEffect(() => {
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
  }, [item.id, item.operation, versionOf(item), again])

  const run = async (go: () => Promise<boolean>) => {
    setBusy(true)
    setError(null)
    try {
      // A choice is applied only to the item as it was shown: if another tab moved it on, or the
      // record moved on, it says so (and what is shown is read again), and nothing is applied.
      await go()
      onClose()
    } catch (e) {
      // The record moved on, or the copy of the app was replaced while the server was read: what is
      // shown is read again.
      if (e instanceof ApiError && (e.code === REVIEW_AGAIN || e.code === 'offline_copy_changed')) {
        setAgain((n) => n + 1)
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
    <section className="tt-resolve" aria-label={t('offline.resolve.title', { name })}>
      {error && (
        <p role="alert" className="tt-error tt-small">
          {error}
        </p>
      )}

      {reason === 'conflict' && !editing && (
        <>
          <h3>{kind === 'delete' ? t('offline.resolve.conflict.titleDelete') : t('offline.resolve.conflict.title')}</h3>
          <p className="tt-muted tt-small">{t('offline.resolve.conflict.intro')}</p>
          {server === 'loading' && <p className="tt-muted tt-small">{t('offline.resolve.conflict.checking')}</p>}
          {current ? (
            <ConflictTable fields={fields} item={item} current={current} changed={changed} f={f} />
          ) : (
            loaded && (
              <p role="status">
                {loaded.deleted ? t('offline.resolve.conflict.nowDeleted') : t('offline.resolve.conflict.unreadable')}
              </p>
            )
          )}
          <p className="tt-row-actions">
            {current && (
              <button
                type="button"
                className="tt-button tt-primary"
                disabled={busy}
                onClick={() => void run(() => reapply(runtime, item, current, seenOf(item)))}
              >
                {kind === 'delete' ? t('offline.resolve.deleteAnyway') : t('offline.resolve.reapply')}
              </button>
            )}
            <button
              type="button"
              className="tt-button"
              disabled={busy || server === 'loading'}
              onClick={() => void run(() => keepServer(runtime, item, seenOf(item)))}
            >
              {t('offline.resolve.keepServer')}
            </button>
            {current && kind === 'update' && def && (
              <button type="button" className="tt-button" disabled={busy} onClick={() => setEditing(true)}>
                {t('offline.resolve.edit')}
              </button>
            )}
            <button type="button" className="tt-button" onClick={onClose}>
              {t('offline.resolve.close')}
            </button>
          </p>
        </>
      )}

      {reason === 'deleted' && (
        <>
          <h3>{t('offline.resolve.deleted.title')}</h3>
          <p>{kind === 'delete' ? t('offline.resolve.deleted.bodyDelete') : t('offline.resolve.deleted.body')}</p>
          {kind === 'update' && deleted === 'loading' && !error && (
            <p className="tt-muted tt-small">{t('offline.resolve.deleted.checking')}</p>
          )}
          {kind === 'update' && deleted && deleted !== 'loading' && (
            <p className="tt-muted tt-small">
              {deleted.by && deleted.at
                ? t('offline.resolve.deleted.byAt', { by: deleted.by, when: f.dateTime(deleted.at) })
                : deleted.at
                  ? t('offline.resolve.deleted.at', { when: f.dateTime(deleted.at) })
                  : null}
            </p>
          )}
          {kind === 'update' && deleted === null && !error && (
            <p className="tt-muted tt-small">{t('offline.resolve.deleted.unknown')}</p>
          )}
          <ChangeSummary fields={fields} changed={changed} f={f} />
          <p className="tt-row-actions">
            {kind === 'update' && deleted && deleted !== 'loading' && (
              <button
                type="button"
                className="tt-button tt-primary"
                disabled={busy}
                onClick={() => void run(() => restoreAndApply(runtime, item, deleted, seenOf(item)))}
              >
                {t('offline.resolve.deleted.restore')}
              </button>
            )}
            <button
              type="button"
              className="tt-button"
              disabled={busy}
              onClick={() => void run(() => keepServer(runtime, item, seenOf(item)))}
            >
              {kind === 'delete' ? t('offline.resolve.deleted.clear') : t('offline.resolve.deleted.discard')}
            </button>
            <button type="button" className="tt-button" onClick={onClose}>
              {t('offline.resolve.close')}
            </button>
          </p>
        </>
      )}

      {needsLive && kind !== 'delete' && !editing && (
        <>
          <h3>{t('offline.resolve.schema.title')}</h3>
          <p className="tt-muted tt-small">{t('offline.resolve.schema.intro')}</p>
          {!live && !error && <p className="tt-muted tt-small">{t('offline.resolve.schema.checking')}</p>}
          {problems.length > 0 && (
            <ul className="tt-resolve-problems">
              {problems.map((p) => (
                <li key={`${p.field}:${p.code}`}>
                  <strong>{p.field}</strong>{' '}
                  {errorText(new ApiError(422, p.code, { code: p.code, params: { field: p.field, ...p.params } }))}
                </li>
              ))}
            </ul>
          )}
          <ChangeSummary fields={fields} changed={kind === 'insert' ? stripId(item.action.context) : changed} f={f} />
          <p className="tt-row-actions">
            {live && (
              <button type="button" className="tt-button tt-primary" disabled={busy} onClick={() => setEditing(true)}>
                {t('offline.resolve.schema.fix')}
              </button>
            )}
            <button type="button" className="tt-button" onClick={onClose}>
              {t('offline.resolve.close')}
            </button>
          </p>
        </>
      )}

      {needsLive && kind === 'delete' && (
        <>
          <h3>{t('offline.resolve.refusedDelete.title')}</h3>
          <p className="tt-muted tt-small">{t('offline.resolve.refusedDelete.body')}</p>
          <p className="tt-row-actions">
            <button type="button" className="tt-button" onClick={onClose}>
              {t('offline.resolve.close')}
            </button>
          </p>
        </>
      )}

      {editing && def && (live ?? download) && (
        <EditForm
          fields={fields}
          // What every field holds now: the record as the server has it (read again when it moved
          // on) with the person's own change on top. A field they didn't touch follows it.
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
                // An update applies only what is the person's: their change, and what they edited.
                kind === 'update' ? [...new Set([...Object.keys(changed), ...touched])] : undefined,
              ),
            )
          }
        />
      )}

      <p className="tt-row-actions">
        <DiscardButton runtime={runtime} item={item} name={name} onDone={onClose} onError={setError} />
      </p>
    </section>
  )
}

const stripId = (context: Record<string, unknown>) => {
  const { id: _id, ...rest } = context
  return rest
}

// What the person changed, field by field, beside what was there when they started and what the
// server has now.
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
    <table className="tt-table tt-resolve-table">
      <thead>
        <tr>
          <th scope="col">{t('offline.resolve.column.field')}</th>
          <th scope="col">{t('offline.resolve.column.original')}</th>
          <th scope="col">{del ? t('offline.resolve.column.deleted') : t('offline.resolve.column.mine')}</th>
          <th scope="col">{t('offline.resolve.column.server')}</th>
        </tr>
      </thead>
      <tbody>
        {shown.map((field) => (
          <tr key={field.name}>
            <th scope="row">{field.name}</th>
            <td>
              <ValueText value={original[field.name]} f={f} field={field} />
            </td>
            <td>
              {del ? (
                <span className="tt-muted">{t('offline.resolve.deletedMark')}</span>
              ) : (
                <ValueText
                  value={field.name in changed ? changed[field.name] : original[field.name]}
                  f={f}
                  field={field}
                />
              )}
            </td>
            <td>
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
    <dl className="tt-resolve-mine" aria-label={t('offline.resolve.mine')}>
      {names.map((n) => (
        <div key={n}>
          <dt>{n}</dt>
          <dd>
            <ValueText value={changed[n]} f={f} field={fields.find((x) => x.name === n)} />
            {!known.has(n) && <span className="tt-muted tt-small"> {t('offline.resolve.notInApp')}</span>}
          </dd>
        </div>
      ))}
    </dl>
  )
}

// The record's fields as inputs, by type: what the person edits to retry. Photos stay as they are.
// A field shows what the person typed in it, and until they type, what `initial` says now: the form
// remembers only the fields that were edited (`onSave` says which), so a record read again while it
// is open comes with its new values in every other field, and only what the person changed is applied.
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
      className="tt-resolve-form"
      aria-label={t('offline.resolve.edit')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave(form, Object.keys(edits))
      }}
    >
      <h3>{t('offline.resolve.edit')}</h3>
      {fields
        .filter((x) => x.type !== 'attachment')
        .map((x) => (
          <div key={x.name} className="tt-field">
            <label htmlFor={`${uid}-${x.name}`}>{x.name}</label>
            {x.type === 'boolean' ? (
              <input
                id={`${uid}-${x.name}`}
                type="checkbox"
                checked={form[x.name] === true}
                onChange={(e) => set(x.name, e.target.checked)}
              />
            ) : x.type === 'choice' ? (
              <select
                id={`${uid}-${x.name}`}
                value={String(form[x.name] ?? '')}
                onChange={(e) => set(x.name, e.target.value)}
              >
                <option value="" />
                {(x.options ?? []).map((o) => (
                  <option key={o} value={o}>
                    {optionLabel(x, o)}
                  </option>
                ))}
              </select>
            ) : (
              <input
                id={`${uid}-${x.name}`}
                type={x.type === 'date' ? 'date' : x.type === 'time' ? 'time' : 'text'}
                inputMode={x.type === 'number' ? 'decimal' : undefined}
                value={String(form[x.name] ?? '')}
                onChange={(e) => set(x.name, e.target.value)}
              />
            )}
          </div>
        ))}
      {fields.some((x) => x.type === 'attachment') && (
        <p className="tt-muted tt-small">{t('offline.resolve.photosKept')}</p>
      )}
      <p className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {t('offline.resolve.save')}
        </button>
        <button type="button" className="tt-button" disabled={busy} onClick={onCancel}>
          {t('offline.resolve.cancel')}
        </button>
      </p>
    </form>
  )
}
