// The records made on this device, each with its own state: a local draft, queued, syncing,
// synced (the server acknowledged it) or needing attention, and its photos' state apart, since a
// record's photos can still be on their way when the record is not. The controls to recover are
// here: try again now, keep as a draft, send, discard. They are the offline writer's (writer.ts): in
// a tab that isn't, the queue is shown and nothing here changes it.

import { useState } from 'react'
import { ApiError, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Row, Rows } from '../ui/Rows'
import { DiscardButton } from './DiscardButton'
import { useWriter } from './hooks'
import { Resolve } from './Resolve'
import type { Runtime } from './runtime'
import { type Download, kindOf, NotWriter, type OutboxItem } from './store'
import { RESOLVE, type SyncStatus, waitsForEarlier } from './sync'
import './OutboxList.css'

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
    <p className="ui-outbox-note" role="status">
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
  const [notice, setNotice] = useState<string | null>(null)
  const writer = useWriter(runtime)
  // What a person's press does to the queue: a refusal (this tab lost the right meanwhile) is said.
  const act = (go: () => Promise<unknown>) =>
    void go().then(
      () => setNotice(null),
      (e) => setNotice(e instanceof NotWriter ? t('errors.offline_not_writer') : errorText(e)),
    )
  // What the server has acknowledged and the person has not cleared from the list. A cleared one
  // stays (it keeps the record as the server has it on screen) until a download holds the change.
  const shown = items.filter((i) => !i.cleared)
  const synced = shown.filter((i) => i.state === 'synced')
  const waiting = items.filter((i) => i.state !== 'synced')

  return (
    <section className="ui-outbox" aria-label={t('offline.queue.title')}>
      <header className="ui-outbox-head">
        <Heading level={2} size="subsection">
          {t('offline.queue.title')}
        </Heading>
        {waiting.length > 0 && (
          <span className="ui-outbox-note">{t('offline.queue.waiting', { count: waiting.length })}</span>
        )}
        {waiting.length > 0 && (
          <Button size="sm" onClick={() => act(() => (writer ? runtime.engine.retry() : runtime.engine.run()))}>
            {t('offline.sync.now')}
          </Button>
        )}
      </header>
      <SyncNotice status={status} waiting={waiting.length} />
      {notice && <Notice tone="danger">{notice}</Notice>}
      {shown.length === 0 && <p className="ui-outbox-note">{t('offline.queue.none')}</p>}
      {shown.length > 0 && (
        <Rows label={t('offline.queue.title')}>
          {shown.map((item) => {
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
            const lines = [
              item.attachments.length > 0
                ? t('offline.queue.photos', { uploaded, total: item.attachments.length })
                : '',
              item.state === 'synced' ? t('offline.queue.syncedNote') : '',
              item.state !== 'synced' && kind !== 'insert' ? t('offline.queue.pendingNote') : '',
              item.state === 'queued' && waitsForEarlier(item, items) ? t('offline.queue.waitsEarlier') : '',
              item.state === 'queued' && item.attempts > 0
                ? `${t('offline.queue.attempts', { count: item.attempts })}${
                    item.nextAttemptAt ? ` ${t('offline.queue.next', { when: f.relative(item.nextAttemptAt) })}` : ''
                  }`
                : '',
            ].filter(Boolean)
            return (
              <Row
                key={item.id}
                title={name}
                status={
                  <Badge tone={toneOf(item)} dot>
                    {t(`offline.state.${item.state}`)}
                  </Badge>
                }
                detail={
                  <>
                    {lines.map((line) => (
                      <span key={line} className="ui-outbox-line">
                        {line}
                      </span>
                    ))}
                    {item.state === 'attention' && item.attention && (
                      <span role="alert" className="ui-outbox-line" data-tone="danger">
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
                  </>
                }
                // Under the row, on a line of their own that wraps: a record that needs attention can
                // have four. A row with nothing to do (sent, being sent) has none.
                actionsBelow
                actions={
                  writer &&
                  item.state !== 'synced' &&
                  item.state !== 'syncing' && (
                    <>
                      {reviewable && (
                        <Button
                          size="sm"
                          aria-label={t('offline.queue.reviewNamed', { name })}
                          aria-expanded={resolving === item.id}
                          onClick={() => setResolving(resolving === item.id ? null : item.id)}
                        >
                          {t('offline.queue.review')}
                        </Button>
                      )}
                      {(item.state === 'queued' || (item.state === 'attention' && !settle)) && (
                        <Button
                          size="sm"
                          aria-label={t('offline.queue.retryNamed', { name })}
                          onClick={() => act(() => runtime.engine.retry(item.id))}
                        >
                          {t('offline.queue.retry')}
                        </Button>
                      )}
                      {(item.state === 'queued' || item.state === 'attention') && (
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-label={t('offline.queue.holdNamed', { name })}
                          onClick={() => act(() => runtime.engine.hold(item.id))}
                        >
                          {t('offline.queue.hold')}
                        </Button>
                      )}
                      {item.state === 'draft' && (
                        <Button
                          size="sm"
                          aria-label={t('offline.queue.sendNamed', { name })}
                          onClick={() => act(() => runtime.engine.release(item.id))}
                        >
                          {t('offline.queue.send')}
                        </Button>
                      )}
                      {/* Apart from the safe ones, at the end. */}
                      <span className="ui-outbox-discard">
                        <DiscardButton
                          runtime={runtime}
                          item={item}
                          name={name}
                          onDone={() => setNotice(null)}
                          onError={setNotice}
                          onClosed={() => setNotice(t('offline.queue.discardClosed'))}
                        />
                      </span>
                    </>
                  )
                }
              >
                {writer && resolving === item.id && reviewable && (
                  <Resolve
                    runtime={runtime}
                    item={item}
                    download={downloads.find((d) => d.key === item.scopeKey)}
                    name={name}
                    onClose={() => setResolving(null)}
                  />
                )}
              </Row>
            )
          })}
        </Rows>
      )}
      {writer && synced.length > 0 && (
        <Button
          size="sm"
          variant="ghost"
          onClick={() =>
            // Out of the list, not out of the queue: a deleted record would be shown again from the
            // copy, which doesn't know of the deletion, until a download does.
            act(() =>
              Promise.all(
                synced.map((i) =>
                  runtime.store.updateItem(i.id, (x) => (x.state === 'synced' ? { ...x, cleared: true } : undefined), {
                    guarded: true,
                  }),
                ),
              ),
            )
          }
        >
          {t('offline.queue.clearSynced')}
        </Button>
      )}
    </section>
  )
}

// The tone of each state: a draft and what is queued are neutral, sending informs, sent succeeded,
// and what needs the person is a warning, or the danger when the server refused it.
export function toneOf(item: OutboxItem): BadgeTone {
  switch (item.state) {
    case 'syncing':
      return 'info'
    case 'synced':
      return 'success'
    case 'attention':
      return item.attention === 'refused' || item.attention === 'gave_up' ? 'danger' : 'warning'
    default:
      return 'neutral'
  }
}
