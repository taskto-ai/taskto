import { useState } from 'react'
import { ApiError, type App, type AppRelease, api, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { LoadingRows } from './AppsLoading'
import './Apps.css'

export function AppReleases({
  base,
  app,
  canRollBack,
  changed,
}: {
  base: string
  app: App
  canRollBack: boolean
  changed: () => void
}) {
  const t = useT()
  const f = useFormat()
  const loaded = useLoad(
    () => api<{ releases: AppRelease[] }>(`${base}/releases`).then((r) => r.releases),
    [base, app.version],
  )
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirmDialog, confirm] = useConfirm()

  const rollBack = async (release: AppRelease) => {
    const yes = await confirm({
      title: t('apps.releases.rollBackNamed', { number: release.number }),
      body: t('apps.releases.rollBackNote'),
      confirmLabel: t('apps.releases.rollBack'),
    })
    if (!yes) return
    setBusy(true)
    try {
      await api<App>(`${base}/rollback`, { method: 'POST', body: { version: app.version, release: release.number } })
      setError(null)
    } catch (e) {
      setError(
        e instanceof ApiError && e.status === 409 && e.code === 'app_changed'
          ? t('apps.changed', { name: app.name })
          : errorText(e),
      )
    }
    setBusy(false)
    changed()
  }

  return (
    <div className="apps-stack">
      {(loaded.error || error) && <Notice tone="danger">{error ?? loaded.error}</Notice>}
      {!loaded.data && !loaded.error && <LoadingRows count={2} />}
      {loaded.data && loaded.data.length === 0 && <p className="apps-note">{t('apps.releases.none')}</p>}
      {loaded.data && loaded.data.length > 0 && (
        <Panel>
          <Rows label={t('apps.releases.title')}>
            {loaded.data.map((r) => (
              <Row
                key={r.number}
                title={t('apps.releases.number', { number: r.number })}
                detail={
                  r.published_by
                    ? t('apps.releases.publishedBy', { name: r.published_by.name, date: f.dateTime(r.published_at) })
                    : t('apps.releases.published', { date: f.dateTime(r.published_at) })
                }
                status={
                  r.live && (
                    <Badge tone="success" dot>
                      {t('apps.releases.live')}
                    </Badge>
                  )
                }
                actions={
                  canRollBack &&
                  !r.live && (
                    <Button
                      size="sm"
                      disabled={busy}
                      onClick={() => rollBack(r)}
                      aria-label={t('apps.releases.rollBackNamed', { number: r.number })}
                    >
                      {t('apps.releases.rollBack')}
                    </Button>
                  )
                }
              />
            ))}
          </Rows>
        </Panel>
      )}
      {canRollBack && <p className="apps-note">{t('apps.releases.rollBackNote')}</p>}
      {confirmDialog}
    </div>
  )
}
