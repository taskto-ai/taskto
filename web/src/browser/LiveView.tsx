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

// LiveView shows the personal browser's screen as it is (VNC, not screenshots). Until the
// user takes over (interactive), it only watches: clicks and keys don't reach the browser.
export function LiveView({
  interactive,
  load = loadRFB,
  onClosed,
}: {
  interactive: boolean
  load?: () => Promise<RFBConstructor>
  onClosed?: () => void
}) {
  const t = useT()
  const screen = useRef<HTMLDivElement>(null)
  const rfb = useRef<RFB | null>(null)
  const [state, setState] = useState<State>('loading')
  // A new attempt connects again, after the server closed the view (the browser stopped, the
  // connection dropped).
  const [attempt, setAttempt] = useState(0)
  const closedRef = useRef(onClosed)
  closedRef.current = onClosed

  // biome-ignore lint/correctness/useExhaustiveDependencies: a new attempt connects again
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
          // Our own teardown (unmount, a new attempt) disconnects too: not a close to report.
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
  }, [load, attempt])

  // Set again once the client exists (state), and whenever the user takes over or lets go.
  // biome-ignore lint/correctness/useExhaustiveDependencies: state stands for the client being made
  useEffect(() => {
    if (!rfb.current) return
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
            <Button
              size="sm"
              onClick={() => {
                setState('loading')
                setAttempt((n) => n + 1)
              }}
            >
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
