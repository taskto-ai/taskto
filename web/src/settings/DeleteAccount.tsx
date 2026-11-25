// Deleting your own account (docs/api.md#deleting-an-account). The account is switched off at once,
// signed out everywhere, and kept for a grace period before the purge deletes what it owned alone;
// what you made for others stays, as "Deleted user". It takes your password (and a code, if you have
// two-factor login on), or, with no password, a sign-in made a few minutes ago. The export of your data
// is offered first, and the workspaces where you are the only admin are named: they stop the deletion.
// Looking at the page asks the server nothing: what deleting takes is read when you press the button.

import { type FormEvent, useState } from 'react'
import { api, type DeletionReply, type DeletionStatus, exportPath, SESSION_LOST } from '../api'
import { rememberDeletion } from '../auth/deleted'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, useAction } from './parts'

export function DeleteAccount({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const [asking, setAsking] = useState(false)
  const status = useLoad(() => (asking ? api<DeletionStatus>('/api/me/deletion') : Promise.resolve(null)), [asking])
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const exporting = useAction()
  const deleting = useAction()
  const s = status.data ?? undefined

  const download = () =>
    exporting.run(async () => {
      const { download } = await api<{ download: string }>(exportPath(workspace), { method: 'POST', body: {} })
      window.location.assign(download)
    })
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let reply: DeletionReply | undefined
    const ok = await deleting.run(async () => {
      reply = await api<DeletionReply>('/api/me/deletion', {
        method: 'POST',
        body: s?.method === 'password' ? { password, ...(s.two_factor ? { code } : {}) } : {},
      })
    })
    if (ok && reply) {
      // The server ended this session: on to the sign-in screen, which says when the account is purged.
      rememberDeletion(reply.purge_at)
      window.dispatchEvent(new Event(SESSION_LOST))
    }
  }

  const blocked = (s?.blockers.length ?? 0) > 0
  const needsFreshLogin = s?.method === 'fresh_login' && !s.fresh
  return (
    <section className="tt-panel" aria-labelledby="tt-delete-account">
      <h2 id="tt-delete-account">{t('settings.delete.title')}</h2>
      <Alert error={status.error} />
      {!asking && (
        <>
          <p className="tt-muted tt-small">{t('settings.delete.summary')}</p>
          <p>
            <button type="button" className="tt-button" onClick={() => setAsking(true)}>
              {t('settings.delete.start')}
            </button>
          </p>
        </>
      )}
      {asking && !s && !status.error && <p className="tt-muted">{t('app.loading')}</p>}
      {asking && !s && status.error && (
        <p>
          <button type="button" className="tt-button" onClick={() => setAsking(false)}>
            {t('common.cancel')}
          </button>
        </p>
      )}
      {asking && s && (
        <>
          <p className="tt-muted tt-small">{t('settings.delete.intro', { days: f.number(s.grace_days) })}</p>
          <p className="tt-small">{t('settings.delete.removed')}</p>
          <ul className="tt-small">
            <li>{t('settings.delete.removedPrivate')}</li>
            <li>{t('settings.delete.removedConversations')}</li>
            <li>{t('settings.delete.removedLogins')}</li>
          </ul>
          <p className="tt-small">{t('settings.delete.kept')}</p>
          <p className="tt-muted tt-small">{t('settings.delete.cancel')}</p>
          {blocked && (
            <div role="alert" className="tt-error">
              <p>{t('settings.delete.blocked')}</p>
              <ul>
                {s.blockers.map((b) => (
                  <li key={b.workspace_id}>
                    {t(b.operator ? 'settings.delete.blockedOperator' : 'settings.delete.blockedAdmin', {
                      workspace: b.workspace,
                    })}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <Alert error={exporting.error} />
          <p>
            <button type="button" className="tt-button" onClick={download} disabled={exporting.busy}>
              {t('settings.delete.exportFirst')}
            </button>
          </p>
          {needsFreshLogin && <p className="tt-notice">{t('settings.delete.signInAgain')}</p>}
          {!needsFreshLogin && (
            <form className="tt-form" onSubmit={submit} aria-label={t('settings.delete.formLabel')}>
              <p>{t(s.method === 'password' ? 'settings.delete.confirmIntro' : 'settings.delete.confirmIntroFresh')}</p>
              {s.method === 'password' && (
                <>
                  <label>
                    {t('settings.delete.password')}
                    <input
                      type="password"
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                      required
                      autoComplete="current-password"
                    />
                  </label>
                  {s.two_factor && (
                    <label>
                      {t('settings.delete.code')}
                      <input
                        type="text"
                        inputMode="numeric"
                        value={code}
                        onChange={(e) => setCode(e.target.value)}
                        required
                        autoComplete="one-time-code"
                      />
                    </label>
                  )}
                </>
              )}
              <Alert error={deleting.error} />
              <div className="tt-row-actions">
                <button type="submit" className="tt-button tt-danger" disabled={deleting.busy || blocked}>
                  {deleting.busy ? t('settings.delete.deleting') : t('settings.delete.confirm')}
                </button>
                <button type="button" className="tt-button" onClick={() => setAsking(false)} disabled={deleting.busy}>
                  {t('common.cancel')}
                </button>
              </div>
            </form>
          )}
          {needsFreshLogin && (
            <p>
              <button type="button" className="tt-button" onClick={() => setAsking(false)}>
                {t('common.cancel')}
              </button>
            </p>
          )}
        </>
      )}
    </section>
  )
}
