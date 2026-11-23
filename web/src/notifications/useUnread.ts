import { useCallback, useEffect, useState } from 'react'
import { api } from '../api'
import { INBOX_CHANGED, inboxPath } from './api'

const POLL_MS = 60_000

/**
 * How many notifications the person hasn't read in a workspace, for the badge. Asked when the app
 * opens, when it comes back to the foreground, when the inbox changed here, when a push reached
 * the service worker, and once a minute while it is shown: the inbox is what is true, and a push
 * is only one more reason to ask. A failed ask keeps the last number; the badge is a convenience
 * and never an error.
 */
export function useUnread(workspace: number): number {
  const [unread, setUnread] = useState(0)
  const ask = useCallback(() => {
    api<{ unread: number }>(`${inboxPath(workspace)}/count`).then(
      (r) => setUnread(r.unread),
      () => {},
    )
  }, [workspace])
  useEffect(() => {
    setUnread(0)
    ask()
    const visible = () => {
      if (document.visibilityState === 'visible') ask()
    }
    const onMessage = (e: MessageEvent) => {
      if (e.data?.type === 'PUSH_RECEIVED') ask()
    }
    const timer = window.setInterval(visible, POLL_MS)
    document.addEventListener('visibilitychange', visible)
    window.addEventListener('focus', ask)
    window.addEventListener(INBOX_CHANGED, ask)
    navigator.serviceWorker?.addEventListener('message', onMessage)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', visible)
      window.removeEventListener('focus', ask)
      window.removeEventListener(INBOX_CHANGED, ask)
      navigator.serviceWorker?.removeEventListener('message', onMessage)
    }
  }, [ask])
  return unread
}
