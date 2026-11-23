// Signing out with records that have not reached the server: the person chooses to sync them
// first, to discard them, or not to sign out. Whatever is chosen, once they are out, everything of
// theirs kept for offline use on this device is deleted (docs/offline.md).
//
// When the database can't be opened or counted (a newer version wrote it, or the storage isn't
// available), the count is not zero but unknown: the person is warned that unsent records may be
// there and that this version can't check, and chooses to keep the database (sign out without
// deleting it) or to discard it.

import { useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { ConfirmButton } from '../settings/parts'
import type { Runtime } from './runtime'

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
  const first = useRef<HTMLButtonElement>(null)
  useEffect(() => first.current?.focus(), [])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onCancel()
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onCancel])

  const sync = async () => {
    setBusy(true)
    setStuck(false)
    try {
      if (!runtime) return
      await runtime.engine.retry()
      const unsent = await runtime.store.unsynced()
      setLeft(unsent.length)
      if (unsent.length === 0) onSignOut()
      else setStuck(true)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="tt-guard">
      <div className="tt-guard-box" role="alertdialog" aria-modal="true" aria-labelledby="tt-guard-title">
        <h2 id="tt-guard-title">{t('offline.logout.title')}</h2>
        {unreadable ? (
          <p>{t(unreadable === 'schema' ? 'offline.logout.unreadableSchema' : 'offline.logout.unreadableStorage')}</p>
        ) : (
          <p>{t('offline.logout.body', { count: left })}</p>
        )}
        {stuck && (
          <p role="alert" className="tt-error">
            {t('offline.logout.stillWaiting')}
          </p>
        )}
        <div className="tt-row-actions">
          {unreadable ? (
            <button ref={first} type="button" className="tt-button tt-primary" disabled={busy} onClick={onKeep}>
              {t('offline.logout.keep')}
            </button>
          ) : (
            <button
              ref={first}
              type="button"
              className="tt-button tt-primary"
              disabled={busy || !runtime}
              onClick={() => void sync()}
            >
              {busy ? t('offline.logout.syncing') : t('offline.logout.sync')}
            </button>
          )}
          <ConfirmButton
            label={t('offline.logout.discard')}
            question={t('offline.queue.discardQuestion')}
            confirm={t('offline.queue.discardConfirm')}
            disabled={busy}
            onConfirm={onSignOut}
          />
          <button type="button" className="tt-button" disabled={busy} onClick={onCancel}>
            {t('offline.logout.cancel')}
          </button>
        </div>
      </div>
    </div>
  )
}
