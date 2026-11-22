import { useState } from 'react'
import { api, BROWSER, type BrowserStatus, codeText, errorText } from '../api'
import { useT } from '../i18n'
import type { Loaded } from '../load'
import { LiveView } from './LiveView'
import type { RFBConstructor } from './novnc'

// BrowserPanel is the user's own browser, beside a conversation: whether it runs, its live
// view, and the controls to take over, start it or stop it.
export function BrowserPanel({
  status,
  interactive,
  onInteractive,
  onClose,
  load,
}: {
  status: Loaded<BrowserStatus>
  interactive: boolean
  onInteractive: (on: boolean) => void
  onClose: () => void
  load?: () => Promise<RFBConstructor>
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const st = status.data
  const personal = st?.browsers.personal
  const act = async (what: 'start' | 'stop') => {
    setBusy(true)
    setError(null)
    try {
      await api(`${BROWSER}/${what}`, { method: 'POST', body: { browser: 'personal' } })
      if (what === 'stop') onInteractive(false)
      status.reload()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="tt-browser" aria-label={t('browser.yours')}>
      <div className="tt-browser-head">
        <h3>{t('browser.yours')}</h3>
        {personal?.running && (
          <>
            <button type="button" className="tt-button" onClick={() => onInteractive(!interactive)}>
              {interactive ? t('browser.justWatch') : t('browser.takeOver')}
            </button>
            <button type="button" className="tt-button" disabled={busy} onClick={() => act('stop')}>
              {t('browser.stop')}
            </button>
          </>
        )}
        <button type="button" className="tt-button" onClick={onClose}>
          {t('browser.close')}
        </button>
      </div>
      {(error ?? status.error) && (
        <p role="alert" className="tt-error">
          {error ?? status.error}
        </p>
      )}
      {st && !st.enabled && <p className="tt-muted">{st.reason && codeText(st.reason_code, undefined, st.reason)}</p>}
      {st?.enabled && !personal?.available && <p className="tt-muted">{t('browser.notInstalled')}</p>}
      {st?.enabled && personal?.available && !personal.running && (
        <p>
          {t('browser.notRunning')}{' '}
          <button type="button" className="tt-button" disabled={busy} onClick={() => act('start')}>
            {t('browser.start')}
          </button>
        </p>
      )}
      {personal?.running && (
        <>
          {interactive && <p className="tt-muted tt-small">{t('browser.youHaveIt')}</p>}
          <LiveView interactive={interactive} load={load} onClosed={status.reload} />
        </>
      )}
    </section>
  )
}
