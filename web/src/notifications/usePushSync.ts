import { useEffect, useMemo } from 'react'
import { useT } from '../i18n'
import { navigate } from '../router'
import { parseNotificationPath } from './api'
import { sendPushStrings, syncPush } from './push'
import { pushStrings } from './strings'

export function usePushSync(userId: number, inWorkspace: boolean) {
  const t = useT()
  const strings = useMemo(() => pushStrings(t), [t])
  useEffect(() => {
    void syncPush(userId, inWorkspace)
      .then(async (on) => {
        if (on) await sendPushStrings(strings)
      })
      .catch(() => {})
  }, [userId, inWorkspace, strings])
}

export function useOpenFromWorker(enabled: boolean) {
  useEffect(() => {
    if (!enabled) return
    const onMessage = (e: MessageEvent) => {
      const path = e.data?.type === 'OPEN' && typeof e.data.path === 'string' ? e.data.path : ''
      if (parseNotificationPath(path) !== null) navigate(path)
    }
    navigator.serviceWorker?.addEventListener('message', onMessage)
    return () => navigator.serviceWorker?.removeEventListener('message', onMessage)
  }, [enabled])
}
