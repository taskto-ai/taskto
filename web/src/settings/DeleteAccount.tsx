import { type FormEvent, useState } from 'react'
import { api, type DeletionReply, type DeletionStatus, exportPath, forAccount, sessionLostFor } from '../api'
import { beginDeletion, rememberDeletion } from '../auth/deleted'
import { forgetAccount } from '../auth/forgetAccount'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Alert, FormActions, Loading, Section, useAction } from './parts'

export function DeleteAccount({ workspace, user }: { workspace: number; user: number }) {
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
    const since = beginDeletion()
    e.preventDefault()
    let reply: DeletionReply | undefined
    await deleting.run(async () => {
      reply = await api<DeletionReply>('/api/me/deletion', {
        method: 'POST',
        headers: forAccount(user),
        body: s?.method === 'password' ? { password, ...(s.two_factor ? { code } : {}) } : {},
      })
    })
    if (reply) {
      rememberDeletion(reply.purge_at, since)
      await forgetAccount(user)
      window.dispatchEvent(sessionLostFor(user))
    }
  }

  const blocked = (s?.blockers.length ?? 0) > 0
  const needsFreshLogin = s?.method === 'fresh_login' && !s.fresh
  return (
    <Section title={t('settings.delete.title')}>
      <Panel padded className="settings-panel-body">
        <Alert error={status.error} />
        {!asking && (
          <>
            <p className="settings-muted">{t('settings.delete.summary')}</p>
            <FormActions>
              <Button variant="danger" onClick={() => setAsking(true)}>
                {t('settings.delete.start')}
              </Button>
            </FormActions>
          </>
        )}
        {asking && !s && !status.error && <Loading lines={2} />}
        {asking && !s && status.error && (
          <FormActions>
            <Button onClick={() => setAsking(false)}>{t('common.cancel')}</Button>
          </FormActions>
        )}
        {asking && s && (
          <>
            <p className="settings-muted">{t('settings.delete.intro', { days: f.number(s.grace_days) })}</p>
            <p className="settings-text">{t('settings.delete.removed')}</p>
            <ul className="settings-list">
              <li>{t('settings.delete.removedPrivate')}</li>
              <li>{t('settings.delete.removedConversations')}</li>
              <li>{t('settings.delete.removedLogins')}</li>
            </ul>
            <p className="settings-text">{t('settings.delete.kept')}</p>
            <p className="settings-muted">{t('settings.delete.cancel')}</p>
            {blocked && (
              <Notice tone="danger" title={t('settings.delete.blocked')}>
                <ul className="settings-list">
                  {s.blockers.map((b) => (
                    <li key={b.workspace_id}>
                      {t(b.operator ? 'settings.delete.blockedOperator' : 'settings.delete.blockedAdmin', {
                        workspace: b.workspace,
                      })}
                    </li>
                  ))}
                </ul>
              </Notice>
            )}
            <Alert error={exporting.error} />
            <FormActions>
              <Button icon="down" onClick={download} loading={exporting.busy}>
                {t('settings.delete.exportFirst')}
              </Button>
            </FormActions>
            {needsFreshLogin && <Notice tone="info">{t('settings.delete.signInAgain')}</Notice>}
            {!needsFreshLogin && (
              <form className="settings-form" onSubmit={submit} aria-label={t('settings.delete.formLabel')}>
                <p className="settings-text">
                  {t(s.method === 'password' ? 'settings.delete.confirmIntro' : 'settings.delete.confirmIntroFresh')}
                </p>
                {s.method === 'password' && (
                  <>
                    <Field label={t('settings.delete.password')} required>
                      <Input
                        type="password"
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        autoComplete="current-password"
                      />
                    </Field>
                    {s.two_factor && (
                      <Field label={t('settings.delete.code')} required>
                        <Input
                          type="text"
                          inputMode="numeric"
                          value={code}
                          onChange={(e) => setCode(e.target.value)}
                          autoComplete="one-time-code"
                        />
                      </Field>
                    )}
                  </>
                )}
                <Alert error={deleting.error} />
                <FormActions>
                  <Button type="submit" variant="danger" loading={deleting.busy} disabled={blocked}>
                    {deleting.busy ? t('settings.delete.deleting') : t('settings.delete.confirm')}
                  </Button>
                  <Button onClick={() => setAsking(false)} disabled={deleting.busy}>
                    {t('common.cancel')}
                  </Button>
                </FormActions>
              </form>
            )}
            {needsFreshLogin && (
              <FormActions>
                <Button onClick={() => setAsking(false)}>{t('common.cancel')}</Button>
              </FormActions>
            )}
          </>
        )}
      </Panel>
    </Section>
  )
}
