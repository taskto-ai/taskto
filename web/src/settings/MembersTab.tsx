// Members and invites, for admins. An invite's link, or a member's password reset link, is
// shown once, when it is made: the server keeps only a hash of its token. The token rides in
// the URL's fragment, which browsers don't send to servers or in Referer.

import { type FormEvent, useState } from 'react'
import { ApiError, api, type Invite, type InviteCreated, type MailStatus, type Member, mailPath, ws } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Avatar } from '../ui/Avatar'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { useConfirm } from '../ui/ConfirmDialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { MenuItem, MenuSeparator, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Table } from '../ui/Table'
import { Alert, CopyField, day, Loading, Section, SettingsTab, useAction } from './parts'

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
  const [confirmDialog, confirm] = useConfirm()
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

  const askDisable = async (m: Member) => {
    const yes = await confirm({
      title: t('common.labelFor', { label: t('settings.members.disable'), name: m.name }),
      body: t('settings.members.disableQuestion', { name: m.name }),
      confirmLabel: t('settings.members.disableConfirm'),
      tone: 'danger',
    })
    if (yes) setDisabled(m, true)
  }
  const askClearTwoFactor = async (m: Member) => {
    const yes = await confirm({
      title: t('common.labelFor', { label: t('settings.members.clearTwoFactor'), name: m.name }),
      body: t('settings.members.clearTwoFactorQuestion', { name: m.name }),
      confirmLabel: t('settings.members.clearTwoFactorConfirm'),
      tone: 'danger',
    })
    if (yes) clearTwoFactor(m)
  }
  const askRemove = async (m: Member) => {
    const yes = await confirm({
      title: t('settings.members.removeQuestion', { name: m.name }),
      confirmLabel: t('settings.members.removeConfirm'),
      tone: 'danger',
    })
    if (yes) remove(m)
  }
  const askWithdraw = async (i: Invite) => {
    const yes = await confirm({
      title: t('settings.members.withdrawQuestion', { email: i.email }),
      confirmLabel: t('settings.members.withdrawConfirm'),
      tone: 'danger',
    })
    if (yes) withdraw(i)
  }

  return (
    <SettingsTab title={t('settings.members.title')} help={t('settings.help.members')}>
      <Section label={t('settings.members.title')}>
        <Alert error={members.error ?? action.error} />
        {!members.data && !members.error && <Loading />}
        {members.data && (
          <Panel>
            <Table
              caption={t('settings.members.title')}
              rows={members.data}
              rowKey={(m) => m.id}
              columns={[
                {
                  key: 'person',
                  header: t('settings.members.person'),
                  cell: (m) => (
                    <span className="settings-person">
                      <Avatar name={m.name} size="sm" decorative />
                      <span className="settings-person-text">
                        <span className="settings-person-name">
                          {m.name}
                          {m.id === me && <Badge>{t('settings.members.youBadge')}</Badge>}
                          {m.disabled && <Badge tone="warning">{t('settings.members.disabled')}</Badge>}
                        </span>
                        <span className="settings-muted">{m.email}</span>
                      </span>
                    </span>
                  ),
                },
                {
                  key: 'role',
                  header: t('settings.members.role'),
                  cell: (m) => (
                    <Select
                      size="sm"
                      className="settings-role"
                      aria-label={t('settings.members.roleOf', { name: m.name })}
                      value={m.role}
                      disabled={m.id === me || action.busy}
                      onChange={(e) => setRole(m, e.target.value as Role)}
                    >
                      <option value="member">{t('settings.members.roles.member')}</option>
                      <option value="admin">{t('settings.members.roles.admin')}</option>
                    </Select>
                  ),
                },
                {
                  key: 'actions',
                  header: t('settings.actionsColumn'),
                  hiddenHeader: true,
                  cell: (m) =>
                    m.id !== me && (
                      <span className="settings-cell-end">
                        <MoreMenu label={t('settings.moreFor', { name: m.name })}>
                          {passwordLogin && (
                            <MenuItem icon="edit" onSelect={() => makeReset(m)} disabled={action.busy}>
                              {t('settings.members.resetLink')}
                            </MenuItem>
                          )}
                          {m.disabled ? (
                            <MenuItem icon="check" onSelect={() => setDisabled(m, false)} disabled={action.busy}>
                              {t('settings.members.enable')}
                            </MenuItem>
                          ) : (
                            <MenuItem icon="block" onSelect={() => askDisable(m)} disabled={action.busy}>
                              {t('settings.members.disable')}
                            </MenuItem>
                          )}
                          {m.two_factor && (
                            <MenuItem onSelect={() => askClearTwoFactor(m)} disabled={action.busy}>
                              {t('settings.members.clearTwoFactor')}
                            </MenuItem>
                          )}
                          <MenuSeparator />
                          <MenuItem tone="danger" onSelect={() => askRemove(m)} disabled={action.busy}>
                            {t('settings.members.remove')}
                          </MenuItem>
                        </MoreMenu>
                      </span>
                    ),
                },
              ]}
            />
          </Panel>
        )}
        {reset && (
          <Notice tone="warning" title={t('settings.members.resetLinkFor', { name: reset.member.name })}>
            <div className="settings-panel-body">
              <p className="settings-text">
                {t.rich('settings.members.resetNotice', {
                  name: reset.member.name,
                  email: reset.member.email,
                  when: f.dateTime(reset.expires),
                  strong: (text) => <strong>{text}</strong>,
                })}
              </p>
              <CopyField
                value={reset.link}
                label={t('settings.members.resetLinkField')}
                name={t('settings.members.resetLinkName')}
              />
            </div>
          </Notice>
        )}
        <p className="settings-muted">{t('settings.members.help')}</p>
      </Section>

      <Section title={t('settings.members.invites')} help={t('settings.members.invitesHelp')}>
        <Panel>
          <InviteForm workspace={workspace} base={base} onInvited={invites.reload} />
          <Alert error={invites.error} />
          {invites.data?.length === 0 && (
            <p className="settings-muted settings-panel-note">{t('settings.members.noInvites')}</p>
          )}
          {invites.data && invites.data.length > 0 && (
            <Rows label={t('settings.members.pendingInvites')}>
              {invites.data.map((i) => (
                <Row
                  key={i.id}
                  leading={<Avatar name={i.email} size="sm" decorative />}
                  title={i.email}
                  detail={t('settings.members.inviteDetails', {
                    role: t(i.role === 'admin' ? 'settings.members.roles.admin' : 'settings.members.roles.member'),
                    date: day(i.expires_at),
                  })}
                  status={
                    <Badge tone="warning" dot>
                      {t('settings.members.pending')}
                    </Badge>
                  }
                  actions={
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={t('common.labelFor', {
                        label: t('settings.members.withdraw'),
                        name: t('settings.members.inviteFor', { email: i.email }),
                      })}
                      disabled={action.busy}
                      onClick={() => askWithdraw(i)}
                    >
                      {t('settings.members.withdraw')}
                    </Button>
                  }
                />
              ))}
            </Rows>
          )}
        </Panel>
      </Section>
      {confirmDialog}
    </SettingsTab>
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
      setEmail('')
      onInvited()
    }
  }

  return (
    <div className="settings-invite">
      <form className="settings-form" onSubmit={submit}>
        <div className="settings-form-row">
          <Field label={t('auth.email')}>
            <Input
              required
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="off"
              placeholder={t('settings.members.emailPlaceholder')}
            />
          </Field>
          <Field label={t('settings.members.role')} className="settings-field-narrow">
            <Select value={role} onChange={(e) => setRole(e.target.value as Role)}>
              <option value="member">{t('settings.members.roles.member')}</option>
              <option value="admin">{t('settings.members.roles.admin')}</option>
            </Select>
          </Field>
          <Button type="submit" variant="primary" loading={action.busy}>
            {t('settings.members.invite')}
          </Button>
        </div>
        {canEmail && (
          <Checkbox
            label={t('settings.members.inviteSendEmail')}
            checked={sendEmail}
            onChange={(e) => setSendEmail(e.target.checked)}
          />
        )}
      </form>
      {mail.data && !canEmail && <Notice tone="neutral">{t('settings.members.noEmail')}</Notice>}
      <Alert error={action.error} />
      {made && (
        <Notice tone={made.emailed === false ? 'warning' : 'success'} title={t('settings.members.inviteLink')}>
          <div className="settings-panel-body">
            <p className="settings-text">
              {t.rich(
                made.emailed === true
                  ? 'settings.members.inviteEmailed'
                  : made.emailed === false
                    ? 'settings.members.inviteEmailFailed'
                    : 'settings.members.inviteMade',
                { email: <strong>{made.email}</strong>, date: day(made.expires) },
              )}
            </p>
            <CopyField value={made.link} label={t('settings.members.inviteLink')} />
          </div>
        </Notice>
      )}
    </div>
  )
}
