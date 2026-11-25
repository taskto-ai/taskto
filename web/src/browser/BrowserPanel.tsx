import { useState } from 'react'
import { api, BROWSER, type BrowserStatus, codeText, errorText } from '../api'
import { useT } from '../i18n'
import type { Loaded } from '../load'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import './Browser.css'
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
    <section className="ui-browser" aria-label={t('browser.yours')}>
      <div className="ui-browser-head">
        <Heading level={3} size="subsection" className="ui-browser-title">
          {t('browser.yours')}
        </Heading>
        {personal?.running && (
          <>
            <Button size="sm" variant="ghost" onClick={() => onInteractive(!interactive)}>
              {interactive ? t('browser.justWatch') : t('browser.takeOver')}
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => act('stop')}>
              {t('browser.stop')}
            </Button>
          </>
        )}
        <IconButton icon="close" size="sm" label={t('browser.close')} onClick={onClose} />
      </div>
      {(error ?? status.error) && <Notice tone="danger">{error ?? status.error}</Notice>}
      {st && !st.enabled && (
        <p className="ui-browser-note">{st.reason && codeText(st.reason_code, undefined, st.reason)}</p>
      )}
      {st?.enabled && !personal?.available && <p className="ui-browser-note">{t('browser.notInstalled')}</p>}
      {st?.enabled && personal?.available && !personal.running && (
        <div className="ui-browser-start">
          <p className="ui-browser-note">{t('browser.notRunning')}</p>
          <Button size="sm" disabled={busy} onClick={() => act('start')}>
            {t('browser.start')}
          </Button>
        </div>
      )}
      {personal?.running && (
        <>
          {interactive && <Notice tone="info">{t('browser.youHaveIt')}</Notice>}
          <LiveView interactive={interactive} load={load} onClosed={status.reload} />
        </>
      )}
    </section>
  )
}
