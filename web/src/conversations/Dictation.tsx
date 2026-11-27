import { type ReactNode, useEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, type DictationStatus, dictationPath, errorText } from '../api'
import { browserLocale, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import './Dictation.css'
import { steps } from '../session'
import { clock, encodeWav, type Recording, type StartRecording, startRecording, toBase64 } from './recorder'

type Phase = 'idle' | 'starting' | 'recording' | 'transcribing'

export interface DictationState {
  phase: Phase
  elapsed: number
  max: number
  canStart: boolean
  start: () => void
  stop: () => void
  cancel: () => void
  error: string | null
  retry?: () => void
}

const SHARED_FOR = 5000
const shared = new Map<number, { at: number; status: Promise<DictationStatus> }>()

function sharedStatus(workspace: number): Promise<DictationStatus> {
  const known = shared.get(workspace)
  if (known && Date.now() - known.at < SHARED_FOR) return known.status
  const status = api<DictationStatus>(dictationPath(workspace))
  shared.set(workspace, { at: Date.now(), status })
  status.catch(() => shared.delete(workspace))
  return status
}

export function forgetSharedDictationStatus() {
  shared.clear()
}

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
}): { controls: ReactNode; message: ReactNode; unavailable?: string; state?: DictationState } {
  const t = useT()
  const status = useLoad(
    () => (shareStatus ? sharedStatus(workspace) : api<DictationStatus>(dictationPath(workspace))),
    [workspace, shareStatus],
  )
  const [phase, setPhase] = useState<Phase>('idle')
  const [elapsed, setElapsed] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [unsent, setUnsent] = useState<Int16Array | null>(null)
  const recording = useRef<Recording | null>(null)
  const max = status.data?.max_seconds ?? 600

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
    const s = steps(alive)
    try {
      const audio = await toBase64(encodeWav(samples))
      s.check(undefined)
      const reply = s.check(
        await api<{ text: string }>(`${dictationPath(workspace)}/transcriptions`, {
          method: 'POST',
          body: { audio_base64: audio, browser_locale: browserLocale() },
        }),
      )
      setUnsent(null)
      if (reply.text.trim()) onText(reply.text.trim())
      else setError(t('conversations.dictation.noSpeech'))
    } catch (e) {
      if (!s.ok()) return
      setUnsent(samples)
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

  if (!status.data) return { controls: null, message: null }
  const unavailable =
    status.data.engine === ''
      ? status.data.reason
        ? codeText(status.data.reason_code, status.data.reason_params, status.data.reason)
        : t('conversations.dictation.notSetUp')
      : undefined

  const controls =
    phase === 'recording' ? (
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
  const state: DictationState = {
    phase,
    elapsed,
    max,
    canStart: !disabled && phase === 'idle' && unavailable === undefined,
    start: () => void start(),
    stop,
    cancel,
    error,
    retry: unsent ? () => void transcribe(unsent) : undefined,
  }
  return { controls, message, unavailable, state }
}
