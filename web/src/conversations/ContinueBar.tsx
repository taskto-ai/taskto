import { useId, useState } from 'react'
import { ApiError, continueConversation, continueModels, errorText } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { sameSession } from '../session'
import { Button } from '../ui/Button'
import { Select } from '../ui/Select'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import type { ModelRef } from './ModelPicker'

const modelKey = (m: ModelRef) => JSON.stringify([m.provider, m.model_id])

export function ContinueBar({
  base,
  current,
  afterEntry,
  onChosen,
  onContinued,
  onPending,
}: {
  base: string
  current: ModelRef
  afterEntry: number
  onChosen: (m: ModelRef) => void
  onContinued: () => void
  onPending: (pending: boolean) => void
}) {
  const t = useT()
  const id = useId()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const models = useLoad(() => continueModels(base), [base, afterEntry])

  const run = async (model: ModelRef) => {
    setBusy(true)
    setError(null)
    onPending(true)
    const same = sameSession()
    try {
      const reply = await continueConversation(base, { model, after_entry: afterEntry })
      if (!same()) return
      onChosen(reply.model)
      onContinued()
    } catch (e) {
      if (!same()) return
      setBusy(false)
      setError(errorText(e))
      if (e instanceof ApiError && e.status === 409) {
        models.reload()
        onContinued()
      }
    } finally {
      onPending(false)
    }
  }

  return (
    <div className="ui-chat-continue">
      <Button size="sm" onClick={() => run(current)} disabled={busy}>
        {t('conversations.tryAgain')}
      </Button>
      <VisuallyHidden>
        <label htmlFor={id}>{t('conversations.continueOn')}</label>
      </VisuallyHidden>
      <Select
        id={id}
        size="sm"
        className="ui-chat-continue-select"
        value=""
        disabled={busy}
        onChange={(e) => {
          const [provider, model_id] = JSON.parse(e.target.value) as [string, string]
          void run({ provider, model_id })
        }}
      >
        <option value="" disabled>
          {t('conversations.continueOn')}
        </option>
        {(models.data?.providers ?? []).flatMap((p) =>
          (p.models ?? [])
            .filter((m) => modelKey({ provider: p.provider, model_id: m.id }) !== modelKey(current))
            .map((m) => (
              <option key={`${p.provider} ${m.id}`} value={modelKey({ provider: p.provider, model_id: m.id })}>
                {t('conversations.continueOption', { provider: p.name, model: m.name })}
              </option>
            )),
        )}
      </Select>
      {error && (
        <span role="alert" className="ui-chat-continue-note">
          {error}
        </span>
      )}
    </div>
  )
}
