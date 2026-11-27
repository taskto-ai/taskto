import { useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { useMounted } from '../mounted'
import { steps } from '../session'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { Notice } from '../ui/Notice'
import type { Runtime } from './runtime'
import { NotWriter } from './store'

export function LogoutGuard({
  runtime,
  count,
  unreadable,
  onSignOut,
  onKeep,
  onCancel,
}: {
  runtime: Runtime | null
  count: number
  unreadable?: 'schema' | 'unavailable' | null
  onSignOut: () => void
  onKeep?: () => void
  onCancel: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [left, setLeft] = useState(count)
  const [stuck, setStuck] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)
  const [discarding, setDiscarding] = useState(false)
  const syncing = useRef(false)
  const mounted = useMounted()

  const sync = async () => {
    if (syncing.current) return
    syncing.current = true
    setBusy(true)
    setStuck(false)
    setFailure(null)
    const s = steps(mounted)
    try {
      if (!runtime) return
      if (!runtime.writer.holds()) {
        await runtime.writer.takeOver()
        if (!s.ok()) return
        if (!runtime.writer.holds()) {
          setFailure(t('offline.logout.notWriter'))
          return
        }
      }
      await runtime.engine.retry()
      const unsent = await runtime.store.unsynced()
      if (!s.ok()) return
      setLeft(unsent.length)
      if (unsent.length === 0) onSignOut()
      else setStuck(true)
    } catch (e) {
      setFailure(e instanceof NotWriter ? t('offline.logout.notWriter') : errorText(e))
    } finally {
      syncing.current = false
      if (mounted.current) setBusy(false)
    }
  }
  const cancel = () => {
    if (!syncing.current) onCancel()
  }
  const canSync = !unreadable && runtime !== null

  return (
    <>
      <Dialog
        open
        onClose={cancel}
        dismissible={!busy}
        role="alertdialog"
        size="sm"
        title={t('offline.logout.title')}
        actions={
          <>
            <Button variant="danger" disabled={busy} onClick={() => setDiscarding(true)}>
              {t('offline.logout.discard')}
            </Button>
            <Button disabled={busy} onClick={cancel} data-autofocus={!unreadable && !canSync ? true : undefined}>
              {t('offline.logout.cancel')}
            </Button>
            {unreadable ? (
              <Button variant="primary" disabled={busy} onClick={onKeep} data-autofocus>
                {t('offline.logout.keep')}
              </Button>
            ) : (
              <Button
                variant="primary"
                loading={busy}
                disabled={!runtime}
                onClick={() => void sync()}
                data-autofocus={canSync ? true : undefined}
              >
                {t('offline.logout.sync')}
              </Button>
            )}
          </>
        }
      >
        {unreadable ? (
          <p>{t(unreadable === 'schema' ? 'offline.logout.unreadableSchema' : 'offline.logout.unreadableStorage')}</p>
        ) : (
          <p>{t('offline.logout.body', { count: left })}</p>
        )}
        {stuck && <Notice tone="danger">{t('offline.logout.stillWaiting')}</Notice>}
        {failure && <Notice tone="danger">{failure}</Notice>}
      </Dialog>
      <ConfirmDialog
        open={discarding}
        title={t('offline.logout.discard')}
        confirmLabel={t('offline.queue.discardConfirm')}
        tone="danger"
        onConfirm={() => {
          setDiscarding(false)
          onSignOut()
        }}
        onCancel={() => setDiscarding(false)}
      >
        {t('offline.queue.discardQuestion')}
      </ConfirmDialog>
    </>
  )
}
