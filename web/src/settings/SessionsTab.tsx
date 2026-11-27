import { api, type Session } from '../api'
import { type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Alert, Loading, SettingsTab, useAction } from './parts'

export function SessionsTab() {
  const sessions = useLoad(() => api<{ sessions: Session[] }>('/api/me/sessions').then((r) => r.sessions), [])
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const f = useFormat()
  const t = useT()

  const run = (change: () => Promise<unknown>) =>
    action.run(change).then((ok) => {
      sessions.reload()
      return ok
    })
  const revoke = async (s: Session, device: string) => {
    const yes = await confirm({
      title: t('settings.sessions.revokeQuestion', { name: device }),
      confirmLabel: t('settings.sessions.revokeConfirm'),
      tone: 'danger',
    })
    if (yes) run(() => api(`/api/me/sessions/${s.id}`, { method: 'DELETE' }))
  }
  const revokeOthers = async () => {
    const yes = await confirm({
      title: t('settings.sessions.revokeOthersQuestion'),
      confirmLabel: t('settings.sessions.revokeOthersConfirm'),
      tone: 'danger',
    })
    if (yes) run(() => api('/api/me/sessions', { method: 'DELETE' }))
  }
  const others = sessions.data?.filter((s) => !s.current).length ?? 0

  return (
    <SettingsTab
      title={t('settings.sessions.title')}
      help={t('settings.sessions.intro')}
      actions={
        others > 0 && (
          <Button variant="danger" disabled={action.busy} onClick={revokeOthers}>
            {t('settings.sessions.revokeOthers')}
          </Button>
        )
      }
    >
      <Alert error={sessions.error ?? action.error} />
      {!sessions.data && !sessions.error && <Loading />}
      {sessions.data && (
        <Panel>
          <Rows label={t('settings.sessions.yours')}>
            {sessions.data.map((s) => {
              const device = deviceName(t, s)
              return (
                <Row
                  key={s.id}
                  title={device}
                  detail={`${t('settings.sessions.created', { date: f.dateTime(s.created_at) })} · ${t(
                    'settings.sessions.lastSeen',
                    { when: f.relative(s.last_seen_at) },
                  )}`}
                  status={
                    s.current && (
                      <Badge tone="success" dot>
                        {t('settings.sessions.current')}
                      </Badge>
                    )
                  }
                  actions={
                    !s.current && (
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label={t('common.labelFor', { label: t('settings.sessions.revoke'), name: device })}
                        disabled={action.busy}
                        onClick={() => revoke(s, device)}
                      >
                        {t('settings.sessions.revoke')}
                      </Button>
                    )
                  }
                />
              )
            })}
          </Rows>
        </Panel>
      )}
      {sessions.data && others === 0 && <p className="settings-muted">{t('settings.sessions.noOthers')}</p>}
      {confirmDialog}
    </SettingsTab>
  )
}

function deviceName(t: T, s: Session): string {
  if (s.browser && s.os) return t('settings.sessions.device', { browser: s.browser, os: s.os })
  return s.browser || s.os || t('settings.sessions.unknown')
}
