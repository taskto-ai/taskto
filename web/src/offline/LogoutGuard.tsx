// Signing out with records that have not reached the server: the person chooses to sync them
// first, to discard them, or not to sign out. Whatever is chosen, once they are out, everything of
// theirs kept for offline use on this device is deleted (docs/offline.md).
//
// When the database can't be opened or counted (a newer version wrote it, or the storage isn't
// available), the count is not zero but unknown: the person is warned that unsent records may be
// there and that this version can't check, and chooses to keep the database (sign out without
// deleting it) or to discard it.

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
  // Null when the database is not open here: there is nothing to sync, only to discard.
  runtime: Runtime | null
  // How many records had not reached the server when this was opened.
  count: number
  // Why the database couldn't be counted, if so: what it holds is unknown.
  unreadable?: 'schema' | 'unavailable' | null
  // Sign out now: what is kept for offline use is deleted by the caller.
  onSignOut: () => void
  // Sign out and leave the database as it is (only offered when it couldn't be checked).
  onKeep?: () => void
  onCancel: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [left, setLeft] = useState(count)
  const [stuck, setStuck] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)
  // Discarding is a second, explicit step.
  const [discarding, setDiscarding] = useState(false)
  // A sync is under way: one at a time (a second press before React draws `busy` is refused too),
  // and the guard can't be closed meanwhile, since its end may sign the person out.
  const syncing = useRef(false)
  // The guard is still on screen, in the session it was opened in (session.ts). Once it is gone
  // (cancelled, or the page moved on) or the session ended (another account in another tab), a sync
  // that ends later signs nobody out and changes nothing: the person was shown the app again, or
  // the page belongs to someone else now.
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
      // Trying again changes items that need attention, which only the offline writer does (writer.ts).
      // In a tab that is not, pressing "Sync now" is the person asking for the right, as the
      // take-over button is: it is asked for first, and when this tab does not get it the person
      // is told, and nothing is attempted.
      if (!runtime.writer.holds()) {
        await runtime.writer.takeOver()
        if (!s.ok()) return
        if (!runtime.writer.holds()) {
          setFailure(t('offline.logout.notWriter'))
          return
        }
      }
      await runtime.engine.retry()
      if (!s.ok()) return
      const unsent = await runtime.store.unsynced()
      if (!s.ok()) return
      setLeft(unsent.length)
      if (unsent.length === 0) onSignOut()
      else setStuck(true)
    } catch (e) {
      if (!s.ok()) return
      // The right was lost while this ran (another tab took it), or the device refused.
      setFailure(e instanceof NotWriter ? t('offline.logout.notWriter') : errorText(e))
    } finally {
      syncing.current = false
      if (mounted.current) setBusy(false)
    }
  }
  // Escape and the close button are Cancel, but not while a sync runs.
  const cancel = () => {
    if (!syncing.current) onCancel()
  }
  // The safe way out takes the focus: keeping or sending the records, or else (nothing to sync
  // here) Cancel, never the destructive button.
  const canSync = !unreadable && runtime !== null

  return (
    <>
      <Dialog
        open
        // Escape, as Cancel: nothing is signed out and nothing is deleted. Not while a sync runs.
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
