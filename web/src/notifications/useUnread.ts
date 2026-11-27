import { useCallback, useEffect, useState } from 'react'
import { api } from '../api'
import { INBOX_CHANGED, inboxPath } from './api'

const POLL_MS = 60_000

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
