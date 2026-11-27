import { useId, useState } from 'react'
import { api, type CliAccounts, cliAccountFor, cliOfProvider, errorText } from '../api'
import { useT } from '../i18n'
import { sameSession } from '../session'
import { Button } from '../ui/Button'
import { Select } from '../ui/Select'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import type { ModelProvider, ModelRef } from './ModelPicker'

const modelKey = (m: ModelRef) => JSON.stringify([m.provider, m.model_id])

export function ContinueBar({
  base,
  providers,
  current,
  cli,
  onChosen,
  onContinued,
}: {
  base: string
  providers: ModelProvider[]
  current: ModelRef
  cli: CliAccounts | null | undefined
  onChosen: (m: ModelRef) => void
  onContinued: () => void
}) {
  const t = useT()
  const id = useId()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = async (model?: ModelRef) => {
    setBusy(true)
    setError(null)
    const same = sameSession()
    try {
      if (model) {
        await api(`${base}/model`, { method: 'PUT', body: model })
        if (!same()) return
        onChosen(model)
      }
      await api(`${base}/continue`, { method: 'POST', body: {} })
      if (!same()) return
      onContinued()
    } catch (e) {
      if (same()) setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  const label = (p: ModelProvider, m: { id: string; name: string }) => {
    const name = cliOfProvider(p.provider)
    if (!name) return t('conversations.continueOption', { provider: p.name, model: m.name })
    const connected = !cli || cliAccountFor(cli.accounts, name)?.state === 'connected'
    return t(connected ? 'conversations.model.cliOption' : 'conversations.model.cliOptionNotConnected', {
      provider: p.name,
      model: m.name,
    })
  }

  return (
    <div className="ui-chat-continue">
      <Button size="sm" onClick={() => run()} disabled={busy}>
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
        {providers.flatMap((p) =>
          p.models
            .filter((m) => modelKey({ provider: p.provider, model_id: m.id }) !== modelKey(current))
            .map((m) => (
              <option key={`${p.provider} ${m.id}`} value={modelKey({ provider: p.provider, model_id: m.id })}>
                {label(p, m)}
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
