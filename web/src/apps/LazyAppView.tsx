// The app's screen, its renderer loaded when it is first drawn: Home and the Apps list draw one
// only when there is an app to show (a widget, an app opened), so a Home without an app widget and
// the list with no app open never load the renderer (A2UI and its schema libraries). While it
// loads, the shape AppView itself shows while its screen loads.

import { type ComponentProps, lazy, Suspense } from 'react'
import { useT } from '../i18n'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'

const AppView = lazy(() => import('./AppView').then((m) => ({ default: m.AppView })))

export function LazyAppView(props: ComponentProps<typeof AppView>) {
  return (
    <Suspense fallback={<AppViewLoading />}>
      <AppView {...props} />
    </Suspense>
  )
}

function AppViewLoading() {
  const t = useT()
  return (
    <div className="ui-app-loading" role="status">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      <Skeleton width="40%" />
      <Skeleton shape="block" height="var(--control-md)" />
      <Skeleton shape="block" height="var(--control-md)" />
      <Skeleton shape="block" width="30%" height="var(--control-md)" />
    </div>
  )
}
