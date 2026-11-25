// A member's own access tokens in the workspace, for their agents and scripts (docs/mcp.md).
// A new token is shown once, when it is made: the server keeps only its hash. It lives in
// this tab's state and nowhere else (no storage, no log), so leaving the tab forgets it.

import { type FormEvent, useId, useState } from 'react'
import { api, type Token, type TokenCreated, ws } from '../api'
import { type Key, type T, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Alert, CopyButton, CopyField, day, Loading, Section, SettingsTab, useAction } from './parts'

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
  const [creating, setCreating] = useState(false)
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const t = useT()

  const revoke = async (token: Token) => {
    const yes = await confirm({
      title: t('settings.tokens.revokeQuestion', { name: token.name }),
      confirmLabel: t('settings.tokens.revokeConfirm'),
      tone: 'danger',
    })
    if (!yes) return
    const ok = await action.run(() => api(`${base}/${token.id}`, { method: 'DELETE' }))
    // Only once it is gone: a failed revoke keeps the new token on screen, still valid.
    if (!ok) return
    if (made?.id === token.id) setMade(null)
    tokens.reload()
  }

  // One new token on show at a time: another would replace it, and its secret is shown only
  // once. Done first.
  const held = made !== null
  const createButton = (
    <Button variant="primary" icon="plus" disabled={held} onClick={() => setCreating(true)}>
      {t('settings.tokens.create')}
    </Button>
  )
  const empty = tokens.data?.length === 0

  return (
    <SettingsTab title={t('settings.tokens.title')} help={t('settings.help.tokens')} actions={!empty && createButton}>
      <p className="settings-muted">{t('settings.tokens.intro')}</p>
      {made && <NewToken key={made.id} made={made} onDone={() => setMade(null)} />}
      <Alert error={tokens.error ?? action.error} />
      {!tokens.data && !tokens.error && <Loading />}
      {empty && (
        <EmptyState title={t('settings.tokens.none')} actions={createButton}>
          {t('settings.tokens.noneHelp')}
        </EmptyState>
      )}
      {tokens.data && tokens.data.length > 0 && (
        <Panel>
          <Rows label={t('settings.tokens.yours')}>
            {tokens.data.map((token) => {
              const expired = token.expires_at !== null && new Date(token.expires_at).getTime() <= Date.now()
              return (
                <Row
                  key={token.id}
                  title={token.name}
                  detail={[
                    t('settings.tokens.created', { date: day(token.created_at) }),
                    token.last_used_at
                      ? t('settings.tokens.lastUsed', { date: day(token.last_used_at) })
                      : t('settings.tokens.neverUsed'),
                    expiry(t, token.expires_at),
                  ].join(' · ')}
                  status={expired && <Badge tone="neutral">{t('settings.tokens.expiredBadge')}</Badge>}
                  actions={
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={t('common.labelFor', { label: t('settings.tokens.revoke'), name: token.name })}
                      disabled={action.busy}
                      onClick={() => revoke(token)}
                    >
                      {t('settings.tokens.revoke')}
                    </Button>
                  }
                />
              )
            })}
          </Rows>
        </Panel>
      )}

      <Section title={t('settings.tokens.connect')}>
        <Setup key={made?.id ?? 0} token={made?.token} />
      </Section>
      {/* Drawn only while open, so each opening starts with an empty form and no old error. */}
      {creating && !held && (
        <CreateToken
          open
          base={base}
          onClose={() => setCreating(false)}
          onCreated={(token) => {
            setCreating(false)
            setMade(token)
            tokens.reload()
          }}
        />
      )}
      {confirmDialog}
    </SettingsTab>
  )
}

function expiry(t: T, at: string | null): string {
  if (!at) return t('settings.tokens.neverExpires')
  return new Date(at).getTime() <= Date.now()
    ? t('settings.tokens.expired', { date: day(at) })
    : t('settings.tokens.expires', { date: day(at) })
}

function CreateToken({
  open,
  base,
  onClose,
  onCreated,
}: {
  open: boolean
  base: string
  onClose: () => void
  onCreated: (t: Made) => void
}) {
  const [name, setName] = useState('')
  const [days, setDays] = useState('90')
  const action = useAction()
  const t = useT()
  const formId = useId()
  // While the token is being made the dialog stays: closed, it would forget the request, whose
  // answer (the token, shown only once) could then be replaced by another before it was seen.
  const close = () => {
    if (!action.busy) onClose()
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (action.busy) return
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
    <Dialog
      open={open}
      onClose={close}
      title={t('settings.tokens.newTitle')}
      actions={
        <>
          <Button onClick={close} disabled={action.busy}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form={formId} variant="primary" loading={action.busy}>
            {t('settings.tokens.create')}
          </Button>
        </>
      }
    >
      <form id={formId} className="settings-form" onSubmit={submit}>
        <Field label={t('settings.tokens.name')} required>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={100}
            placeholder={t('settings.tokens.namePlaceholder')}
            autoComplete="off"
            data-autofocus
          />
        </Field>
        <Field label={t('settings.tokens.expiresLabel')}>
          <Select value={days} onChange={(e) => setDays(e.target.value)}>
            {EXPIRY.map((x) => (
              <option key={x.label} value={x.days ?? ''}>
                {t(x.label)}
              </option>
            ))}
          </Select>
        </Field>
        <Alert error={action.error} />
      </form>
    </Dialog>
  )
}

function NewToken({ made, onDone }: { made: Made; onDone: () => void }) {
  const t = useT()
  return (
    <Notice
      tone="warning"
      title={t('settings.tokens.newToken')}
      action={<Button onClick={onDone}>{t('settings.tokens.done')}</Button>}
    >
      <div className="settings-panel-body">
        <p className="settings-text">
          {t.rich('settings.tokens.made', { name: <strong>{made.name}</strong>, b: (text) => <strong>{text}</strong> })}
        </p>
        <CopyField value={made.token} label={t('settings.tokens.newToken')} />
        <p className="settings-muted">{t('settings.tokens.held')}</p>
      </div>
    </Notice>
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
      <p className="settings-muted">
        {t.rich('settings.tokens.setup.endpoint', { url: <code>{url}</code>, code: (text) => <code>{text}</code> })}
      </p>
      {snippets.map((s) => (
        <Panel
          key={s.label}
          as="section"
          padded
          className="settings-panel-body"
          aria-label={t('settings.tokens.setup.named', { name: s.label })}
        >
          <div className="settings-snippet-head">
            <div className="settings-section-titles">
              <Heading level={3} size="subsection">
                {s.label}
              </Heading>
              <p className="settings-section-help">{s.note}</p>
            </div>
            <CopyButton size="sm" text={s.text} name={t('settings.tokens.setup.theNamed', { name: s.label })} />
          </div>
          <pre>
            <code>{s.text}</code>
          </pre>
        </Panel>
      ))}
    </>
  )
}
