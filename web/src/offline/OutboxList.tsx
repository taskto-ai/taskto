// The records made on this device, each with its own state: a local draft, queued, syncing,
// synced (the server acknowledged it) or needing attention, and its photos' state apart, since a
// record's photos can still be on their way when the record is not. The controls to recover are
// here: try again now, keep as a draft, send, discard.

import { ApiError, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { ConfirmButton } from '../settings/parts'
import type { Runtime } from './runtime'
import type { OutboxItem } from './store'
import type { SyncStatus } from './sync'

// What a record is called in the list: the app, and its first filled-in value.
function summaryOf(item: OutboxItem): string | undefined {
  for (const [name, v] of Object.entries(item.action.context)) {
    if (item.attachments.some((a) => a.field === name)) continue
    if (typeof v === 'string' && v.trim()) return v.trim().slice(0, 60)
    if (typeof v === 'number') return String(v)
  }
  return undefined
}

export function SyncNotice({ status, waiting }: { status: SyncStatus; waiting: number }) {
  const t = useT()
  if (waiting === 0 && !status.paused) return null
  return (
    <p className="tt-muted tt-small" role="status">
      {status.paused === 'session'
        ? t('offline.sync.paused')
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
}: {
  runtime: Runtime
  items: OutboxItem[]
  status: SyncStatus
  // App names by scope key, for the label of each record.
  apps: Record<string, string>
}) {
  const t = useT()
  const f = useFormat()
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
          const name = summary ? t('offline.queue.item', { app, summary }) : t('offline.queue.itemNew', { app })
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
                {(item.state === 'queued' || item.state === 'attention') && (
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
