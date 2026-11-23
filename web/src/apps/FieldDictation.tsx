// A microphone beside an app's text fields (docs/dictation.md): the same recorder and the same
// transcription route as the chat box, for the workspace the app is in, so the workspace's
// dictation settings and the person's membership decide whether it works. What it needs is said,
// not left to a button that does nothing:
//
// - the speech is sent to this server (and, if the workspace's engine is a hosted API, on to it) to
//   be turned into text, so it needs a connection: offline, the button is off and says so;
// - where the server has dictation off, or not ready, the button is off and says why;
// - either way, the keyboard's own dictation (the microphone key on a phone's keyboard) still works
//   in the field, and the note says so.
//
// The text lands in the field as typed text would: the person edits it before saving.

import { useSyncExternalStore } from 'react'
import { useDictation } from '../conversations/Dictation'
import { useT } from '../i18n'
import { Icon } from '../shell/Icon'

function subscribe(onChange: () => void) {
  window.addEventListener('online', onChange)
  window.addEventListener('offline', onChange)
  return () => {
    window.removeEventListener('online', onChange)
    window.removeEventListener('offline', onChange)
  }
}

// Whether the browser thinks it has a connection (it can say no more than that).
export function useOnline(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => navigator.onLine,
    () => true,
  )
}

export function FieldDictation({
  workspace,
  field,
  onText,
}: {
  workspace: number
  // The field's name, as its label says it.
  field: string
  onText: (text: string) => void
}) {
  const t = useT()
  const online = useOnline()
  const dictation = useDictation({
    workspace,
    disabled: !online,
    onText,
    shareStatus: true,
    name: {
      label: t('apps.dictation.dictate', { field }),
      title: t('apps.dictation.title'),
    },
  })
  if (!online) {
    return (
      <div className="tt-dictate">
        <button
          type="button"
          className="tt-button"
          disabled
          aria-label={t('apps.dictation.dictate', { field })}
          title={t('apps.dictation.offline')}
        >
          <Icon name="mic" />
        </button>
        <span role="status" className="tt-muted tt-small">
          {t('apps.dictation.offline')}
        </span>
      </div>
    )
  }
  // A server without dictation, or that can't say: nothing to offer.
  if (!dictation.controls) return null
  return (
    <div className="tt-dictate">
      {dictation.controls}
      {dictation.unavailable !== undefined && (
        <span role="status" className="tt-muted tt-small">
          {t('apps.dictation.unavailable', { reason: dictation.unavailable })}
        </span>
      )}
      {dictation.message}
    </div>
  )
}
