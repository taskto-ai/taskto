// Your own sessions: the browsers and devices logged in as you, each with where it was made,
// when, and when it was last used, to sign out one at a time or all the others at once. The server
// never sends a session's token (docs/api.md#your-sessions).

import { api, type Session } from '../api'
import { type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, useAction } from './parts'

export function SessionsTab() {
  const sessions = useLoad(() => api<{ sessions: Session[] }>('/api/me/sessions').then((r) => r.sessions), [])
  const action = useAction()
  const f = useFormat()
  const t = useT()

  const run = (change: () => Promise<unknown>) =>
    action.run(change).then((ok) => {
      // A failed change may still have ended some of them: show what is true now.
      sessions.reload()
      return ok
    })
  const revoke = (s: Session) => run(() => api(`/api/me/sessions/${s.id}`, { method: 'DELETE' }))
  const revokeOthers = () => run(() => api('/api/me/sessions', { method: 'DELETE' }))
  const others = sessions.data?.filter((s) => !s.current).length ?? 0

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-sessions">
        <h2 id="tt-sessions">{t('settings.sessions.title')}</h2>
        <p className="tt-muted tt-small">{t('settings.sessions.intro')}</p>
        <Alert error={sessions.error ?? action.error} />
        {!sessions.data && !sessions.error && <p className="tt-muted">{t('app.loading')}</p>}
        {sessions.data && (
          <ul className="tt-rows" aria-label={t('settings.sessions.yours')}>
            {sessions.data.map((s) => {
              const device = deviceName(t, s)
              return (
                <li key={s.id} className="tt-row">
                  <div className="tt-row-main">
                    <strong>
                      {device}
                      {s.current && <span className="tt-badge"> {t('settings.sessions.current')}</span>}
                    </strong>
                    <span className="tt-muted tt-small">
                      {t('settings.sessions.created', { date: f.dateTime(s.created_at) })}
                      {' · '}
                      {t('settings.sessions.lastSeen', { when: f.relative(s.last_seen_at) })}
                    </span>
                  </div>
                  {!s.current && (
                    <div className="tt-row-actions">
                      <ConfirmButton
                        label={t('settings.sessions.revoke')}
                        name={device}
                        question={t('settings.sessions.revokeQuestion', { name: device })}
                        confirm={t('settings.sessions.revokeConfirm')}
                        disabled={action.busy}
                        onConfirm={() => revoke(s)}
                      />
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        )}
        {sessions.data &&
          (others > 0 ? (
            <ConfirmButton
              label={t('settings.sessions.revokeOthers')}
              question={t('settings.sessions.revokeOthersQuestion')}
              confirm={t('settings.sessions.revokeOthersConfirm')}
              disabled={action.busy}
              onConfirm={revokeOthers}
            />
          ) : (
            <p className="tt-muted tt-small">{t('settings.sessions.noOthers')}</p>
          ))}
      </section>
    </div>
  )
}

// "Firefox on macOS", or whichever of the two the server could tell.
function deviceName(t: T, s: Session): string {
  if (s.browser && s.os) return t('settings.sessions.device', { browser: s.browser, os: s.os })
  return s.browser || s.os || t('settings.sessions.unknown')
}
