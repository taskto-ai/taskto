import { useId, useSyncExternalStore } from 'react'
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
  const why = useId()
  const reason = !online || unavailable !== undefined
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
          title={!online ? t('apps.dictation.offline') : (unavailable ?? t('apps.dictation.title'))}
          aria-describedby={reason ? why : undefined}
          aria-busy={phase === 'starting' || phase === 'transcribing' || undefined}
          onClick={() => state?.start()}
        />
      )}
      {!online && (
        <span role="status" className="ui-dictate-note" id={why}>
          {t('apps.dictation.offline')}
        </span>
      )}
      {online && unavailable !== undefined && (
        <span role="status" className="ui-dictate-note" id={why}>
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
