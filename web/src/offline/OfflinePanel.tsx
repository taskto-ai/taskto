// "Offline use" on an app's page: download it for use without a connection, see whether it is
// ready and how much room is left, and remove the copy. Building with AI, publishing and sharing
// are said to stay online.

import { useCallback, useEffect, useState } from 'react'
import { ApiError, type App, errorText, type Space } from '../api'
import { unsupportedBy } from '../apps/catalog'
import { useFormat, useT } from '../i18n'
import { downloadApp, type StorageStatus, storageStatus, UnsupportedRelease } from './download'
import { useOfflineData, useRuntime } from './hooks'
import { OutboxList } from './OutboxList'
import { NotSaved, StaleDownload, scopeKey } from './store'
import { notSavedText } from './transport'
import { WriterNotice } from './WriterNotice'

export function OfflinePanel({
  workspace,
  space,
  app,
  spaceInfo,
}: {
  workspace: number
  space: number
  app: App
  // The space as the caller sees it, which says whether an admin turned downloads off.
  spaceInfo?: Space
}) {
  const t = useT()
  const f = useFormat()
  const { runtime, problem } = useRuntime()
  const scope = { workspace, space, app: app.id }
  const data = useOfflineData(runtime, scope)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [storage, setStorage] = useState<StorageStatus>({})
  const copy = data.downloads.find((d) => d.app === app.id && d.space === space && d.workspace === workspace)
  const live = app.live
  const refresh = useCallback(() => void storageStatus().then(setStorage), [])
  // biome-ignore lint/correctness/useExhaustiveDependencies: a changed copy changes what is used
  useEffect(refresh, [refresh, copy?.capturedAt])

  const disabled = spaceInfo?.offline_downloads === false
  const unsupported = live ? unsupportedBy(live) : []

  const download = async () => {
    if (!runtime) return
    setBusy(true)
    setError(null)
    try {
      await downloadApp(runtime.store, scope)
    } catch (e) {
      // Overtaken by a removal that began later (the copy was revoked): the panel shows what is held.
      if (e instanceof StaleDownload) return
      setError(
        e instanceof NotSaved
          ? notSavedText(e)
          : e instanceof UnsupportedRelease
            ? t('apps.updateRequired.message')
            : e instanceof ApiError || e instanceof Error
              ? errorText(e)
              : String(e),
      )
    } finally {
      setBusy(false)
      refresh()
    }
  }
  const remove = async () => {
    if (!runtime) return
    try {
      await runtime.store.removeDownload(scope)
    } catch (e) {
      setError(errorText(e))
    }
  }

  const apps = { ...Object.fromEntries(data.downloads.map((d) => [d.key, d.name])), [scopeKey(scope)]: app.name }
  const stale = copy && live && live.number > copy.release

  return (
    <section className="tt-panel tt-offline" aria-label={t('offline.title')}>
      <h2>{t('offline.title')}</h2>
      {problem === 'schema' && (
        <p role="alert" className="tt-error">
          {t('offline.schemaNewer')}
        </p>
      )}
      {problem === 'unavailable' && <p className="tt-muted">{t('offline.unavailable')}</p>}
      {disabled && <p className="tt-muted">{t('offline.disabled')}</p>}
      {runtime && !disabled && (
        <>
          {copy ? (
            <div className="tt-offline-ready">
              <p role="status">
                <strong>{t('offline.ready')}</strong>
              </p>
              <p className="tt-muted tt-small">
                {t('offline.readyDetail', {
                  release: copy.release,
                  when: f.dateTime(copy.capturedAt),
                  records: f.number(copy.records),
                  total: f.number(copy.total),
                })}
              </p>
              {copy.truncated && (
                <p className="tt-muted tt-small">
                  {t('offline.truncated', { records: f.number(copy.records), total: f.number(copy.total) })}
                </p>
              )}
              {copy.scopeOf === 'own' && <p className="tt-muted tt-small">{t('offline.ownOnly')}</p>}
              {!copy.canInsert && <p className="tt-muted tt-small">{t('offline.cantInsert')}</p>}
              {stale && live && <p className="tt-muted tt-small">{t('offline.newerRelease', { live: live.number })}</p>}
            </div>
          ) : (
            <p className="tt-muted">{t('offline.notDownloaded')}</p>
          )}
          {unsupported.length > 0 && <p className="tt-muted tt-small">{t('apps.updateRequired.message')}</p>}
          <div className="tt-row-actions">
            <button
              type="button"
              className="tt-button tt-primary"
              disabled={busy || !live || unsupported.length > 0}
              aria-label={
                copy
                  ? t('offline.downloadAgainNamed', { name: app.name })
                  : t('offline.downloadNamed', { name: app.name })
              }
              onClick={() => void download()}
            >
              {busy ? t('offline.downloading') : copy ? t('offline.downloadAgain') : t('offline.download')}
            </button>
            {copy && (
              <button
                type="button"
                className="tt-button"
                aria-label={t('offline.removeNamed', { name: app.name })}
                onClick={() => void remove()}
              >
                {t('offline.remove')}
              </button>
            )}
          </div>
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <p className="tt-muted tt-small">
            {storage.usage !== undefined && storage.quota !== undefined
              ? t('offline.storage', { usage: f.bytes(storage.usage), quota: f.bytes(storage.quota) })
              : t('offline.storageUnknown')}{' '}
            {storage.persisted === true
              ? t('offline.persisted')
              : storage.persisted === false
                ? t('offline.notPersisted')
                : ''}
          </p>
        </>
      )}
      <p className="tt-muted tt-small">{t('offline.onlineOnly')}</p>
      {runtime && data.items.length > 0 && (
        <>
          <WriterNotice runtime={runtime} />
          <OutboxList
            runtime={runtime}
            items={data.items}
            status={data.status}
            apps={apps}
            downloads={data.downloads}
          />
        </>
      )}
    </section>
  )
}
