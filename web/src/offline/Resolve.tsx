// The place a person settles a change that needs them (docs/offline.md, "Conflicts and what
// doesn't fit"): a conflict shows what they changed beside the record as the server has it now,
// a record deleted meanwhile says so, and what no longer fits the app can be fixed. Every choice
// is explicit; closing it changes nothing, and nothing is overwritten by being looked at.

import { useEffect, useId, useState } from 'react'
import { ApiError, errorText } from '../api'
import { type Format, useFormat, useT } from '../i18n'
import { ConfirmButton } from '../settings/parts'
import {
  type Deleted,
  findDeleted,
  keepServer,
  problemsOf,
  reapply,
  refreshDefinition,
  restoreAndApply,
  saveEdit,
} from './choices'
import { fieldsOf, parseUpdate } from './local'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem, type Problem } from './store'

type Field = Download['definition']['collection']['fields'][number]

// A value as it is shown: a number in the person's format, yes or no, the text as it is.
function ValueText({ value, f }: { value: unknown; f: Format }) {
  const t = useT()
  if (value === undefined || value === null || value === '')
    return <span className="tt-muted">{t('offline.resolve.empty')}</span>
  if (typeof value === 'number') return <>{f.number(value)}</>
  if (typeof value === 'boolean') return <>{value ? t('offline.resolve.yes') : t('offline.resolve.no')}</>
  if (Array.isArray(value)) return <>{t('offline.resolve.photos', { count: value.length })}</>
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

  // Asked once, when it opens: the app as it is now, or the deletion's revision.
  // biome-ignore lint/correctness/useExhaustiveDependencies: for this item as it was shown
  useEffect(() => {
    let current = true
    if (needsLive && kind !== 'delete') {
      refreshDefinition(runtime, item).then(
        (d) => current && setLive(d),
        (e) => current && setError(errorText(e)),
      )
    }
    if (reason === 'deleted' && kind === 'update') {
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
  }, [item.id, item.operation])

  const run = async (go: () => Promise<boolean>) => {
    setBusy(true)
    setError(null)
    try {
      // false: another tab moved the item on, and what was chosen was not applied.
      await go()
      onClose()
    } catch (e) {
      setError(errorText(e))
      setBusy(false)
    }
  }

  const current = item.conflict?.current ?? null
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
          {current ? (
            <ConflictTable fields={fields} item={item} current={current} changed={changed} f={f} />
          ) : (
            <p role="status">{t('offline.resolve.conflict.unreadable')}</p>
          )}
          <p className="tt-row-actions">
            {current && (
              <button
                type="button"
                className="tt-button tt-primary"
                disabled={busy}
                onClick={() => void run(() => reapply(runtime, item))}
              >
                {kind === 'delete' ? t('offline.resolve.deleteAnyway') : t('offline.resolve.reapply')}
              </button>
            )}
            <button
              type="button"
              className="tt-button"
              disabled={busy}
              onClick={() => void run(() => keepServer(runtime, item))}
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
                onClick={() => void run(() => restoreAndApply(runtime, item, deleted))}
              >
                {t('offline.resolve.deleted.restore')}
              </button>
            )}
            <button
              type="button"
              className="tt-button"
              disabled={busy}
              onClick={() => void run(() => keepServer(runtime, item))}
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
          initial={kind === 'update' ? { ...base, ...changed } : stripId(item.action.context)}
          busy={busy}
          onCancel={() => setEditing(false)}
          onSave={(form) => void run(() => saveEdit(runtime, item, form, (live ?? download) as Download))}
        />
      )}

      <p className="tt-row-actions">
        <ConfirmButton
          label={t('offline.queue.discard')}
          name={name}
          question={t('offline.queue.discardQuestion')}
          confirm={t('offline.queue.discardConfirm')}
          onConfirm={() => {
            void runtime.store.removeItem(item.id)
            onClose()
          }}
        />
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
              <ValueText value={original[field.name]} f={f} />
            </td>
            <td>
              {del ? (
                <span className="tt-muted">{t('offline.resolve.deletedMark')}</span>
              ) : (
                <ValueText value={field.name in changed ? changed[field.name] : original[field.name]} f={f} />
              )}
            </td>
            <td>
              <ValueText value={server[field.name]} f={f} />
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
            <ValueText value={changed[n]} f={f} />
            {!known.has(n) && <span className="tt-muted tt-small"> {t('offline.resolve.notInApp')}</span>}
          </dd>
        </div>
      ))}
    </dl>
  )
}

// The record's fields as inputs, by type: what the person edits to retry. Photos stay as they are.
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
  onSave: (form: Record<string, unknown>) => void
  onCancel: () => void
}) {
  const t = useT()
  const uid = useId()
  const [form, setForm] = useState<Record<string, unknown>>(() =>
    Object.fromEntries(
      fields
        .filter((x) => x.type !== 'attachment')
        .map((x) => [x.name, initial[x.name] ?? (x.type === 'boolean' ? false : '')]),
    ),
  )
  const set = (name: string, value: unknown) => setForm((prev) => ({ ...prev, [name]: value }))
  return (
    <form
      className="tt-resolve-form"
      aria-label={t('offline.resolve.edit')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave(form)
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
                    {o}
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
