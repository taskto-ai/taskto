// About: TaskTo.AI's licence, the credits of the third-party work this workspace uses, as
// the server lists them (a local speech model's, or an add-on's while it is on), and for
// admins, how the server's scheduled backups are doing (docs/install.md#backups) and what its
// host's updater last did (docs/install.md#automatic-updates).

import {
  ApiError,
  api,
  type BackupStatus,
  backupPath,
  type Credit,
  type DictationStatus,
  dictationPath,
  type UpdateRun,
  type UpdateStatus,
  updatesPath,
} from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { InstallPanel } from '../pwa/InstallPanel'
import { Alert } from './parts'

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
// it wrote: nothing here runs or changes it, and what a person must do is done on the host.
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
      <Alert error={updates.error} />
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
