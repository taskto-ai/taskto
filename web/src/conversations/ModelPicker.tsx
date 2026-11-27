import { type ReactNode, useId, useState } from 'react'
import { api, CLI_PRODUCTS, type CliAccounts, cliAccountFor, cliOfProvider, codeText, errorText } from '../api'
import { useT } from '../i18n'
import { Link, routePath } from '../router'
import { Select } from '../ui/Select'
import { VisuallyHidden } from '../ui/VisuallyHidden'

export type ModelRef = { provider: string; model_id: string }
export type ModelProvider = { provider: string; name: string; scope: string; models: { id: string; name: string }[] }

const modelKey = (m: ModelRef) => JSON.stringify([m.provider, m.model_id])

export function ModelPicker({
  workspace,
  url,
  providers,
  current,
  cli,
  onChosen,
  disabled,
}: {
  workspace: number
  url: string
  providers: ModelProvider[]
  current: ModelRef
  cli: CliAccounts | null | undefined
  onChosen: (m: ModelRef) => void
  disabled: boolean
}) {
  const t = useT()
  const id = useId()
  const [pending, setPending] = useState<ModelRef | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const selected = pending ?? current
  const known = providers.some(
    (p) => p.provider === selected.provider && p.models.some((m) => m.id === selected.model_id),
  )
  const account = (provider: string) => {
    const name = cliOfProvider(provider)
    return name && cli ? cliAccountFor(cli.accounts, name) : undefined
  }
  const cliProviders = providers.filter((p) => p.scope === 'cli')
  const groups: (ModelProvider | 'cli')[] = []
  for (const p of providers) {
    if (p.scope !== 'cli') groups.push(p)
    else if (!groups.includes('cli')) groups.push('cli')
  }

  const choose = async (key: string) => {
    const [provider, model_id] = JSON.parse(key) as [string, string]
    const model = { provider, model_id }
    setPending(model)
    setBusy(true)
    setError(null)
    try {
      onChosen(await api<ModelRef>(url, { method: 'PUT', body: model }))
    } catch (e) {
      setError(errorText(e))
    } finally {
      setPending(null)
      setBusy(false)
    }
  }

  const option = (p: ModelProvider, m: { id: string; name: string }, label: string) => (
    <option key={`${p.provider} ${m.id}`} value={modelKey({ provider: p.provider, model_id: m.id })}>
      {label}
    </option>
  )
  const selectedCli = cliOfProvider(selected.provider)
  const selectedAccount = account(selected.provider)
  const settings = (text: ReactNode) => (
    <Link key="settings" href={routePath({ workspace, section: 'settings', item: 'cli-accounts' })}>
      {text}
    </Link>
  )

  return (
    <div className="ui-chat-model">
      <VisuallyHidden>
        <label htmlFor={id}>{t('conversations.model.label')}</label>
      </VisuallyHidden>
      <Select
        id={id}
        size="sm"
        className="ui-chat-model-select"
        value={modelKey(selected)}
        onChange={(e) => choose(e.target.value)}
        disabled={disabled || busy}
      >
        {!known && (
          <option value={modelKey(selected)}>
            {t('conversations.model.unavailable', { model: selected.model_id, provider: selected.provider })}
          </option>
        )}
        {groups.map((g) =>
          g === 'cli' ? (
            <optgroup key="cli" label={t('conversations.model.cliGroup')}>
              {cliProviders.flatMap((p) => {
                const a = account(p.provider)
                const label =
                  cli && a?.state !== 'connected'
                    ? 'conversations.model.cliOptionNotConnected'
                    : 'conversations.model.cliOption'
                return p.models.map((m) => option(p, m, t(label, { provider: p.name, model: m.name })))
              })}
            </optgroup>
          ) : (
            <optgroup key={g.provider} label={g.name}>
              {g.models.map((m) => option(g, m, m.name))}
            </optgroup>
          ),
        )}
      </Select>
      {error && (
        <span role="alert" className="ui-chat-model-note" data-tone="danger">
          {error}
        </span>
      )}
      {selectedCli && cli && !cli.enabled && cli.reason && (
        <span className="ui-chat-model-note">{codeText(cli.reason_code, undefined, cli.reason)}</span>
      )}
      {selectedCli && cli?.enabled && selectedAccount?.state !== 'connected' && (
        <span className="ui-chat-model-note">
          {t.rich(
            selectedAccount?.state === 'reconnect'
              ? 'conversations.model.signInAgain'
              : 'conversations.model.notConnected',
            { cli: CLI_PRODUCTS[selectedCli], link: settings },
          )}
        </span>
      )}
    </div>
  )
}
