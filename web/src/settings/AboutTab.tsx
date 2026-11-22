// About: TaskTo.AI's licence, the credits of the third-party work this workspace uses, as
// the server lists them (a local speech model's, or an add-on's while it is on), and for
// admins, how the server's scheduled backups are doing (docs/install.md#backups).

import { ApiError, api, type BackupStatus, backupPath, type Credit, type DictationStatus, dictationPath } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert } from './parts'

export function AboutTab({ workspace, admin }: { workspace: number; admin: boolean }) {
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
      {admin && <BackupsPanel workspace={workspace} />}
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
