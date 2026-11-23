// One record of an app: its fields, which a person who may change it edits, its history, and for a
// deleted one its restore. Edits go through the app's own update action with the revision the
// person saw (as the screen's buttons do), so someone else's change is a conflict and never a silent
// overwrite. On a conflict nothing the person typed is lost: their input stays in the form, the
// server's current values are shown beside it, and they choose to save theirs over them or to drop
// theirs. What the server decides is never second-guessed here.

import { useId, useState } from 'react'
import { ApiError, type AppField, type AppRecord, api, errorText, type RecordVersion } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { isLocation } from './capture'
import { toText } from './explorer'
import { LocationField } from './LocationInput'
import { PhotoStrip } from './PhotoInput'
import { photoIds, signatureId } from './photos'
import { HistoryList, optionLabel, shown, who } from './RecordParts'
import { SignatureField } from './SignaturePad'

const RECORD_CONFLICT = 'app_record_conflict'
// Shown after the name of a field that must have a value.
const REQUIRED_MARK = '*'

type History = { record: AppRecord; history: RecordVersion[]; truncated: boolean }
// The record as the server has it now, after a conflict: its revision and its fields.
type Current = { revision: number; data: Record<string, unknown> }

export function RecordDetail({
  base,
  appId,
  id,
  fields,
  updateAction,
  deleteAction,
  release,
  deleted,
  canChange,
  canRestore,
  onClose,
  onChanged,
}: {
  // …/spaces/{space}/apps/{app}
  base: string
  appId: number
  id: number
  fields: AppField[]
  // The app's actions that change and remove a record; without one the record is read-only.
  updateAction?: string
  deleteAction?: string
  // The live release the actions are named from.
  release?: number
  deleted: boolean
  canChange: boolean
  canRestore: boolean
  onClose: () => void
  // Something changed (an edit, a delete, a restore): the list and the app's screen are loaded again.
  onChanged: () => void
}) {
  const t = useT()
  const f = useFormat()
  const formId = useId()
  const loaded = useLoad(() => api<History>(`${base}/records/${id}/history`), [base, id])
  // What the person typed, by field name: only the fields they touched. The rest of the form is the
  // record as it is, so an edit never carries a stale value of a field it didn't change.
  const [edits, setEdits] = useState<Record<string, string>>({})
  const [conflict, setConflict] = useState<Current | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const record = loaded.data?.record
  const editable = canChange && !deleted && !!updateAction && !record?.deleted_at

  if (!record) {
    return (
      <div className="tt-record-detail">
        <button type="button" className="tt-button" onClick={onClose}>
          {t('apps.explorer.back')}
        </button>
        {loaded.error ? (
          <p role="alert" className="tt-error">
            {loaded.error}
          </p>
        ) : (
          <p className="tt-muted">{t('app.loading')}</p>
        )}
      </div>
    )
  }

  // What the form shows for a field: what was typed, else the record's value (the server's current
  // one once a conflict has been shown).
  const base0: Record<string, unknown> = conflict?.data ?? record.data
  const textOf = (name: string) => edits[name] ?? toText(base0[name])
  const touched = Object.keys(edits)
  const revision = conflict?.revision ?? record.revision

  const send = async (name: string, context: Record<string, unknown>) =>
    api(`${base}/actions`, {
      method: 'POST',
      body: {
        name,
        surfaceId: `app-${appId}`,
        sourceComponentId: 'explorer',
        timestamp: new Date().toISOString(),
        context,
        ...(release !== undefined ? { release } : {}),
        expectedRevision: revision,
      },
    })

  // The record as the server has it now, from the conflict's answer or, failing that, read again.
  const adoptCurrent = async (e: ApiError) => {
    const item = (e.body as { current?: Record<string, unknown> } | undefined)?.current
    if (item && typeof item._revision === 'number') {
      // The record's own fields: not its id, nor the keys the server adds (its revision, its choices' labels).
      const data = Object.fromEntries(Object.entries(item).filter(([k]) => k !== 'id' && !k.startsWith('_')))
      setConflict({ revision: item._revision, data })
      return
    }
    try {
      const now = await api<History>(`${base}/records/${id}/history`)
      setConflict({ revision: now.record.revision, data: now.record.data })
    } catch {
      // The conflict is what the person is told.
    }
  }

  const save = async () => {
    if (!updateAction || touched.length === 0) return
    setBusy(true)
    setNotice(null)
    try {
      await send(updateAction, { id, ...edits })
      setEdits({})
      setConflict(null)
      setError(null)
      setNotice(t('apps.explorer.saved'))
      loaded.reload()
      onChanged()
    } catch (e) {
      setError(errorText(e))
      if (e instanceof ApiError && e.code === RECORD_CONFLICT) await adoptCurrent(e)
    }
    setBusy(false)
  }

  const remove = async () => {
    if (!deleteAction) return
    setBusy(true)
    try {
      await send(deleteAction, { id })
      onChanged()
      onClose()
      return
    } catch (e) {
      setError(errorText(e))
      setConfirmDelete(false)
      if (e instanceof ApiError && e.code === RECORD_CONFLICT) await adoptCurrent(e)
    }
    setBusy(false)
  }

  const restore = async () => {
    setBusy(true)
    try {
      await api(`${base}/records/${id}/restore`, { method: 'POST', body: { expected_revision: record.revision } })
      onChanged()
      onClose()
      return
    } catch (e) {
      setError(errorText(e))
      loaded.reload()
    }
    setBusy(false)
  }

  const discard = () => {
    setEdits({})
    setConflict(null)
    setError(null)
    loaded.reload()
  }

  // The fields the person typed in where the server's value now differs: shown side by side.
  const clashes = conflict
    ? touched
        .filter((name) => edits[name] !== toText(conflict.data[name]))
        .map((name) => fields.find((x) => x.name === name))
    : []

  return (
    <div className="tt-record-detail">
      <div className="tt-record-head">
        <button type="button" className="tt-button" onClick={onClose}>
          {t('apps.explorer.back')}
        </button>
        <h3>{t('apps.records.record', { id: String(id) })}</h3>
        <span className="tt-muted tt-small">{t('apps.records.revision', { revision: String(revision) })}</span>
      </div>
      <p className="tt-muted tt-small">
        {record.deleted_at
          ? t('apps.records.deletedBy', { when: f.dateTime(record.deleted_at), name: who(record.deleted_by, t) })
          : t('apps.records.changed', { when: f.dateTime(record.updated_at), name: who(record.updated_by, t) })}
      </p>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {notice && (
        <p role="status" className="tt-muted tt-small">
          {notice}
        </p>
      )}
      {conflict && (
        <section className="tt-conflict" aria-label={t('apps.explorer.conflict.title')}>
          <p>
            <strong>{t('apps.explorer.conflict.title')}</strong>
          </p>
          <p className="tt-small">{t('apps.explorer.conflict.keep')}</p>
          {clashes.length > 0 && (
            <table className="tt-conflict-table">
              <thead>
                <tr>
                  <th scope="col">{t('apps.explorer.conflict.field')}</th>
                  <th scope="col">{t('apps.explorer.conflict.yours')}</th>
                  <th scope="col">{t('apps.explorer.conflict.current')}</th>
                </tr>
              </thead>
              <tbody>
                {clashes.map(
                  (field) =>
                    field && (
                      <tr key={field.name}>
                        <th scope="row">{field.name}</th>
                        <td>{shownText(edits[field.name], field, f, t) || t('apps.explorer.empty')}</td>
                        <td>{shown(conflict.data[field.name], field.type, f, t, field) || t('apps.explorer.empty')}</td>
                      </tr>
                    ),
                )}
              </tbody>
            </table>
          )}
          <div className="tt-record-actions">
            <button type="button" className="tt-button tt-primary" disabled={busy} onClick={save}>
              {t('apps.explorer.conflict.reapply')}
            </button>
            <button type="button" className="tt-button" disabled={busy} onClick={discard}>
              {t('apps.explorer.conflict.discard')}
            </button>
          </div>
        </section>
      )}
      <form
        className="tt-record-form"
        aria-label={t('apps.explorer.fields', { id: String(id) })}
        onSubmit={(e) => {
          e.preventDefault()
          if (!conflict) save()
        }}
      >
        {fields.map((field) => (
          <FieldRow
            key={field.name}
            idPrefix={formId}
            base={base}
            field={field}
            text={textOf(field.name)}
            raw={base0[field.name]}
            editable={editable}
            onChange={(v) => setEdits((cur) => ({ ...cur, [field.name]: v }))}
          />
        ))}
        {editable && !conflict && (
          <div className="tt-record-actions">
            <button type="submit" className="tt-button tt-primary" disabled={busy || touched.length === 0}>
              {t('apps.explorer.save')}
            </button>
            {deleteAction &&
              (confirmDelete ? (
                <>
                  <button type="button" className="tt-button tt-danger" disabled={busy} onClick={remove}>
                    {t('apps.explorer.deleteConfirm')}
                  </button>
                  <button type="button" className="tt-button" onClick={() => setConfirmDelete(false)}>
                    {t('apps.explorer.cancel')}
                  </button>
                </>
              ) : (
                <button type="button" className="tt-button" disabled={busy} onClick={() => setConfirmDelete(true)}>
                  {t('apps.explorer.delete')}
                </button>
              ))}
          </div>
        )}
      </form>
      {(deleted || record.deleted_at) && canRestore && (
        <div className="tt-record-actions">
          <button
            type="button"
            className="tt-button tt-primary"
            disabled={busy}
            aria-label={t('apps.records.restoreNamed', { id: String(id) })}
            onClick={restore}
          >
            {t('apps.records.restore')}
          </button>
        </div>
      )}
      <h4>{t('apps.records.history')}</h4>
      {loaded.data && (
        <HistoryList id={id} versions={loaded.data.history} truncated={loaded.data.truncated} fields={fields} />
      )}
    </div>
  )
}

// A typed value in the person's language, for the conflict's comparison.
function shownText(
  text: string | undefined,
  field: AppField,
  f: ReturnType<typeof useFormat>,
  t: ReturnType<typeof useT>,
) {
  if (text === undefined || text === '') return ''
  if (field.type === 'number' && !Number.isNaN(Number(text))) return f.number(Number(text))
  if (field.type === 'boolean') return t(text === 'true' ? 'apps.records.yes' : 'apps.records.no')
  return shown(text, field.type, f, t, field)
}

function FieldRow({
  idPrefix,
  base,
  field,
  text,
  raw,
  editable,
  onChange,
}: {
  idPrefix: string
  base: string
  field: AppField
  text: string
  // The value as the record holds it (an object for a location).
  raw: unknown
  editable: boolean
  onChange: (v: string) => void
}) {
  const t = useT()
  const f = useFormat()
  const id = `${idPrefix}-${field.name}`
  const label = (
    <span className="tt-record-field-label">
      <label htmlFor={id}>{field.name}</label>
      {field.required ? <span aria-hidden="true">{REQUIRED_MARK}</span> : null}
    </span>
  )
  const known = ['string', 'number', 'boolean', 'date', 'time', 'choice', 'barcode'].includes(field.type)
  // A field this version doesn't know how to edit (captured evidence such as media, a location or a
  // signature) is shown as it was recorded, never as an input.
  if (field.type === 'location' || field.type === 'signature') {
    // Captured evidence: shown as recorded, never an input. The server refuses a change to it (unless
    // the field says it is editable), so no control is offered.
    return (
      <div className="tt-record-field">
        {field.type === 'location' ? (
          <LocationField
            field={field.name}
            label={field.name}
            value={isLocation(raw) ? raw : undefined}
            onChange={() => {}}
            readOnly
          />
        ) : (
          <SignatureField
            field={field.name}
            label={field.name}
            files={{ base, uploads: false }}
            value={signatureId(raw)}
            onChange={() => {}}
            readOnly
          />
        )}
        {!field.editable && <span className="tt-muted tt-small">{t('apps.explorer.evidence')}</span>}
      </div>
    )
  }
  if (!editable || !known) {
    // Photos are shown as thumbnails, through the viewer's own session.
    const photos = field.type === 'attachment' ? photoIds(text.split(',')) : []
    if (photos.length > 0) {
      return (
        <div className="tt-record-field">
          <span className="tt-record-field-name">{field.name}</span>
          <PhotoStrip base={base} ids={photos} label={field.name} />
        </div>
      )
    }
    return (
      <div className="tt-record-field">
        <span className="tt-record-field-name">{field.name}</span>
        <span className="tt-record-data">
          {(field.type === 'number' && text !== '' ? f.number(Number(text)) : shown(text, field.type, f, t, field)) ||
            t('apps.explorer.empty')}
        </span>
      </div>
    )
  }
  switch (field.type) {
    case 'boolean':
    case 'choice': {
      const options =
        field.type === 'boolean'
          ? [
              { value: 'true', label: t('apps.records.yes') },
              { value: 'false', label: t('apps.records.no') },
            ]
          : (field.options ?? []).map((o) => ({ value: o, label: optionLabel(field, o) }))
      // A stored choice the options no longer list stays selectable as it is.
      if (text !== '' && !options.some((o) => o.value === text)) options.push({ value: text, label: text })
      return (
        <div className="tt-record-field">
          {label}
          <select id={id} value={text} onChange={(e) => onChange(e.target.value)}>
            <option value="">{t('apps.explorer.unset')}</option>
            {options.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </div>
      )
    }
    default:
      return (
        <div className="tt-record-field">
          {label}
          <input
            id={id}
            type={field.type === 'string' || field.type === 'barcode' ? 'text' : field.type}
            step={field.type === 'number' ? 'any' : undefined}
            inputMode={field.type === 'number' ? 'decimal' : undefined}
            maxLength={field.type === 'string' || field.type === 'barcode' ? field.max_length : undefined}
            value={text}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      )
  }
}
