// The app's screen, its renderer loaded when it is first drawn: Home and the Apps list draw one
// only when there is an app to show (a widget, an app opened), so a Home without an app widget and
// the list with no app open never load the renderer (A2UI and its schema libraries). While it
// loads, the frame AppView draws (a panel, or bare in a widget) with the shape AppView shows while
// its screen loads, so nothing moves when it arrives.
//
// The renderer's code can fail to arrive (a first visit before the service worker took over, a
// server reached over plain HTTP, a tab left open across an update): that is said in the app's own
// frame, with Try again, never as the widget's or the page's failure. A failed import is forgotten
// at once, so Try again, and any later draw elsewhere (another widget, an app opened), imports
// again: React.lazy keeps a rejection for good, so the lazy component itself is made anew.

import { Component, type ComponentProps, lazy, type ReactNode, Suspense, useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import type { AppView as AppViewType } from './AppView'
import './AppLoading.css'

type Props = ComponentProps<typeof AppViewType>

// The renderer's code could not be fetched: told apart from an app that fails while it is drawn,
// which is the boundary above's to say (a widget's, the page's).
class RendererMissing extends Error {}

// How the renderer is loaded: a new lazy component after each failure.
export const rendererLoad = {
  import: () => import('./AppView'),
}
function makeLazy() {
  return lazy(() =>
    rendererLoad.import().then(
      (m) => ({ default: m.AppView }),
      (e: unknown) => {
        LazyView = makeLazy()
        throw new RendererMissing(e instanceof Error ? e.message : String(e))
      },
    ),
  )
}
let LazyView = makeLazy()

export function LazyAppView(props: Props) {
  // Each Try again draws anew, with whatever lazy component is current (a new one after a failure).
  const [attempt, setAttempt] = useState(0)
  const View = LazyView
  return (
    <Missing key={attempt} frame={props.frame ?? 'panel'} label={props.label} onRetry={() => setAttempt((n) => n + 1)}>
      <Suspense fallback={<AppViewLoading frame={props.frame ?? 'panel'} label={props.label} />}>
        <View {...props} />
      </Suspense>
    </Missing>
  )
}

// Catches only the renderer's missing code; any other error goes on to the boundary above.
class Missing extends Component<
  { frame: 'panel' | 'bare'; label: string; onRetry: () => void; children: ReactNode },
  { missing: boolean; error: unknown }
> {
  state = { missing: false, error: null as unknown }
  static getDerivedStateFromError(error: unknown) {
    return { missing: error instanceof RendererMissing, error }
  }
  render() {
    if (this.state.error && !this.state.missing) throw this.state.error
    if (this.state.missing)
      return <RendererFailed frame={this.props.frame} label={this.props.label} onRetry={this.props.onRetry} />
    return this.props.children
  }
}

function RendererFailed({ frame, label, onRetry }: { frame: 'panel' | 'bare'; label: string; onRetry: () => void }) {
  const t = useT()
  return (
    <section className="ui-app" data-frame={frame} aria-label={label}>
      <div className="ui-app-failed">
        <Notice
          tone="danger"
          action={
            <Button size="sm" onClick={onRetry}>
              {t('app.tryAgain')}
            </Button>
          }
        >
          {t('apps.rendererFailed')}
        </Notice>
      </div>
    </section>
  )
}

function AppViewLoading({ frame, label }: { frame: 'panel' | 'bare'; label: string }) {
  const t = useT()
  return (
    <section className="ui-app" data-frame={frame} aria-label={label}>
      <div className="ui-app-loading" role="status">
        <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
        <Skeleton width="40%" />
        <Skeleton shape="block" height="var(--control-md)" />
        <Skeleton shape="block" height="var(--control-md)" />
        <Skeleton shape="block" width="30%" height="var(--control-md)" />
      </div>
    </section>
  )
}
