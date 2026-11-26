// The page a confirmation email's link opens (docs/pwa.md#email): the token follows the `#`, so no
// server log has it. It confirms the signed-in person's own address, once, and takes the token out
// of the address bar whatever comes of it.

import { useEffect, useRef, useState } from 'react'
import { ApiError, api, errorText } from '../api'
import { useT } from '../i18n'
import { routePath } from '../router'
import { ButtonLink } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { EMAIL_CONFIRM_API } from './api'

type Outcome = { state: 'working' } | { state: 'done' } | { state: 'failed'; message: string | null }

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
      // An answer of the server's says what it refused; no answer is said in the app's words when
      // drawn, never the browser's ("Failed to fetch").
      (e) => live && setOutcome({ state: 'failed', message: e instanceof ApiError ? errorText(e) : null }),
    )
    return () => {
      live = false
    }
  }, [token])
  return (
    <Page width="reading">
      <PageHeader title={t('notifications.confirmEmail.title')} />
      {outcome.state === 'working' && (
        <div role="status" aria-busy="true">
          <VisuallyHidden>{t('notifications.confirmEmail.working')}</VisuallyHidden>
          <Skeleton width="60%" />
        </div>
      )}
      {outcome.state === 'done' && <Notice tone="success">{t('notifications.confirmEmail.done')}</Notice>}
      {outcome.state === 'failed' && (
        <Notice tone="danger">{outcome.message ?? t('notifications.confirmEmail.failed')}</Notice>
      )}
      {workspace !== null && (
        <div>
          <ButtonLink href={routePath({ workspace, section: 'settings', item: 'notifications' })}>
            {t('notifications.confirmEmail.settings')}
          </ButtonLink>
        </div>
      )}
    </Page>
  )
}
