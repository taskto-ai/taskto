// The chat box's microphone: press to record, press again to stop; the server turns the
// speech into text, which goes into the box to edit before sending (docs/dictation.md).

import { type ReactNode, useEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, type DictationStatus, dictationPath, errorText } from '../api'
import { browserLocale, useT } from '../i18n'
import { useLoad } from '../load'
import { Icon } from '../shell/Icon'
import { clock, encodeWav, type Recording, type StartRecording, startRecording, toBase64 } from './recorder'

type Phase = 'idle' | 'starting' | 'recording' | 'transcribing'

// useDictation gives the composer its dictation controls, for the row beside the box, and a
// message line (progress, errors) for above it. Both are null on a server without dictation.
export function useDictation({
  workspace,
  disabled,
  onText,
  record = startRecording,
}: {
  workspace: number
  disabled?: boolean
  onText: (text: string) => void
  record?: StartRecording
}): { controls: ReactNode; message: ReactNode } {
  const t = useT()
  const status = useLoad(() => api<DictationStatus>(dictationPath(workspace)), [workspace])
  const [phase, setPhase] = useState<Phase>('idle')
  const [elapsed, setElapsed] = useState(0)
  const [error, setError] = useState<string | null>(null)
  // The last recording that didn't become text, to try again without speaking again.
  const [unsent, setUnsent] = useState<Int16Array | null>(null)
  const recording = useRef<Recording | null>(null)
  const max = status.data?.max_seconds ?? 600

  // A recording doesn't outlive the chat box, even one that starts after it is gone (the
  // browser was still asking for the microphone).
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      recording.current?.cancel()
    }
  }, [])

  const transcribe = async (samples: Int16Array) => {
    setPhase('transcribing')
    setError(null)
    try {
      const reply = await api<{ text: string }>(`${dictationPath(workspace)}/transcriptions`, {
        method: 'POST',
        // The server listens for the user's language when the workspace sets none (docs/i18n.md).
        body: { audio_base64: await toBase64(encodeWav(samples)), browser_locale: browserLocale() },
      })
      setUnsent(null)
      if (reply.text.trim()) onText(reply.text.trim())
      else setError(t('conversations.dictation.noSpeech'))
    } catch (e) {
      setUnsent(samples)
      // 503: not set up, or the local model is still downloading; the reason says which.
      const notReady = e instanceof ApiError && e.status === 503
      setError(notReady ? errorText(e) : t('conversations.dictation.failed', { error: errorText(e) }))
      if (notReady) status.reload()
    } finally {
      setPhase('idle')
    }
  }

  const stop = () => {
    const r = recording.current
    recording.current = null
    if (!r) return
    const samples = r.stop()
    if (samples.length === 0) {
      setPhase('idle')
      setError(t('conversations.dictation.nothingRecorded'))
      return
    }
    void transcribe(samples)
  }
  const stopRef = useRef(stop)
  stopRef.current = stop

  // The clock, and the time limit.
  useEffect(() => {
    if (phase !== 'recording') return
    const timer = setInterval(() => {
      const s = recording.current?.seconds() ?? 0
      setElapsed(s)
      if (s >= max) stopRef.current()
    }, 250)
    return () => clearInterval(timer)
  }, [phase, max])

  const start = async () => {
    setError(null)
    setUnsent(null)
    setPhase('starting')
    // The local model downloads on first use: start that now, while the person speaks.
    void api(`${dictationPath(workspace)}/warm`, { method: 'POST', body: {} }).catch(() => {})
    try {
      const r = await record()
      if (!alive.current) {
        r.cancel()
        return
      }
      recording.current = r
      setElapsed(0)
      setPhase('recording')
    } catch (e) {
      setPhase('idle')
      setError(
        e instanceof DOMException && e.name === 'NotAllowedError'
          ? t('conversations.dictation.micNotAllowed')
          : t('conversations.dictation.micFailed', { error: errorText(e) }),
      )
    }
  }

  const cancel = () => {
    recording.current?.cancel()
    recording.current = null
    setPhase('idle')
  }

  // A server without dictation, or one that can't say: nothing to show.
  if (!status.data) return { controls: null, message: null }
  const unavailable =
    status.data.engine === ''
      ? status.data.reason
        ? codeText(status.data.reason_code, status.data.reason_params, status.data.reason)
        : t('conversations.dictation.notSetUp')
      : undefined

  const controls =
    phase === 'recording' ? (
      <>
        <span className="tt-small tt-recording" role="timer" aria-label={t('conversations.dictation.recorded')}>
          {t('conversations.dictation.elapsed', { elapsed: clock(elapsed), max: clock(max) })}
        </span>
        <button type="button" className="tt-button" onClick={cancel}>
          {t('common.cancel')}
        </button>
        <button
          type="button"
          className="tt-button tt-recording"
          onClick={stop}
          aria-label={t('conversations.dictation.stop')}
          title={t('conversations.dictation.stopTitle')}
        >
          <Icon name="stop" />
        </button>
      </>
    ) : (
      <button
        type="button"
        className="tt-button"
        onClick={start}
        disabled={disabled || phase !== 'idle' || unavailable !== undefined}
        aria-label={t('conversations.dictation.dictate')}
        title={unavailable ?? t('conversations.dictation.dictateTitle')}
      >
        <Icon name="mic" />
      </button>
    )

  let message: ReactNode = null
  if (phase === 'transcribing')
    message = <p className="tt-muted tt-small">{t('conversations.dictation.transcribing')}</p>
  else if (error)
    message = (
      <p role="alert" className="tt-error">
        {error}{' '}
        {unsent && (
          <button type="button" className="tt-button" onClick={() => transcribe(unsent)}>
            {t('app.tryAgain')}
          </button>
        )}
      </p>
    )
  return { controls, message }
}
