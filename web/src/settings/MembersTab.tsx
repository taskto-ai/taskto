// Members and invites, for admins. An invite's link, or a member's password reset link, is
// shown once, when it is made: the server keeps only a hash of its token. The token rides in
// the URL's fragment, which browsers don't send to servers or in Referer.

import { type FormEvent, useState } from 'react'
import {
  ApiError,
  api,
  type Invite,
  type InviteCreated,
  type MailStatus,
  type Member,
  mailPath,
  type PendingDeletion,
  ws,
} from '../api'
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

export function MembersTab({ workspace, me, operator = false }: { workspace: number; me: number; operator?: boolean }) {
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

  // The accounts waiting to be deleted, which only the server's operators see and can cancel.
  const deletions = useLoad(
    () =>
      operator
        ? api<{ deletions: PendingDeletion[] }>(`${base}/account-deletions`).then((r) => r.deletions)
        : Promise.resolve([] as PendingDeletion[]),
    [base, operator],
  )
  const deleteAccount = (m: Member) =>
    action
      .run(() => api(`${base}/members/${m.id}/deletion`, { method: 'POST' }))
      .then(() => Promise.all([members.reload(), deletions.reload()]))
  const cancelDeletion = (user: number) =>
    action
      .run(() => api(`${base}/account-deletions/${user}`, { method: 'DELETE' }))
      .then(() => Promise.all([members.reload(), deletions.reload()]))
  const setRole = (m: Member, role: Role) =>
    action.run(() => api(`${base}/members/${m.id}`, { method: 'PATCH', body: { role } })).then(members.reload)
  const remove = (m: Member) =>
    action.run(() => api(`${base}/members/${m.id}`, { method: 'DELETE' })).then(members.reload)
  const setDisabled = (m: Member, disabled: boolean) =>
    action
      .run(() => api(`${base}/members/${m.id}/${disabled ? 'disable' : 'enable'}`, { method: 'POST' }))
      .then(members.reload)
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
                    {m.deletion_at ? (
                      <span className="tt-badge">
                        {' '}
                        {t('settings.members.deletionPending', { date: f.date(m.deletion_at) })}
                      </span>
                    ) : (
                      m.disabled && <span className="tt-badge"> {t('settings.members.disabled')}</span>
                    )}
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
                  {m.id !== me &&
                    !m.deletion_at &&
                    (m.disabled ? (
                      <button
                        type="button"
                        className="tt-button"
                        aria-label={t('common.labelFor', { label: t('settings.members.enable'), name: m.name })}
                        disabled={action.busy}
                        onClick={() => setDisabled(m, false)}
                      >
                        {t('settings.members.enable')}
                      </button>
                    ) : (
                      <ConfirmButton
                        label={t('settings.members.disable')}
                        name={m.name}
                        question={t('settings.members.disableQuestion', { name: m.name })}
                        confirm={t('settings.members.disableConfirm')}
                        disabled={action.busy}
                        onConfirm={() => setDisabled(m, true)}
                      />
                    ))}
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
                  {m.id !== me && !m.deletion_at && (
                    <ConfirmButton
                      label={t('settings.members.deleteAccount')}
                      name={m.name}
                      question={t('settings.members.deleteAccountQuestion', { name: m.name })}
                      confirm={t('settings.members.deleteAccountConfirm')}
                      disabled={action.busy}
                      onConfirm={() => deleteAccount(m)}
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

      {operator && (
        <section className="tt-panel" aria-labelledby="tt-deletions">
          <h2 id="tt-deletions">{t('settings.members.deletions')}</h2>
          <p className="tt-muted tt-small">{t('settings.members.deletionsHelp')}</p>
          <Alert error={deletions.error} />
          {deletions.data?.length === 0 && <p className="tt-muted">{t('settings.members.noDeletions')}</p>}
          {deletions.data && deletions.data.length > 0 && (
            <ul className="tt-rows" aria-label={t('settings.members.deletions')}>
              {deletions.data.map((d) => (
                <li key={d.user.id} className="tt-row">
                  <div className="tt-row-main">
                    <strong>{d.user.name}</strong>
                    <span className="tt-muted tt-small">{d.user.email}</span>
                    <span className="tt-muted tt-small">
                      {t(d.purging ? 'settings.members.deletionPurging' : 'settings.members.deletionDetails', {
                        date: f.date(d.purge_at),
                        by: t(d.by_self ? 'settings.members.deletedBySelf' : 'settings.members.deletedByAdmin'),
                      })}
                    </span>
                  </div>
                  <div className="tt-row-actions">
                    {!d.purging && (
                      <ConfirmButton
                        label={t('settings.members.cancelDeletion')}
                        name={d.user.name}
                        question={t('settings.members.cancelDeletionQuestion', { name: d.user.name })}
                        confirm={t('settings.members.cancelDeletionConfirm')}
                        disabled={action.busy}
                        onConfirm={() => cancelDeletion(d.user.id)}
                      />
                    )}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>
      )}

      <section className="tt-panel" aria-labelledby="tt-invites">
        <h2 id="tt-invites">{t('settings.members.invites')}</h2>
        <InviteForm workspace={workspace} base={base} onInvited={invites.reload} />
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

function InviteForm({ workspace, base, onInvited }: { workspace: number; base: string; onInvited: () => void }) {
  const t = useT()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('member')
  const [made, setMade] = useState<{
    email: string
    link: string
    expires: string
    // Whether the email was asked for, and went: null when it wasn't asked for.
    emailed: boolean | null
  } | null>(null)
  const [copied, setCopied] = useState(false)
  const [sendEmail, setSendEmail] = useState(true)
  const action = useAction()
  // Offered only where the server can email links; any failure to know says no, and the invite is
  // made and copied by hand, as it always was.
  const mail = useLoad(
    () => api<MailStatus>(mailPath(workspace)).catch(() => ({ configured: false }) as MailStatus),
    [workspace],
  )
  const canEmail = mail.data?.sends_links === true

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let invite: InviteCreated | undefined
    const ask = canEmail && sendEmail
    const ok = await action.run(async () => {
      invite = await api<InviteCreated>(`${base}/invites`, {
        method: 'POST',
        body: ask ? { email, role, send_email: true } : { email, role },
      })
    })
    if (ok && invite) {
      setMade({
        email: invite.email,
        link: inviteLink(invite.token),
        expires: invite.expires_at,
        emailed: ask ? invite.email_sent === true : null,
      })
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
        {canEmail && (
          <label className="tt-check">
            <input type="checkbox" checked={sendEmail} onChange={(e) => setSendEmail(e.target.checked)} />
            {t('settings.members.inviteSendEmail')}
          </label>
        )}
      </form>
      <Alert error={action.error} />
      {made && (
        <div className="tt-notice" role="status">
          <p>
            {t.rich(
              made.emailed === true
                ? 'settings.members.inviteEmailed'
                : made.emailed === false
                  ? 'settings.members.inviteEmailFailed'
                  : 'settings.members.inviteMade',
              { email: <strong>{made.email}</strong>, date: day(made.expires) },
            )}
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
