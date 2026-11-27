import { useEffect, useState } from 'react'

import {
  ApiError,
  api,
  type BackupStatus,
  backupPath,
  type Credit,
  type DictationStatus,
  dictationPath,
  type UpdateAvailable,
  type UpdateRequested,
  type UpdateRun,
  type UpdateStatus,
  updateRequestPath,
  updatesPath,
} from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { InstallPanel } from '../pwa/InstallPanel'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Notice, type NoticeTone } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Alert, Facts, FormActions, Loading, Section, SettingsTab, useAction } from './parts'

export function AboutTab({ workspace, operator }: { workspace: number; operator: boolean }) {
  const dictation = useLoad(
    () =>
      api<DictationStatus>(dictationPath(workspace)).catch((e) => {
        if (e instanceof ApiError && e.status === 404) return null
        throw e
      }),
    [workspace],
  )
  const credits = dictation.data?.credits ?? []
  const t = useT()
  return (
    <SettingsTab title={t('settings.about.title')} help={t('settings.about.licence')}>
      <InstallPanel />
      <Section title={t('settings.about.credits')}>
        <Alert error={dictation.error} />
        {dictation.data === undefined && !dictation.error && <Loading lines={2} />}
        {dictation.data !== undefined && credits.length === 0 && (
          <p className="settings-muted">{t('settings.about.noCredits')}</p>
        )}
        {credits.length > 0 && (
          <ul className="settings-list">
            {credits.map((c) => (
              <CreditItem key={`${c.name} ${c.by}`} credit={c} />
            ))}
          </ul>
        )}
      </Section>
      {operator && <UpdatesPanel workspace={workspace} />}
      {operator && <BackupsPanel workspace={workspace} />}
    </SettingsTab>
  )
}

const RESULTS = [
  'updated',
  'up-to-date',
  'skipped',
  'failed',
  'rolled-back',
  'rollback-failed',
  'stopped',
  'stop-failed',
  'blocked',
  'interrupted',
  'cleared',
  'ignored',
  'unknown',
] as const
const CHANNELS = ['release', 'main', 'off'] as const
const REASONS = [
  'other',
  'off',
  'hold',
  'manual-upgrade',
  'github',
  'not-published',
  'not-newer',
  'version-mismatch',
  'server-down',
  'unsafe-settings',
  'disk',
  'registry',
  'backup',
  'bad-setting',
  'firewall',
  'settings',
  'blocked',
  'stopped',
  'stop-failed',
  'rollback-failed',
  'interrupted',
  'rolled-back',
  'unrecorded-failure',
  'rate-limited',
  'bad-request',
] as const
const isReason = (c: string): c is (typeof REASONS)[number] => (REASONS as readonly string[]).includes(c)
const FAILURES = ['failed', 'rolled-back', 'rollback-failed', 'stopped', 'stop-failed', 'interrupted']
const isResult = (r: string): r is (typeof RESULTS)[number] => (RESULTS as readonly string[]).includes(r)
const isChannel = (c: string): c is (typeof CHANNELS)[number] => (CHANNELS as readonly string[]).includes(c)

type UpdateHealth = 'ok' | 'hold' | 'off' | 'stale' | 'failed' | 'blocked' | 'manualUpgrade'

const UPDATE_TONE: Record<UpdateHealth, NoticeTone> = {
  ok: 'success',
  hold: 'warning',
  off: 'neutral',
  stale: 'warning',
  failed: 'warning',
  blocked: 'danger',
  manualUpgrade: 'warning',
}

function UpdatesPanel({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const updates = useLoad(() => api<UpdateStatus>(updatesPath(workspace)), [workspace])
  const u = updates.data
  const st = u?.status ?? null
  const needs = st?.needs_person ?? null
  let health: UpdateHealth | null = null
  if (st) {
    if (needs?.kind === 'blocked') health = 'blocked'
    else if (needs?.kind === 'manual-upgrade') health = 'manualUpgrade'
    else if (u?.stale) health = 'stale'
    else if (st.last_run && FAILURES.includes(st.last_run.result)) health = 'failed'
    else if (st.channel === 'off') health = 'off'
    else if (st.hold) health = 'hold'
    else health = 'ok'
  }
  const mismatch = !!st?.recorded_version && !!u && st.recorded_version !== u.running_version
  const label = (r: string) => t(`settings.about.updates.result.${isResult(r) ? r : 'unknown'}`)
  const reasonText = (c: string) => t(`settings.about.updates.reasons.${isReason(c) ? c : 'other'}`)
  const run = (r: UpdateRun | null | undefined) => {
    if (!r) return t('settings.about.updates.noRun')
    const when = r.finished ?? r.at
    return (
      <>
        {t('settings.about.updates.runLine', {
          result: label(r.result),
          time: when ? `${f.dateTime(when)} (${f.relative(when)})` : '',
        })}
        {r.to && (
          <>
            <br />
            {t('settings.about.updates.versions', { from: r.from || '?', to: r.to })}
          </>
        )}
        {r.code && (
          <>
            <br />
            {t('settings.about.updates.why', { reason: reasonText(r.code) })}
          </>
        )}
        {r.backed_up && (
          <>
            <br />
            {t('settings.about.updates.backedUp')}
          </>
        )}
      </>
    )
  }
  return (
    <Section
      title={t('settings.about.updates.title')}
      help={t('settings.about.updates.intro')}
      actions={
        health && (
          <Badge tone={UPDATE_TONE[health]} dot={health === 'ok'}>
            {t(`settings.about.updates.badge.${health}`)}
          </Badge>
        )
      }
    >
      {!(st?.running || (st && u?.request.reason === 'pending')) && <Alert error={updates.error} />}
      {!u && !updates.error && <Loading lines={3} />}
      {u && !u.configured && <Notice tone="neutral">{t('settings.about.updates.notConfigured')}</Notice>}
      {u?.configured && !st && !u.expects_status && (
        <Notice tone="neutral">{t('settings.about.updates.noStatus')}</Notice>
      )}
      {u?.configured && !st && u.expects_status && (
        <Notice tone="warning">
          {t.rich('settings.about.updates.tooOld', { command: <code>{'taskto-update --publish-status'}</code> })}
        </Notice>
      )}
      {health && <Notice tone={UPDATE_TONE[health]}>{t(`settings.about.updates.health.${health}`)}</Notice>}
      {needs && (
        <Panel padded className="settings-panel-body">
          <p className="settings-text">
            {t.rich(
              needs.kind === 'blocked'
                ? 'settings.about.updates.needs.blocked'
                : 'settings.about.updates.needs.manualUpgrade',
              {
                command: (
                  <code>{needs.kind === 'blocked' ? 'taskto-update --clear-block' : 'taskto-update --to VERSION'}</code>
                ),
              },
            )}
          </p>
          {needs.code && (
            <p className="settings-text">{t('settings.about.updates.why', { reason: reasonText(needs.code) })}</p>
          )}
          <p className="settings-muted">{t('settings.about.updates.needs.where')}</p>
        </Panel>
      )}
      {st?.available && <AvailableUpdate available={st.available} notesUrl={u?.notes_url} />}
      {st && !st.available && st.looked && st.channel !== 'off' && (
        <p className="settings-muted">{t('settings.about.updates.available.upToDate')}</p>
      )}
      {u?.configured && st && (
        <UpdateNow
          workspace={workspace}
          report={u}
          reload={updates.reload}
          settled={updates.settled ?? 0}
          loading={updates.loading ?? false}
          loadError={updates.error}
          label={label}
          reasonText={reasonText}
        />
      )}
      {mismatch && <Notice tone="warning">{t('settings.about.updates.mismatch')}</Notice>}
      {u && (
        <Panel padded>
          <Facts
            items={[
              { key: 'running', term: t('settings.about.updates.running'), value: <code>{u.running_version}</code> },
              ...(st?.recorded_version
                ? [
                    {
                      key: 'recorded',
                      term: t('settings.about.updates.recorded'),
                      value: (
                        <>
                          <code>{st.recorded_version}</code>
                          {mismatch && (
                            <>
                              {' '}
                              <Badge tone="warning">{t('settings.about.updates.mismatchBadge')}</Badge>
                            </>
                          )}
                        </>
                      ),
                    },
                  ]
                : []),
              ...(st
                ? [
                    {
                      key: 'channel',
                      term: t('settings.about.updates.channel'),
                      value: t(st.hold ? 'settings.about.updates.channelHold' : 'settings.about.updates.channelName', {
                        channel: isChannel(st.channel)
                          ? t(`settings.about.updates.channelValue.${st.channel}`)
                          : st.channel || '?',
                      }),
                    },
                    { key: 'lastRun', term: t('settings.about.updates.lastRun'), value: run(st.last_run) },
                    ...(st.looked
                      ? [
                          {
                            key: 'looked',
                            term: t('settings.about.updates.available.looked'),
                            value: `${f.dateTime(st.looked)} (${f.relative(st.looked)})`,
                          },
                        ]
                      : []),
                    { key: 'lastChange', term: t('settings.about.updates.lastChange'), value: run(st.last_change) },
                    ...(st.checked
                      ? [
                          {
                            key: 'checked',
                            term: t('settings.about.updates.checked'),
                            value: `${f.dateTime(st.checked)} (${f.relative(st.checked)})`,
                          },
                        ]
                      : []),
                  ]
                : []),
            ]}
          />
        </Panel>
      )}
    </Section>
  )
}

function AvailableUpdate({ available: a, notesUrl }: { available: UpdateAvailable; notesUrl?: string }) {
  const t = useT()
  const f = useFormat()
  const commit = /^[0-9a-f]{40}$/.test(a.version)
  const version = <code title={a.version}>{commit ? a.version.slice(0, 12) : a.version}</code>
  return (
    <Notice tone={a.manual_upgrade ? 'warning' : 'info'}>
      <p className="settings-text">
        {a.published
          ? t.rich('settings.about.updates.available.line', { version, date: f.date(a.published) })
          : t.rich('settings.about.updates.available.lineUndated', { version })}
        {a.manual_upgrade && (
          <>
            {' '}
            <Badge tone="warning">{t('settings.about.updates.available.needsPerson')}</Badge>
          </>
        )}
      </p>
      {notesUrl && (
        <p className="settings-text">
          <a href={notesUrl} target="_blank" rel="noreferrer noopener">
            {t(commit ? 'settings.about.updates.available.compare' : 'settings.about.updates.available.notes')}
          </a>
        </p>
      )}
      {a.manual_upgrade && (
        <p className="settings-muted">
          {t.rich('settings.about.updates.available.manual', {
            command: <code>{`taskto-update --to ${a.version}`}</code>,
          })}
        </p>
      )}
    </Notice>
  )
}

const POLL_MS = 3000
const GIVE_UP_MS = 15 * 60_000

function UpdateNow({
  workspace,
  report,
  reload,
  settled,
  loading,
  loadError,
  label,
  reasonText,
}: {
  workspace: number
  report: UpdateStatus
  reload: () => void
  settled: number
  loading: boolean
  loadError: string | null
  label: (result: string) => string
  reasonText: (code: string) => string
}) {
  const t = useT()
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const [requestedAt, setRequestedAt] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const st = report.status
  const req = report.request
  const reason = req.reason ?? ''
  const running = !!st?.running
  const pending = reason === 'pending'
  const since = requestedAt === null ? null : Math.floor(Date.parse(requestedAt) / 1000) * 1000
  const last = st?.requests?.last ?? null
  const answered = since !== null && !!last?.at && Date.parse(last.at) >= since
  const askedAt = since ?? (pending && req.pending_since ? Date.parse(req.pending_since) : null)
  const waiting = since !== null && !answered
  const gaveUp = !answered && !running && askedAt !== null && now - askedAt > GIVE_UP_MS
  const busy = running || ((pending || waiting) && !gaveUp)
  useEffect(() => {
    if (!busy || loading || settled === 0) return
    const timer = setTimeout(() => {
      setNow(Date.now())
      reload()
    }, POLL_MS)
    return () => clearTimeout(timer)
  }, [busy, loading, settled, reload])
  const press = async () => {
    const yes = await confirm({
      title: t('settings.about.updates.now.title'),
      body: t('settings.about.updates.now.question'),
      confirmLabel: t('settings.about.updates.now.confirm'),
    })
    if (!yes) return false
    const ok = await action.run(async () => {
      const r = await api<UpdateRequested>(updateRequestPath(workspace), { method: 'POST', body: {} })
      setRequestedAt(r.requested_at)
      setNow(Date.now())
    })
    reload()
    return ok
  }
  const outcome = answered && !busy && last
  return (
    <>
      {busy && (
        <Notice tone="info">
          {t(running ? 'settings.about.updates.now.updating' : 'settings.about.updates.now.requested')}
        </Notice>
      )}
      {busy && loadError && <p className="settings-muted">{t('settings.about.updates.now.reconnecting')}</p>}
      {gaveUp && <Notice tone="warning">{t('settings.about.updates.now.gaveUp')}</Notice>}
      {outcome && (
        <Notice tone={outcome.result === 'updated' ? 'success' : 'neutral'}>
          <p className="settings-text">{t('settings.about.updates.now.outcome', { result: label(outcome.result) })}</p>
          {outcome.code && (
            <p className="settings-text">{t('settings.about.updates.why', { reason: reasonText(outcome.code) })}</p>
          )}
          {(outcome.result === 'skipped' || outcome.result === 'failed') && (
            <p className="settings-muted">
              {t.rich('settings.about.updates.now.hint', { command: <code>{'taskto-update --to VERSION'}</code> })}
            </p>
          )}
        </Notice>
      )}
      {!busy && req.available && (
        <FormActions>
          <Button disabled={action.busy} onClick={press}>
            {t('settings.about.updates.now.button')}
          </Button>
        </FormActions>
      )}
      {!busy && !req.available && (reason === 'not_configured' || reason === 'unsupported' || reason === 'off') && (
        <p className="settings-muted">{t(`settings.about.updates.now.unavailable.${reason}`)}</p>
      )}
      <Alert error={action.error} />
      {confirmDialog}
    </>
  )
}

const BACKUP_TONE: Record<BackupStatus['health'], NoticeTone> = {
  none: 'neutral',
  pending: 'info',
  ok: 'success',
  failing: 'warning',
  late: 'danger',
}

function BackupsPanel({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const backups = useLoad(() => api<BackupStatus>(backupPath(workspace)), [workspace])
  const b = backups.data
  const attempt = b?.last_attempt
  const running = attempt && [b.last_success, b.last_error_at].every((x) => !x || Date.parse(x) < Date.parse(attempt))
  const failedSince =
    b?.last_error && b.last_error_at && (!b.last_success || Date.parse(b.last_error_at) > Date.parse(b.last_success))
  return (
    <Section
      title={t('settings.about.backups.title')}
      actions={
        b && (
          <Badge tone={BACKUP_TONE[b.health]} dot={b.health === 'ok'}>
            {t(`settings.about.backups.badge.${b.health}`)}
          </Badge>
        )
      }
    >
      <Alert error={backups.error} />
      {!b && !backups.error && <Loading lines={3} />}
      {b && <Notice tone={BACKUP_TONE[b.health]}>{t(`settings.about.backups.health.${b.health}`)}</Notice>}
      {running && <p className="settings-text">{t('settings.about.backups.running', { time: f.dateTime(attempt) })}</p>}
      {b && b.health !== 'none' && (
        <Panel padded>
          <Facts
            items={[
              {
                key: 'lastSuccess',
                term: t('settings.about.backups.lastSuccess'),
                value: b.last_success
                  ? `${f.dateTime(b.last_success)} (${f.relative(b.last_success)})`
                  : t('settings.about.backups.never'),
              },
              ...(b.last_snapshot
                ? [
                    {
                      key: 'snapshot',
                      term: t('settings.about.backups.snapshot'),
                      value: <code>{b.last_snapshot.slice(0, 8)}</code>,
                    },
                  ]
                : []),
              ...(failedSince && b.last_error_at
                ? [
                    {
                      key: 'lastError',
                      term: t('settings.about.backups.lastError'),
                      value: (
                        <>
                          {f.dateTime(b.last_error_at)}
                          <br />
                          <code>{b.last_error}</code>
                        </>
                      ),
                    },
                  ]
                : []),
              ...(b.next_attempt
                ? [
                    {
                      key: 'nextAttempt',
                      term: t('settings.about.backups.nextAttempt'),
                      value: f.dateTime(b.next_attempt),
                    },
                  ]
                : []),
              ...(b.at
                ? [
                    {
                      key: 'schedule',
                      term: t('settings.about.backups.schedule'),
                      value: (
                        <>
                          {t('settings.about.backups.scheduleAt', { at: b.at })}{' '}
                          {b.retries
                            ? t('settings.about.backups.retries', {
                                count: b.retries,
                                every: f.number(Math.round((b.retry_every_seconds ?? 0) / 60), {
                                  style: 'unit',
                                  unit: 'minute',
                                  unitDisplay: 'long',
                                }),
                              })
                            : t('settings.about.backups.noRetries')}
                        </>
                      ),
                    },
                  ]
                : []),
            ]}
          />
        </Panel>
      )}
    </Section>
  )
}

function CreditItem({ credit: c }: { credit: Credit }) {
  const t = useT()
  return (
    <li>
      {t.rich('settings.about.credit', {
        name: c.name,
        by: c.by,
        licence: c.licence,
        work: (text) => (
          <a href={c.url} target="_blank" rel="noreferrer noopener">
            {text}
          </a>
        ),
        licenceLink: (text) => (
          <a href={c.licence_url} target="_blank" rel="noreferrer noopener">
            {text}
          </a>
        ),
      })}
      {c.note && <span className="settings-muted"> {c.note}</span>}
    </li>
  )
}
