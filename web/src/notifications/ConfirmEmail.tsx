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

type Outcome =
  | { state: 'working' }
  | { state: 'done' }
  | { state: 'failed'; message: string | null }
  | { state: 'nothing' }

export function ConfirmEmail({ token, workspace }: { token: string; workspace: number | null }) {
  const t = useT()
  const [opened] = useState(token)
  const [outcome, setOutcome] = useState<Outcome>(opened ? { state: 'working' } : { state: 'nothing' })
  const request = useRef<Promise<unknown> | null>(null)
  useEffect(() => {
    let live = true
    window.history.replaceState(null, '', window.location.pathname)
    if (!opened) return
    request.current ??= api(EMAIL_CONFIRM_API, { method: 'POST', body: { token: opened } })
    request.current.then(
      () => live && setOutcome({ state: 'done' }),
      (e) => live && setOutcome({ state: 'failed', message: e instanceof ApiError ? errorText(e) : null }),
    )
    return () => {
      live = false
    }
  }, [opened])
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
      {outcome.state === 'nothing' && (
        <Notice tone="neutral" live={false}>
          {t('notifications.confirmEmail.nothing')}
        </Notice>
      )}
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
