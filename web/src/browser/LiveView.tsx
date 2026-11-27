import { useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { liveURL, loadRFB, type RFB, type RFBConstructor } from './novnc'

type State = 'loading' | 'connecting' | 'connected' | 'closed' | 'failed'

const LABEL = {
  loading: 'browser.live.loading',
  connecting: 'browser.live.connecting',
  connected: null,
  closed: 'browser.live.closed',
  failed: 'browser.live.failed',
} as const satisfies Record<State, string | null>

type LiveProps = {
  interactive: boolean
  load?: () => Promise<RFBConstructor>
  onClosed?: () => void
}

export function LiveView(props: LiveProps) {
  const [attempt, setAttempt] = useState(0)
  return <LiveSession key={attempt} {...props} onReconnect={() => setAttempt((n) => n + 1)} />
}

function LiveSession({ interactive, load = loadRFB, onClosed, onReconnect }: LiveProps & { onReconnect: () => void }) {
  const t = useT()
  const screen = useRef<HTMLDivElement>(null)
  const rfb = useRef<RFB | null>(null)
  const [state, setState] = useState<State>('loading')
  const closedRef = useRef(onClosed)
  closedRef.current = onClosed

  useEffect(() => {
    let gone = false
    load().then(
      (RFBClient) => {
        const el = screen.current
        if (gone || !el) return
        setState('connecting')
        const client = new RFBClient(el, liveURL(), { wsProtocols: ['binary'] })
        client.scaleViewport = true
        client.resizeSession = false
        client.background = 'transparent'
        client.addEventListener('connect', () => setState('connected'))
        client.addEventListener('disconnect', () => {
          if (gone) return
          setState('closed')
          closedRef.current?.()
        })
        rfb.current = client
      },
      () => {
        if (!gone) setState('failed')
      },
    )
    return () => {
      gone = true
      rfb.current?.disconnect()
      rfb.current = null
    }
  }, [load])

  useEffect(() => {
    if (state === 'loading' || state === 'failed' || !rfb.current) return
    rfb.current.viewOnly = !interactive
    rfb.current.focusOnClick = interactive
    if (interactive) rfb.current.focus()
  }, [interactive, state])

  const label = LABEL[state]
  return (
    <div className="ui-browser-live" data-interactive={interactive || undefined}>
      {label && (
        <p className="ui-browser-note" role="status">
          {t(label)}{' '}
          {(state === 'closed' || state === 'failed') && (
            <Button size="sm" onClick={onReconnect}>
              {t('browser.live.reconnect')}
            </Button>
          )}
        </p>
      )}
      <div
        ref={screen}
        className="ui-browser-screen"
        role="img"
        aria-label={interactive ? t('browser.live.yours') : t('browser.live.watching')}
      />
    </div>
  )
}
