import { useState } from 'react'
import { api, errorText } from '../api'
import { t as translate, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import './Browser.css'

// What the agent asked for, from its browser_help call.
export type HelpRequest = { reason: string; message: string }

const TITLE = {
  login: 'browser.help.login',
  captcha: 'browser.help.captcha',
  two_factor: 'browser.help.two_factor',
} as const

// What Done and Not now send: an answer the agent goes on from, in the user's language, as
// the user's own message in the conversation.
export const doneMessage = () => translate('browser.help.doneMessage')
export const notNowMessage = () => translate('browser.help.notNowMessage')

// HelpCard is the agent's request to take over the browser (a login, a captcha, a 2FA code):
// Take over opens the live view under the user's control; Done and Not now answer the agent,
// which carries on from that message.
export function HelpCard({
  help,
  conversation,
  canTakeOver,
  onTakeOver,
  onAnswered,
}: {
  help: HelpRequest
  // The conversation's API path, for the answer.
  conversation: string
  // Whether the browser can be used now (the live view needs it).
  canTakeOver: boolean
  onTakeOver: () => void
  onAnswered: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const answer = async (content: string) => {
    setBusy(true)
    setError(null)
    try {
      const id = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
      await api(`${conversation}/messages`, { method: 'POST', body: { content, request_id: id } })
      onAnswered()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="ui-browser-help" aria-label={t('browser.help.label')}>
      <Notice
        tone="info"
        title={t(Object.hasOwn(TITLE, help.reason) ? TITLE[help.reason as keyof typeof TITLE] : 'browser.help.other')}
      >
        {/* The model's words, shown as plain text. */}
        <p className="ui-browser-help-said">{help.message}</p>
        <div className="ui-browser-actions">
          <Button size="sm" icon="browser" disabled={!canTakeOver} onClick={onTakeOver}>
            {t('browser.takeOver')}
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => answer(doneMessage())}>
            {t('browser.help.done')}
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => answer(notNowMessage())}>
            {t('browser.help.notNow')}
          </Button>
        </div>
      </Notice>
      {error && <Notice tone="danger">{error}</Notice>}
    </section>
  )
}
