import { type CliAccount, type CliAccounts, cliAccountFor, cliAccountTitle, cliOfProvider } from '../api'
import { useT } from '../i18n'
import { CliLoginFlow } from '../settings/CliAccountsTab'
import { Notice } from '../ui/Notice'
import type { ModelRef } from './ModelPicker'

export function reconnectAccount(
  model: ModelRef | null | undefined,
  cli: CliAccounts | null | undefined,
): CliAccount | null {
  const name = cliOfProvider(model?.provider)
  if (!name || !cli?.enabled) return null
  const account = cliAccountFor(cli.accounts, name)
  return account?.state === 'reconnect' ? account : null
}

export function ReconnectBanner({ account, onConnected }: { account: CliAccount; onConnected: () => void }) {
  const t = useT()
  return (
    <section className="ui-chat-reconnect" aria-label={t('conversations.reconnect.label', { name: account.name })}>
      <Notice tone="warning">
        <p>{t('conversations.reconnect.text', { account: cliAccountTitle(account) })}</p>
        <CliLoginFlow
          account={account.id}
          name={account.name}
          label={t('conversations.reconnect.button')}
          onConnected={onConnected}
        />
      </Notice>
    </section>
  )
}
