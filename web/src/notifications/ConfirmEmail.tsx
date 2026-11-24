// The page a confirmation email's link opens (docs/pwa.md#email): the token follows the `#`, so no
// server log has it. It confirms the signed-in person's own address, once, and takes the token out
// of the address bar whatever comes of it.

import { useEffect, useRef, useState } from 'react'
import { api, errorText } from '../api'
import { useT } from '../i18n'
import { Link, routePath } from '../router'
import { EMAIL_CONFIRM_API } from './api'

type Outcome = { state: 'working' } | { state: 'done' } | { state: 'failed'; message: string }

export function ConfirmEmail({ token, workspace }: { token: string; workspace: number | null }) {
  const t = useT()
  const [outcome, setOutcome] = useState<Outcome>({ state: 'working' })
  // One request however often the effect runs: the token works once.
  const request = useRef<Promise<unknown> | null>(null)
  useEffect(() => {
    let live = true
    window.history.replaceState(null, '', window.location.pathname)
    request.current ??= api(EMAIL_CONFIRM_API, { method: 'POST', body: { token } })
    request.current.then(
      () => live && setOutcome({ state: 'done' }),
      (e) => live && setOutcome({ state: 'failed', message: errorText(e) }),
    )
    return () => {
      live = false
    }
  }, [token])
  return (
    <div className="tt-center">
      <h1>{t('notifications.confirmEmail.title')}</h1>
      {outcome.state === 'working' && <p className="tt-muted">{t('notifications.confirmEmail.working')}</p>}
      {outcome.state === 'done' && <p role="status">{t('notifications.confirmEmail.done')}</p>}
      {outcome.state === 'failed' && <p role="alert">{outcome.message}</p>}
      {workspace !== null && (
        <Link className="tt-button" href={routePath({ workspace, section: 'settings', item: 'notifications' })}>
          {t('notifications.confirmEmail.settings')}
        </Link>
      )}
    </div>
  )
}
