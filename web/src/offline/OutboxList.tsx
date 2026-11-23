// The records made on this device, each with its own state: a local draft, queued, syncing,
// synced (the server acknowledged it) or needing attention, and its photos' state apart, since a
// record's photos can still be on their way when the record is not. The controls to recover are
// here: try again now, keep as a draft, send, discard.

import { useState } from 'react'
import { ApiError, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { ConfirmButton } from '../settings/parts'
import { Resolve } from './Resolve'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem } from './store'
import { RESOLVE, type SyncStatus } from './sync'

// What a record is called in the list: the app, and its first filled-in value (for a change to a
// record, the one it is about: as it was downloaded, else what the change sets).
function summaryOf(item: OutboxItem): string | undefined {
  if (kindOf(item) === 'insert') {
    for (const [name, v] of Object.entries(item.action.context)) {
      if (item.attachments.some((a) => a.field === name)) continue
      if (typeof v === 'string' && v.trim()) return v.trim().slice(0, 60)
      if (typeof v === 'number') return String(v)
    }
    return undefined
  }
  // A change to a record is named by the record: its first text as it was downloaded, else a number.
  const values = [...Object.entries(item.original ?? {}), ...Object.entries(item.action.context)].filter(
    ([name]) => name !== 'id',
  )
  const text = values.find(([, v]) => typeof v === 'string' && v.trim())
  if (text) return (text[1] as string).trim().slice(0, 60)
  const number = values.find(([, v]) => typeof v === 'number')
  return number ? String(number[1]) : undefined
}

export function SyncNotice({ status, waiting }: { status: SyncStatus; waiting: number }) {
  const t = useT()
  if (waiting === 0 && !status.paused) return null
  return (
    <p className="tt-muted tt-small" role="status">
      {status.paused === 'session'
        ? t('offline.sync.paused')
        : status.paused === 'account'
          ? t('offline.sync.account')
          : !status.online
            ? t('offline.sync.offline')
            : status.running
              ? t('offline.sync.running')
              : null}
    </p>
  )
}

export function OutboxList({
  runtime,
  items,
  status,
  apps,
  downloads = [],
}: {
  runtime: Runtime
  items: OutboxItem[]
  status: SyncStatus
  // App names by scope key, for the label of each record.
  apps: Record<string, string>
  // The downloaded copies, which a change needing attention is compared against.
  downloads?: Download[]
}) {
  const t = useT()
  const f = useFormat()
  const [resolving, setResolving] = useState<string | null>(null)
  const synced = items.filter((i) => i.state === 'synced')
  const waiting = items.filter((i) => i.state !== 'synced')

  return (
    <section className="tt-outbox" aria-label={t('offline.queue.title')}>
      <div className="tt-pane-head">
        <h2>{t('offline.queue.title')}</h2>
        {waiting.length > 0 && (
          <span className="tt-muted tt-small">{t('offline.queue.waiting', { count: waiting.length })}</span>
        )}
        {waiting.length > 0 && (
          <button type="button" className="tt-button" onClick={() => void runtime.engine.retry()}>
            {t('offline.sync.now')}
          </button>
        )}
      </div>
      <SyncNotice status={status} waiting={waiting.length} />
      {items.length === 0 && <p className="tt-muted">{t('offline.queue.none')}</p>}
      <ul className="tt-outbox-list">
        {items.map((item) => {
          const app = apps[item.scopeKey] ?? ''
          const summary = summaryOf(item)
          const kind = kindOf(item)
          const name =
            kind === 'update'
              ? summary
                ? t('offline.queue.itemChange', { app, summary })
                : t('offline.queue.itemChangeAny', { app })
              : kind === 'delete'
                ? summary
                  ? t('offline.queue.itemDelete', { app, summary })
                  : t('offline.queue.itemDeleteAny', { app })
                : summary
                  ? t('offline.queue.item', { app, summary })
                  : t('offline.queue.itemNew', { app })
          const settle = item.state === 'attention' && item.attention !== undefined && RESOLVE.has(item.attention)
          // Offered for what can be looked at and settled: a conflict, a deleted record, what no
          // longer fits, and a refusal of the server's.
          const reviewable =
            item.state === 'attention' &&
            (settle || item.attention === 'refused' || item.attention === 'outdated') &&
            !(kind === 'insert' && (item.attention === 'conflict' || item.attention === 'deleted'))
          const uploaded = item.attachments.filter((a) => a.state === 'uploaded' && a.serverId).length
          return (
            <li key={item.id} className="tt-outbox-item" data-state={item.state}>
              <span className="tt-outbox-name">{name}</span>
              <span className="tt-stage-label" data-state={item.state}>
                {t(`offline.state.${item.state}`)}
              </span>
              {item.attachments.length > 0 && (
                <span className="tt-muted tt-small">
                  {t('offline.queue.photos', { uploaded, total: item.attachments.length })}
                </span>
              )}
              {item.state === 'synced' && <span className="tt-muted tt-small">{t('offline.queue.syncedNote')}</span>}
              {item.state !== 'synced' && kind !== 'insert' && (
                <span className="tt-muted tt-small">{t('offline.queue.pendingNote')}</span>
              )}
              {item.state === 'attention' && item.attention && (
                <span role="alert" className="tt-error tt-small">
                  {t(`offline.attention.${item.attention}`, {
                    error: item.error
                      ? errorText(
                          new ApiError(item.error.status ?? 0, item.error.message, {
                            code: item.error.code,
                            params: item.error.params,
                          }),
                        )
                      : '',
                    count: item.attempts,
                  })}
                </span>
              )}
              {item.state === 'queued' && item.attempts > 0 && (
                <span className="tt-muted tt-small">
                  {t('offline.queue.attempts', { count: item.attempts })}
                  {item.nextAttemptAt ? ` ${t('offline.queue.next', { when: f.relative(item.nextAttemptAt) })}` : ''}
                </span>
              )}
              <span className="tt-row-actions">
                {reviewable && (
                  <button
                    type="button"
                    className="tt-button tt-primary"
                    aria-label={t('offline.queue.reviewNamed', { name })}
                    aria-expanded={resolving === item.id}
                    onClick={() => setResolving(resolving === item.id ? null : item.id)}
                  >
                    {t('offline.queue.review')}
                  </button>
                )}
                {(item.state === 'queued' || (item.state === 'attention' && !settle)) && (
                  <button
                    type="button"
                    className="tt-button"
                    aria-label={t('offline.queue.retryNamed', { name })}
                    onClick={() => void runtime.engine.retry(item.id)}
                  >
                    {t('offline.queue.retry')}
                  </button>
                )}
                {(item.state === 'queued' || item.state === 'attention') && (
                  <button
                    type="button"
                    className="tt-button"
                    aria-label={t('offline.queue.holdNamed', { name })}
                    onClick={() => void runtime.engine.hold(item.id)}
                  >
                    {t('offline.queue.hold')}
                  </button>
                )}
                {item.state === 'draft' && (
                  <button
                    type="button"
                    className="tt-button tt-primary"
                    aria-label={t('offline.queue.sendNamed', { name })}
                    onClick={() => void runtime.engine.release(item.id)}
                  >
                    {t('offline.queue.send')}
                  </button>
                )}
                {item.state !== 'synced' && item.state !== 'syncing' && (
                  <ConfirmButton
                    label={t('offline.queue.discard')}
                    name={name}
                    question={t('offline.queue.discardQuestion')}
                    confirm={t('offline.queue.discardConfirm')}
                    onConfirm={() => void runtime.store.removeItem(item.id)}
                  />
                )}
              </span>
              {resolving === item.id && reviewable && (
                <Resolve
                  runtime={runtime}
                  item={item}
                  download={downloads.find((d) => d.key === item.scopeKey)}
                  name={name}
                  onClose={() => setResolving(null)}
                />
              )}
            </li>
          )
        })}
      </ul>
      {synced.length > 0 && (
        <button
          type="button"
          className="tt-button"
          onClick={() => void Promise.all(synced.map((i) => runtime.store.removeItem(i.id)))}
        >
          {t('offline.queue.clearSynced')}
        </button>
      )}
    </section>
  )
}
