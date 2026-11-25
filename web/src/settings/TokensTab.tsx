// A member's own access tokens in the workspace, for their agents and scripts (docs/mcp.md).
// A new token is shown once, when it is made: the server keeps only its hash. It lives in
// this tab's state and nowhere else (no storage, no log), so leaving the tab forgets it.

import { type FormEvent, useState } from 'react'
import { api, type Token, type TokenCreated, ws } from '../api'
import { type Key, type T, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, CopyButton, day, useAction } from './parts'

type Made = TokenCreated

// What "Expires" offers, in days; null never expires.
const EXPIRY: { days: number | null; label: Key }[] = [
  { days: 30, label: 'settings.tokens.expiry.days30' },
  { days: 90, label: 'settings.tokens.expiry.days90' },
  { days: 365, label: 'settings.tokens.expiry.year' },
  { days: null, label: 'settings.tokens.expiry.never' },
]

export function TokensTab({ workspace }: { workspace: number }) {
  const base = `${ws(workspace)}/tokens`
  const tokens = useLoad(() => api<{ tokens: Token[] }>(base).then((r) => r.tokens), [base])
  const [made, setMade] = useState<Made | null>(null)
  const action = useAction()
  const t = useT()

  const revoke = (token: Token) =>
    action
      .run(() => api(`${base}/${token.id}`, { method: 'DELETE' }))
      .then((ok) => {
        // Only once it is gone: a failed revoke keeps the new token on screen, still valid.
        if (!ok) return
        if (made?.id === token.id) setMade(null)
        tokens.reload()
      })

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-tokens">
        <h2 id="tt-tokens">{t('settings.tokens.title')}</h2>
        <p className="tt-muted tt-small">{t('settings.tokens.intro')}</p>
        <CreateToken
          base={base}
          // One new token on show at a time: another would replace it, and its secret is
          // shown only once. Done first.
          held={made !== null}
          onCreated={(token) => {
            setMade(token)
            tokens.reload()
          }}
        />
        {made && <NewToken key={made.id} made={made} onDone={() => setMade(null)} />}
        <Alert error={tokens.error ?? action.error} />
        {!tokens.data && !tokens.error && <p className="tt-muted">{t('app.loading')}</p>}
        {tokens.data?.length === 0 && <p className="tt-muted">{t('settings.tokens.none')}</p>}
        {tokens.data && tokens.data.length > 0 && (
          <ul className="tt-rows" aria-label={t('settings.tokens.yours')}>
            {tokens.data.map((token) => (
              <li key={token.id} className="tt-row">
                <div className="tt-row-main">
                  <strong>{token.name}</strong>
                  <span className="tt-muted tt-small">
                    {[
                      t('settings.tokens.created', { date: day(token.created_at) }),
                      token.last_used_at
                        ? t('settings.tokens.lastUsed', { date: day(token.last_used_at) })
                        : t('settings.tokens.neverUsed'),
                      expiry(t, token.expires_at),
                    ].join(' · ')}
                  </span>
                </div>
                <div className="tt-row-actions">
                  <ConfirmButton
                    label={t('settings.tokens.revoke')}
                    name={token.name}
                    question={t('settings.tokens.revokeQuestion', { name: token.name })}
                    confirm={t('settings.tokens.revokeConfirm')}
                    disabled={action.busy}
                    onConfirm={() => revoke(token)}
                  />
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="tt-panel" aria-labelledby="tt-setup">
        <h2 id="tt-setup">{t('settings.tokens.connect')}</h2>
        <Setup key={made?.id ?? 0} token={made?.token} />
      </section>
    </div>
  )
}

function expiry(t: T, at: string | null): string {
  if (!at) return t('settings.tokens.neverExpires')
  return new Date(at).getTime() <= Date.now()
    ? t('settings.tokens.expired', { date: day(at) })
    : t('settings.tokens.expires', { date: day(at) })
}

function CreateToken({ base, held, onCreated }: { base: string; held: boolean; onCreated: (t: Made) => void }) {
  const [name, setName] = useState('')
  const [days, setDays] = useState('90')
  const action = useAction()
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (held) return
    const expires_at = days ? new Date(Date.now() + Number(days) * 86_400_000).toISOString() : undefined
    let made: Made | undefined
    const ok = await action.run(async () => {
      made = await api<Made>(base, { method: 'POST', body: { name, expires_at } })
    })
    if (ok && made) {
      setName('')
      onCreated(made)
    }
  }
  return (
    <>
      <form className="tt-form tt-form-row" onSubmit={submit}>
        <label>
          {t('settings.tokens.name')}
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            maxLength={100}
            placeholder={t('settings.tokens.namePlaceholder')}
            autoComplete="off"
          />
        </label>
        <label>
          {t('settings.tokens.expiresLabel')}
          <select value={days} onChange={(e) => setDays(e.target.value)}>
            {EXPIRY.map((x) => (
              <option key={x.label} value={x.days ?? ''}>
                {t(x.label)}
              </option>
            ))}
          </select>
        </label>
        <button type="submit" className="tt-button tt-primary" disabled={action.busy || held}>
          {action.busy ? t('settings.tokens.creating') : t('settings.tokens.create')}
        </button>
        {held && <span className="tt-muted tt-small">{t('settings.tokens.held')}</span>}
      </form>
      <Alert error={action.error} />
    </>
  )
}

function NewToken({ made, onDone }: { made: Made; onDone: () => void }) {
  const t = useT()
  return (
    <div className="tt-notice" role="status">
      <p>
        {t.rich('settings.tokens.made', { name: <strong>{made.name}</strong>, b: (text) => <strong>{text}</strong> })}
      </p>
      <div className="tt-copy">
        <input
          readOnly
          value={made.token}
          aria-label={t('settings.tokens.newToken')}
          onFocus={(e) => e.target.select()}
        />
        <CopyButton text={made.token} />
      </div>
      <div>
        <button type="button" className="tt-button" onClick={onDone}>
          {t('settings.tokens.done')}
        </button>
      </div>
    </div>
  )
}

// The setups of docs/mcp.md, for this server. `token` fills them in, right after it is made;
// otherwise they show where it goes.
function Setup({ token }: { token?: string }) {
  const t = useT()
  const url = `${window.location.origin}/mcp`
  const value = token ?? 'ttp_…'
  const snippets = [
    {
      label: t('settings.tokens.setup.env'),
      note: t('settings.tokens.setup.envNote'),
      text: `export TASKTO_TOKEN='${value}'`,
    },
    {
      label: 'Claude Code',
      note: t('settings.tokens.setup.claudeNote'),
      text: `claude mcp add --transport http --scope user taskto ${url} \\\n  --header "Authorization: Bearer $TASKTO_TOKEN"`,
    },
    {
      label: 'Codex',
      note: t('settings.tokens.setup.codexNote'),
      text: `codex mcp add taskto --url ${url} --bearer-token-env-var TASKTO_TOKEN`,
    },
    {
      label: t('settings.tokens.setup.other'),
      note: t('settings.tokens.setup.otherNote'),
      text: JSON.stringify({ mcpServers: { taskto: { url, headers: { Authorization: `Bearer ${value}` } } } }, null, 2),
    },
  ]
  return (
    <>
      <p className="tt-muted tt-small">
        {t.rich('settings.tokens.setup.endpoint', { url: <code>{url}</code>, code: (text) => <code>{text}</code> })}
      </p>
      {snippets.map((s) => (
        <section
          key={s.label}
          className="tt-setup-snippet"
          aria-label={t('settings.tokens.setup.named', { name: s.label })}
        >
          <h3>{s.label}</h3>
          <p className="tt-muted tt-small">{s.note}</p>
          <pre>
            <code>{s.text}</code>
          </pre>
          <div>
            <CopyButton text={s.text} name={t('settings.tokens.setup.theNamed', { name: s.label })} />
          </div>
        </section>
      ))}
    </>
  )
}
