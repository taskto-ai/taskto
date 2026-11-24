// Members and invites, for admins. An invite's link, or a member's password reset link, is
// shown once, when it is made: the server keeps only a hash of its token. The token rides in
// the URL's fragment, which browsers don't send to servers or in Referer.

import { type FormEvent, useState } from 'react'
import { ApiError, api, type Invite, type Member, ws } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, CopyButton, day, useAction } from './parts'

type Role = Member['role']

export function inviteLink(token: string): string {
  return `${window.location.origin}/invite#${token}`
}

export function resetLink(token: string): string {
  return `${window.location.origin}/reset-password#${token}`
}

export function MembersTab({ workspace, me }: { workspace: number; me: number }) {
  const t = useT()
  const f = useFormat()
  const base = ws(workspace)
  const members = useLoad(() => api<{ members: Member[] }>(`${base}/members`).then((r) => r.members), [base])
  const invites = useLoad(() => api<{ invites: Invite[] }>(`${base}/invites`).then((r) => r.invites), [base])
  const action = useAction()
  // A reset link sets a password: with password login turned off the server refuses it, so the
  // action is offered only once the server has said password login is on (a server without the
  // route, 404, has only passwords): not while the answer is pending, nor after it failed.
  const methods = useLoad(
    () =>
      api<{ password: boolean }>('/api/login/methods').catch((e: unknown) => {
        if (e instanceof ApiError && e.status === 404) return { password: true }
        throw e
      }),
    [],
  )
  const passwordLogin = methods.data?.password === true

  const setRole = (m: Member, role: Role) =>
    action.run(() => api(`${base}/members/${m.id}`, { method: 'PATCH', body: { role } })).then(members.reload)
  const remove = (m: Member) =>
    action.run(() => api(`${base}/members/${m.id}`, { method: 'DELETE' })).then(members.reload)
  const withdraw = (i: Invite) =>
    action.run(() => api(`${base}/invites/${i.id}`, { method: 'DELETE' })).then(invites.reload)
  const clearTwoFactor = (m: Member) =>
    action.run(() => api(`${base}/members/${m.id}/two-factor`, { method: 'DELETE' })).then(members.reload)
  const [reset, setReset] = useState<{ member: Member; link: string; expires: string } | null>(null)
  const makeReset = async (m: Member) => {
    let made: { token: string; expires_at: string } | undefined
    const ok = await action.run(async () => {
      made = await api<{ token: string; expires_at: string }>(`${base}/members/${m.id}/password-reset`, {
        method: 'POST',
      })
    })
    if (ok && made) setReset({ member: m, link: resetLink(made.token), expires: made.expires_at })
  }

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-members">
        <h2 id="tt-members">{t('settings.members.title')}</h2>
        <Alert error={members.error ?? action.error} />
        {!members.data && !members.error && <p className="tt-muted">{t('app.loading')}</p>}
        {members.data && (
          <ul className="tt-rows">
            {members.data.map((m) => (
              <li key={m.id} className="tt-row">
                <div className="tt-row-main">
                  <strong>
                    {m.name}
                    {m.id === me && <span className="tt-muted"> {t('settings.members.you')}</span>}
                  </strong>
                  <span className="tt-muted tt-small">{m.email}</span>
                </div>
                <div className="tt-row-actions">
                  <label>
                    <span className="tt-sr-only">{t('settings.members.roleOf', { name: m.name })}</span>
                    <select
                      value={m.role}
                      disabled={m.id === me || action.busy}
                      onChange={(e) => setRole(m, e.target.value as Role)}
                    >
                      <option value="member">{t('settings.members.roles.member')}</option>
                      <option value="admin">{t('settings.members.roles.admin')}</option>
                    </select>
                  </label>
                  {m.id !== me && passwordLogin && (
                    <button
                      type="button"
                      className="tt-button"
                      aria-label={t('settings.members.resetLinkFor', { name: m.name })}
                      disabled={action.busy}
                      onClick={() => makeReset(m)}
                    >
                      {t('settings.members.resetLink')}
                    </button>
                  )}
                  {m.id !== me && m.two_factor && (
                    <ConfirmButton
                      label={t('settings.members.clearTwoFactor')}
                      name={m.name}
                      question={t('settings.members.clearTwoFactorQuestion', { name: m.name })}
                      confirm={t('settings.members.clearTwoFactorConfirm')}
                      disabled={action.busy}
                      onConfirm={() => clearTwoFactor(m)}
                    />
                  )}
                  {m.id !== me && (
                    <ConfirmButton
                      label={t('settings.members.remove')}
                      name={m.name}
                      question={t('settings.members.removeQuestion', { name: m.name })}
                      confirm={t('settings.members.removeConfirm')}
                      disabled={action.busy}
                      onConfirm={() => remove(m)}
                    />
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
        {reset && (
          <div className="tt-notice" role="status">
            <p>
              {t.rich('settings.members.resetNotice', {
                name: reset.member.name,
                email: reset.member.email,
                when: f.dateTime(reset.expires),
                strong: (text) => <strong>{text}</strong>,
              })}
            </p>
            <div className="tt-copy">
              <input
                readOnly
                value={reset.link}
                aria-label={t('settings.members.resetLinkField')}
                onFocus={(e) => e.target.select()}
              />
              <CopyButton text={reset.link} name={t('settings.members.resetLinkName')} />
            </div>
          </div>
        )}
        <p className="tt-muted tt-small">{t('settings.members.help')}</p>
      </section>

      <section className="tt-panel" aria-labelledby="tt-invites">
        <h2 id="tt-invites">{t('settings.members.invites')}</h2>
        <InviteForm base={base} onInvited={invites.reload} />
        <Alert error={invites.error} />
        {invites.data?.length === 0 && <p className="tt-muted">{t('settings.members.noInvites')}</p>}
        {invites.data && invites.data.length > 0 && (
          <ul className="tt-rows" aria-label={t('settings.members.pendingInvites')}>
            {invites.data.map((i) => (
              <li key={i.id} className="tt-row">
                <div className="tt-row-main">
                  <strong>{i.email}</strong>
                  <span className="tt-muted tt-small">
                    {t('settings.members.inviteDetails', {
                      role: t(i.role === 'admin' ? 'settings.members.roles.admin' : 'settings.members.roles.member'),
                      date: day(i.expires_at),
                    })}
                  </span>
                </div>
                <div className="tt-row-actions">
                  <ConfirmButton
                    label={t('settings.members.withdraw')}
                    name={t('settings.members.inviteFor', { email: i.email })}
                    question={t('settings.members.withdrawQuestion', { email: i.email })}
                    confirm={t('settings.members.withdrawConfirm')}
                    disabled={action.busy}
                    onConfirm={() => withdraw(i)}
                  />
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}

function InviteForm({ base, onInvited }: { base: string; onInvited: () => void }) {
  const t = useT()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('member')
  const [made, setMade] = useState<{ email: string; link: string; expires: string } | null>(null)
  const [copied, setCopied] = useState(false)
  const action = useAction()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let invite: (Invite & { token: string }) | undefined
    const ok = await action.run(async () => {
      invite = await api<Invite & { token: string }>(`${base}/invites`, { method: 'POST', body: { email, role } })
    })
    if (ok && invite) {
      setMade({ email: invite.email, link: inviteLink(invite.token), expires: invite.expires_at })
      setCopied(false)
      setEmail('')
      onInvited()
    }
  }
  const copy = async (link: string) => {
    try {
      await navigator.clipboard.writeText(link)
      setCopied(true)
    } catch {
      // No clipboard (plain http, or refused): the link is selectable.
    }
  }

  return (
    <>
      <form className="tt-form tt-form-row" onSubmit={submit}>
        <label>
          {t('auth.email')}
          <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="off" />
        </label>
        <label>
          {t('settings.members.role')}
          <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="member">{t('settings.members.roles.member')}</option>
            <option value="admin">{t('settings.members.roles.admin')}</option>
          </select>
        </label>
        <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
          {action.busy ? t('settings.members.inviting') : t('settings.members.invite')}
        </button>
      </form>
      <Alert error={action.error} />
      {made && (
        <div className="tt-notice" role="status">
          <p>
            {t.rich('settings.members.inviteMade', { email: <strong>{made.email}</strong>, date: day(made.expires) })}
          </p>
          <div className="tt-copy">
            <input
              readOnly
              value={made.link}
              aria-label={t('settings.members.inviteLink')}
              onFocus={(e) => e.target.select()}
            />
            <button type="button" className="tt-button" onClick={() => copy(made.link)}>
              {copied ? t('common.copied') : t('common.copy')}
            </button>
          </div>
        </div>
      )}
    </>
  )
}
