// About: TaskTo.AI's licence, the credits of the third-party work this workspace uses, as
// the server lists them (a local speech model's, or an add-on's while it is on), and for
// admins, how the server's scheduled backups are doing (docs/install.md#backups), what its
// host's updater last did (docs/install.md#automatic-updates), and for operators the Update now
// button, which asks that updater for a run (docs/install.md#update-now).

import { useEffect, useState } from 'react'
import {
  ApiError,
  api,
  type BackupStatus,
  backupPath,
  type Credit,
  type DictationStatus,
  dictationPath,
  type UpdateRequested,
  type UpdateRun,
  type UpdateStatus,
  updateRequestPath,
  updatesPath,
} from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { InstallPanel } from '../pwa/InstallPanel'
import { Alert, ConfirmButton, useAction } from './parts'

// `operator`: an admin who runs the server, the only one who sees its backups.
export function AboutTab({ workspace, operator }: { workspace: number; operator: boolean }) {
  // A server without dictation has nothing to credit for it: no error, no credits.
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
    <div className="tt-settings">
      <InstallPanel />
      <section className="tt-panel" aria-labelledby="tt-about">
        <h2 id="tt-about">{t('settings.about.title')}</h2>
        <p>{t('settings.about.licence')}</p>
      </section>
      <section className="tt-panel" aria-labelledby="tt-credits">
        <h2 id="tt-credits">{t('settings.about.credits')}</h2>
        <Alert error={dictation.error} />
        {dictation.data === undefined && !dictation.error && <p className="tt-muted">{t('app.loading')}</p>}
        {dictation.data !== undefined && credits.length === 0 && (
          <p className="tt-muted">{t('settings.about.noCredits')}</p>
        )}
        {credits.length > 0 && (
          <ul className="tt-credits">
            {credits.map((c) => (
              <CreditItem key={`${c.name} ${c.by}`} credit={c} />
            ))}
          </ul>
        )}
      </section>
      {operator && <UpdatesPanel workspace={workspace} />}
      {operator && <BackupsPanel workspace={workspace} />}
    </div>
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
// The updater's reason codes (internal/updatestatus): each has its sentence in the language files.
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
// Runs that ended with something wrong, which a person should look at, whether or not the updater
// also blocked itself.
const FAILURES = ['failed', 'rolled-back', 'rollback-failed', 'stopped', 'stop-failed', 'interrupted']
const isResult = (r: string): r is (typeof RESULTS)[number] => (RESULTS as readonly string[]).includes(r)
const isChannel = (c: string): c is (typeof CHANNELS)[number] => (CHANNELS as readonly string[]).includes(c)

type UpdateHealth = 'ok' | 'hold' | 'off' | 'stale' | 'failed' | 'blocked' | 'manualUpgrade'

const UPDATE_CLASS: Record<UpdateHealth, string | undefined> = {
  ok: 'tt-ok',
  hold: 'tt-warning',
  off: 'tt-muted',
  stale: 'tt-warning',
  failed: 'tt-warning',
  blocked: 'tt-error',
  manualUpgrade: 'tt-warning',
}

// What the host's updater last did. The updater runs on the host, and the server only reads what
// it wrote: nothing here runs or changes it. Update now asks it for a run (a request that names no
// version), and what a person must do about a gate is done on the host.
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
    <section className="tt-panel" aria-labelledby="tt-updates">
      <h2 id="tt-updates">{t('settings.about.updates.title')}</h2>
      <p className="tt-muted">{t('settings.about.updates.intro')}</p>
      {/* While an update goes on the server restarts, and a failed read is expected: UpdateNow says so. */}
      {!(st?.running || (st && u?.request.reason === 'pending')) && <Alert error={updates.error} />}
      {!u && !updates.error && <p className="tt-muted">{t('app.loading')}</p>}
      {u && !u.configured && <p className="tt-muted">{t('settings.about.updates.notConfigured')}</p>}
      {u?.configured && !st && !u.expects_status && <p className="tt-muted">{t('settings.about.updates.noStatus')}</p>}
      {u?.configured && !st && u.expects_status && (
        <p className="tt-warning">
          {t.rich('settings.about.updates.tooOld', { command: <code>{'taskto-update --publish-status'}</code> })}
        </p>
      )}
      {health && (
        <p className={UPDATE_CLASS[health]} role={health === 'blocked' ? 'alert' : undefined}>
          {t(`settings.about.updates.health.${health}`)}
        </p>
      )}
      {needs && (
        <>
          <p>
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
          {needs.code && <p>{t('settings.about.updates.why', { reason: reasonText(needs.code) })}</p>}
          <p className="tt-muted">{t('settings.about.updates.needs.where')}</p>
        </>
      )}
      {u?.configured && st && (
        <UpdateNow
          workspace={workspace}
          report={u}
          reload={updates.reload}
          settled={updates.settled ?? 0}
          loadError={updates.error}
          label={label}
          reasonText={reasonText}
        />
      )}
      {u && (
        <dl className="tt-endpoint">
          <dt>{t('settings.about.updates.running')}</dt>
          <dd>
            <code>{u.running_version}</code>
          </dd>
          {st?.recorded_version && (
            <>
              <dt>{t('settings.about.updates.recorded')}</dt>
              <dd>
                <code>{st.recorded_version}</code>
                {mismatch && (
                  <>
                    <br />
                    <span className="tt-warning">{t('settings.about.updates.mismatch')}</span>
                  </>
                )}
              </dd>
            </>
          )}
          {st && (
            <>
              <dt>{t('settings.about.updates.channel')}</dt>
              <dd>
                {t(st.hold ? 'settings.about.updates.channelHold' : 'settings.about.updates.channelName', {
                  channel: isChannel(st.channel)
                    ? t(`settings.about.updates.channelValue.${st.channel}`)
                    : st.channel || '?',
                })}
              </dd>
              <dt>{t('settings.about.updates.lastRun')}</dt>
              <dd>{run(st.last_run)}</dd>
              <dt>{t('settings.about.updates.lastChange')}</dt>
              <dd>{run(st.last_change)}</dd>
              {st.checked && (
                <>
                  <dt>{t('settings.about.updates.checked')}</dt>
                  <dd>{`${f.dateTime(st.checked)} (${f.relative(st.checked)})`}</dd>
                </>
              )}
            </>
          )}
        </dl>
      )}
    </section>
  )
}

// How often the page asks again while an update is pending or running, and how long it waits for
// the host to start one it asked for before it says so.
const POLL_MS = 3000
const GIVE_UP_MS = 15 * 60_000

// Update now: asks the host's updater for the nightly run, now. The request has no version: the
// host installs the newest of its channel, under every gate of the nightly run, and the outcome
// (an update, or why not) comes back in the status the page already reads. The server restarts
// during an update, so the page keeps asking and says so until it answers again.
function UpdateNow({
  workspace,
  report,
  reload,
  settled,
  loadError,
  label,
  reasonText,
}: {
  workspace: number
  report: UpdateStatus
  reload: () => void
  // How many reads have ended: the next one is asked for after the last ended, not on a clock.
  settled: number
  loadError: string | null
  label: (result: string) => string
  reasonText: (code: string) => string
}) {
  const t = useT()
  const action = useAction()
  const [requestedAt, setRequestedAt] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const st = report.status
  const req = report.request
  const reason = req.reason ?? ''
  const running = !!st?.running
  const pending = reason === 'pending'
  // The updater stamps a run with the second it started in; ours is the second we asked in.
  const since = requestedAt === null ? null : Math.floor(Date.parse(requestedAt) / 1000) * 1000
  const last = st?.requests?.last ?? null
  const answered = since !== null && !!last?.at && Date.parse(last.at) >= since
  // A request that waits, or was just made, and that nobody has answered: since when.
  const askedAt = since ?? (pending && req.pending_since ? Date.parse(req.pending_since) : null)
  const waiting = since !== null && !answered
  // The host never took it (its path unit is off) or never answered: said, whether or not the file
  // still waits in the directory.
  const gaveUp = !answered && !running && askedAt !== null && now - askedAt > GIVE_UP_MS
  const busy = running || ((pending || waiting) && !gaveUp)
  // Ask again a moment after the last answer (or failure) came, never while one is on its way: the
  // server restarts during an update, and a read that is cut off by the next one shows nothing.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `settled` is what moves the timer on
  useEffect(() => {
    if (!busy || settled === 0) return
    const timer = setTimeout(() => {
      setNow(Date.now())
      reload()
    }, POLL_MS)
    return () => clearTimeout(timer)
  }, [busy, settled, reload])
  const press = async () => {
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
    <div className="tt-update-now">
      {busy && (
        <p role="status">
          {t(running ? 'settings.about.updates.now.updating' : 'settings.about.updates.now.requested')}
        </p>
      )}
      {busy && loadError && <p className="tt-muted">{t('settings.about.updates.now.reconnecting')}</p>}
      {gaveUp && <p className="tt-warning">{t('settings.about.updates.now.gaveUp')}</p>}
      {outcome && (
        <>
          <p role="status" className={outcome.result === 'updated' ? 'tt-ok' : undefined}>
            {t('settings.about.updates.now.outcome', { result: label(outcome.result) })}
          </p>
          {outcome.code && <p>{t('settings.about.updates.why', { reason: reasonText(outcome.code) })}</p>}
          {(outcome.result === 'skipped' || outcome.result === 'failed') && (
            <p className="tt-muted">
              {t.rich('settings.about.updates.now.hint', { command: <code>{'taskto-update --to VERSION'}</code> })}
            </p>
          )}
        </>
      )}
      {!busy && req.available && (
        <ConfirmButton
          label={t('settings.about.updates.now.button')}
          question={t('settings.about.updates.now.question')}
          confirm={t('settings.about.updates.now.confirm')}
          disabled={action.busy}
          onConfirm={press}
        />
      )}
      {!busy && !req.available && (reason === 'not_configured' || reason === 'unsupported' || reason === 'off') && (
        <p className="tt-muted">{t(`settings.about.updates.now.unavailable.${reason}`)}</p>
      )}
      <Alert error={action.error} />
    </div>
  )
}

const HEALTH_CLASS: Record<BackupStatus['health'], string | undefined> = {
  none: 'tt-muted',
  pending: undefined,
  ok: 'tt-ok',
  failing: 'tt-warning',
  late: 'tt-error',
}

function BackupsPanel({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const backups = useLoad(() => api<BackupStatus>(backupPath(workspace)), [workspace])
  const b = backups.data
  // A backup that started after the last outcome is still running.
  const attempt = b?.last_attempt
  const running = attempt && [b.last_success, b.last_error_at].every((x) => !x || Date.parse(x) < Date.parse(attempt))
  // The last error, unless a success came after it.
  const failedSince =
    b?.last_error && b.last_error_at && (!b.last_success || Date.parse(b.last_error_at) > Date.parse(b.last_success))
  return (
    <section className="tt-panel" aria-labelledby="tt-backups">
      <h2 id="tt-backups">{t('settings.about.backups.title')}</h2>
      <Alert error={backups.error} />
      {!b && !backups.error && <p className="tt-muted">{t('app.loading')}</p>}
      {b && (
        <p className={HEALTH_CLASS[b.health]} role={b.health === 'late' ? 'alert' : undefined}>
          {t(`settings.about.backups.health.${b.health}`)}
        </p>
      )}
      {running && <p>{t('settings.about.backups.running', { time: f.dateTime(attempt) })}</p>}
      {b && b.health !== 'none' && (
        <dl className="tt-endpoint">
          <dt>{t('settings.about.backups.lastSuccess')}</dt>
          <dd>
            {b.last_success
              ? `${f.dateTime(b.last_success)} (${f.relative(b.last_success)})`
              : t('settings.about.backups.never')}
          </dd>
          {b.last_snapshot && (
            <>
              <dt>{t('settings.about.backups.snapshot')}</dt>
              <dd>
                <code>{b.last_snapshot.slice(0, 8)}</code>
              </dd>
            </>
          )}
          {failedSince && b.last_error_at && (
            <>
              <dt>{t('settings.about.backups.lastError')}</dt>
              <dd>
                {f.dateTime(b.last_error_at)}
                <br />
                <code>{b.last_error}</code>
              </dd>
            </>
          )}
          {b.next_attempt && (
            <>
              <dt>{t('settings.about.backups.nextAttempt')}</dt>
              <dd>{f.dateTime(b.next_attempt)}</dd>
            </>
          )}
          {b.at && (
            <>
              <dt>{t('settings.about.backups.schedule')}</dt>
              <dd>
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
              </dd>
            </>
          )}
        </dl>
      )}
    </section>
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
      {c.note && <span className="tt-muted"> {c.note}</span>}
    </li>
  )
}
