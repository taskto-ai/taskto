// The banner a conversation on a CLI shows when the user's login to that CLI has expired: they
// sign in again right here, and the waiting turn goes on by itself.

import { type CliAccount, type CliAccounts, cliAccountFor, cliAccountTitle, cliOfProvider } from '../api'
import { useT } from '../i18n'
import { CliLoginFlow } from '../settings/CliAccountsTab'
import type { ModelRef } from './ModelPicker'

// The caller's account for the conversation's CLI when it needs them to sign in again; null
// for a conversation on any other model, or a CLI account that is fine.
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
    <section
      className="tt-conflict tt-cli-banner"
      aria-label={t('conversations.reconnect.label', { name: account.name })}
    >
      <p>{t('conversations.reconnect.text', { account: cliAccountTitle(account) })}</p>
      <CliLoginFlow
        account={account.id}
        name={account.name}
        label={t('conversations.reconnect.button')}
        onConnected={onConnected}
      />
    </section>
  )
}
