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
import { clock } from '../conversations/recorder'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Spinner } from '../ui/Spinner'
import './capture.css'

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
  const { state, unavailable } = useDictation({
    workspace,
    disabled: !online,
    onText,
    shareStatus: true,
  })
  const label = t('apps.dictation.dictate', { field })
  // A server without dictation, or that can't say, has nothing to offer online; offline the button
  // is there, off, with why.
  if (online && !state) return null
  const phase = state?.phase ?? 'idle'
  return (
    <div className="ui-dictate">
      {phase === 'recording' && state ? (
        <>
          <span className="ui-dictate-clock" role="timer" aria-label={t('conversations.dictation.recorded')}>
            {t('conversations.dictation.elapsed', { elapsed: clock(state.elapsed), max: clock(state.max) })}
          </span>
          <Button size="sm" variant="ghost" onClick={state.cancel}>
            {t('common.cancel')}
          </Button>
          <IconButton icon="stop" label={t('conversations.dictation.stop')} onClick={state.stop} />
        </>
      ) : (
        <IconButton
          icon="mic"
          label={label}
          disabled={!online || !state?.canStart}
          // Where the speech goes, or why it can't be taken now.
          title={!online ? t('apps.dictation.offline') : (unavailable ?? t('apps.dictation.title'))}
          aria-busy={phase === 'starting' || phase === 'transcribing' || undefined}
          onClick={() => state?.start()}
        />
      )}
      {!online && (
        <span role="status" className="ui-dictate-note">
          {t('apps.dictation.offline')}
        </span>
      )}
      {online && unavailable !== undefined && (
        <span role="status" className="ui-dictate-note">
          {t('apps.dictation.unavailable', { reason: unavailable })}
        </span>
      )}
      {phase === 'transcribing' && (
        <span className="ui-dictate-note">
          <Spinner size="sm" /> {t('conversations.dictation.transcribing')}
        </span>
      )}
      {state?.error && (
        <span role="alert" className="ui-dictate-error">
          {state.error}
          {state.retry && (
            <Button size="sm" onClick={state.retry}>
              {t('app.tryAgain')}
            </Button>
          )}
        </span>
      )}
    </div>
  )
}
