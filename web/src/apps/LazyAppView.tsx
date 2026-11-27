import { Component, type ComponentProps, lazy, type ReactNode, Suspense, useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import type { AppView as AppViewType } from './AppView'
import './AppLoading.css'

type Props = ComponentProps<typeof AppViewType>

class RendererMissing extends Error {}

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
