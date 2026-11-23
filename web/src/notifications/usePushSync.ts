import { useEffect } from 'react'
import { useLocale, useT } from '../i18n'
import { navigate } from '../router'
import { parseNotificationPath } from './api'
import { sendPushStrings, syncPush } from './push'
import { pushStrings } from './strings'

/**
 * A device whose person turned push on is registered again when the app starts signed in, and its
 * worker is given the person's language. Nothing happens for a device that didn't opt in.
 */
export function usePushSync(userId: number) {
  const t = useT()
  const locale = useLocale()
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new language is a new set of strings
  useEffect(() => {
    void syncPush(userId)
      .then(async (on) => {
        if (on) await sendPushStrings(pushStrings(t))
      })
      .catch(() => {})
  }, [userId, locale])
}

/**
 * The service worker says where a click on a push goes, when the app is open already: the
 * notification's own address, which the app opens and checks like any other.
 */
export function useOpenFromWorker() {
  useEffect(() => {
    const onMessage = (e: MessageEvent) => {
      const path = e.data?.type === 'OPEN' && typeof e.data.path === 'string' ? e.data.path : ''
      if (parseNotificationPath(path) !== null) navigate(path)
    }
    navigator.serviceWorker?.addEventListener('message', onMessage)
    return () => navigator.serviceWorker?.removeEventListener('message', onMessage)
  }, [])
}
