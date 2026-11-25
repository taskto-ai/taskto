// The chat box's microphone: press to record, press again to stop; the server turns the
// speech into text, which goes into the box to edit before sending (docs/dictation.md).

import { type ReactNode, useEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, type DictationStatus, dictationPath, errorText } from '../api'
import { browserLocale, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import './Dictation.css'
import { clock, encodeWav, type Recording, type StartRecording, startRecording, toBase64 } from './recorder'

type Phase = 'idle' | 'starting' | 'recording' | 'transcribing'

// The status of a workspace's dictation, shared by the many fields of an app's screen so they ask
// once: kept for a few seconds, and forgotten when the server says it isn't ready.
const SHARED_FOR = 5000
const shared = new Map<number, { at: number; status: Promise<DictationStatus> }>()

function sharedStatus(workspace: number): Promise<DictationStatus> {
  const known = shared.get(workspace)
  if (known && Date.now() - known.at < SHARED_FOR) return known.status
  const status = api<DictationStatus>(dictationPath(workspace))
  shared.set(workspace, { at: Date.now(), status })
  // A failure isn't kept: the next field asks again.
  status.catch(() => shared.delete(workspace))
  return status
}

export function forgetSharedDictationStatus() {
  shared.clear()
}

// useDictation gives the composer its dictation controls, for the row beside the box, and a
// message line (progress, errors) for above it. Both are null on a server without dictation.
//
// Fields of an app use it too (apps/FieldDictation.tsx): shareStatus has them ask for the status once
// together, name says which field the button is for (its label and title), and unavailable is why
// dictation can't be used on this server, when it can't.
export function useDictation({
  workspace,
  disabled,
  onText,
  record = startRecording,
  shareStatus,
  name,
}: {
  workspace: number
  disabled?: boolean
  onText: (text: string) => void
  record?: StartRecording
  shareStatus?: boolean
  name?: { label: string; title: string }
}): { controls: ReactNode; message: ReactNode; unavailable?: string } {
  const t = useT()
  const status = useLoad(
    () => (shareStatus ? sharedStatus(workspace) : api<DictationStatus>(dictationPath(workspace))),
    [workspace, shareStatus],
  )
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
      if (notReady) {
        shared.delete(workspace)
        status.reload()
      }
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
      // Recording: the clock and the stop button in the danger tone, so it's plain the microphone is on.
      <span className="ui-dictation" data-recording="">
        <span className="ui-dictation-clock" role="timer" aria-label={t('conversations.dictation.recorded')}>
          {t('conversations.dictation.elapsed', { elapsed: clock(elapsed), max: clock(max) })}
        </span>
        <Button size="sm" variant="ghost" onClick={cancel}>
          {t('common.cancel')}
        </Button>
        <IconButton
          icon="stop"
          className="ui-dictation-stop"
          label={t('conversations.dictation.stop')}
          onClick={stop}
        />
      </span>
    ) : (
      <IconButton
        icon="mic"
        onClick={start}
        disabled={disabled || phase !== 'idle' || unavailable !== undefined}
        label={name?.label ?? t('conversations.dictation.dictate')}
        // Why it can't be used, where it can't: a disabled button shows no tooltip of its own.
        title={unavailable ?? name?.title}
      />
    )

  let message: ReactNode = null
  if (phase === 'transcribing')
    message = <p className="ui-dictation-progress">{t('conversations.dictation.transcribing')}</p>
  else if (error)
    message = (
      <Notice
        tone="danger"
        action={
          unsent && (
            <Button size="sm" onClick={() => transcribe(unsent)}>
              {t('app.tryAgain')}
            </Button>
          )
        }
      >
        {error}
      </Notice>
    )
  return { controls, message, unavailable }
}
