// Your own two-factor login (docs/api.md#two-factor-login): turn it on with the password, a QR
// code (or the secret as text) for an authenticator app, and a code from it; then ten recovery
// codes, shown once; turn it off or make new recovery codes with the password and a code. The
// secret and the recovery codes live in this panel's state while they are on screen, and nowhere
// else (no storage, no log).

import { type FormEvent, useEffect, useState } from 'react'
import { api } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Alert, CopyButton, CopyField, FormActions, Loading, Section, useAction } from './parts'
import { qrSvg, svgSource } from './qr'

type Status = { enabled: boolean; recovery_codes_left: number }
type Setup = { enrolment: string; secret: string; otpauth_uri: string }

export function TwoFactorPanel() {
  const t = useT()
  const status = useLoad(() => api<Status>('/api/me/two-factor'), [])
  const [setup, setSetup] = useState<Setup | null>(null)
  const [codes, setCodes] = useState<string[] | null>(null)

  const done = () => {
    setSetup(null)
    status.reload()
  }

  return (
    <Section
      title={t('settings.twoFactor.title')}
      help={t('settings.twoFactor.intro')}
      actions={
        status.data &&
        (status.data.enabled ? (
          <Badge tone="success" dot>
            {t('settings.twoFactor.onBadge')}
          </Badge>
        ) : (
          <Badge tone="neutral">{t('settings.twoFactor.offBadge')}</Badge>
        ))
      }
    >
      <Alert error={status.error} />
      {codes ? (
        <RecoveryCodes
          codes={codes}
          onDone={() => {
            setCodes(null)
            status.reload()
          }}
        />
      ) : !status.data ? (
        !status.error && <Loading lines={2} />
      ) : status.data.enabled ? (
        <TurnedOn status={status.data} onCodes={setCodes} onOff={done} />
      ) : setup ? (
        <Confirm
          setup={setup}
          onCancel={() => setSetup(null)}
          onFailed={status.reload}
          onCodes={(c) => {
            setSetup(null)
            setCodes(c)
          }}
        />
      ) : (
        <Start onStarted={setSetup} />
      )}
    </Section>
  )
}

function Start({ onStarted }: { onStarted: (s: Setup) => void }) {
  const t = useT()
  const [password, setPassword] = useState('')
  const action = useAction()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let made: Setup | undefined
    const ok = await action.run(async () => {
      made = await api<Setup>('/api/me/two-factor/enrol', { method: 'POST', body: { password } })
    })
    if (ok && made) {
      setPassword('')
      onStarted(made)
    }
  }
  return (
    <Panel padded>
      <form className="settings-form" onSubmit={submit}>
        <p className="settings-text">{t('settings.twoFactor.off')}</p>
        <Field label={t('settings.twoFactor.password')}>
          <Input
            required
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </Field>
        <Alert error={action.error} />
        <FormActions>
          <Button type="submit" loading={action.busy}>
            {t('settings.twoFactor.turnOn')}
          </Button>
        </FormActions>
      </form>
    </Panel>
  )
}

function Confirm({
  setup,
  onCancel,
  onFailed,
  onCodes,
}: {
  setup: Setup
  onCancel: () => void
  onFailed: () => void
  onCodes: (codes: string[]) => void
}) {
  const t = useT()
  const [code, setCode] = useState('')
  const action = useAction()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let got: { recovery_codes: string[] } | undefined
    const ok = await action.run(async () => {
      got = await api<{ recovery_codes: string[] }>('/api/me/two-factor/confirm', {
        method: 'POST',
        body: { enrolment: setup.enrolment, code },
      })
    })
    if (ok && got) onCodes(got.recovery_codes)
    // A confirmation whose answer was lost may have turned it on: the status says, and a panel that
    // finds it on shows that instead of this form.
    else onFailed()
  }
  return (
    <Panel padded>
      <form className="settings-form" onSubmit={submit}>
        <p className="settings-text">{t('settings.twoFactor.step1')}</p>
        <Qr text={setup.otpauth_uri} />
        <p className="settings-muted">{t('settings.twoFactor.orType')}</p>
        <CopyField
          value={setup.secret}
          label={t('settings.twoFactor.secret')}
          name={t('settings.twoFactor.secretName')}
        />
        <Field label={t('settings.twoFactor.step2')}>
          <Input
            required
            value={code}
            onChange={(e) => setCode(e.target.value)}
            inputMode="numeric"
            autoComplete="one-time-code"
            spellCheck={false}
          />
        </Field>
        <Alert error={action.error} />
        <FormActions>
          <Button type="submit" loading={action.busy} disabled={code.trim() === ''}>
            {t('settings.twoFactor.confirm')}
          </Button>
          <Button variant="ghost" onClick={onCancel} disabled={action.busy}>
            {t('settings.twoFactor.cancel')}
          </Button>
        </FormActions>
      </form>
    </Panel>
  )
}

// The QR code, drawn in the browser. Where it can't be (the writer fails to load), the secret as
// text, which is always shown beside it, is the way.
function Qr({ text }: { text: string }) {
  const t = useT()
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    let live = true
    setSrc(null)
    setFailed(false)
    qrSvg(text).then(
      (svg) => live && setSrc(svgSource(svg)),
      () => live && setFailed(true),
    )
    return () => {
      live = false
    }
  }, [text])
  if (failed) return <Notice tone="warning">{t('settings.twoFactor.qrFailed')}</Notice>
  return (
    <div className="settings-qr">
      {src ? <img src={src} alt={t('settings.twoFactor.qrAlt')} /> : <Loading lines={1} />}
    </div>
  )
}

function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const t = useT()
  return (
    <Notice tone="warning" title={t('settings.twoFactor.codesTitle')}>
      <div className="settings-panel-body">
        <p className="settings-text">{t('settings.twoFactor.codesHelp')}</p>
        <ul className="settings-recovery" aria-label={t('settings.twoFactor.codesTitle')}>
          {codes.map((c) => (
            <li key={c}>
              <code>{c}</code>
            </li>
          ))}
        </ul>
        <FormActions>
          <CopyButton text={codes.join('\n')} name={t('settings.twoFactor.codesName')} />
          <Button onClick={onDone}>{t('settings.twoFactor.codesSaved')}</Button>
        </FormActions>
      </div>
    </Notice>
  )
}

function TurnedOn({
  status,
  onCodes,
  onOff,
}: {
  status: Status
  onCodes: (codes: string[]) => void
  onOff: () => void
}) {
  const t = useT()
  const [form, setForm] = useState({ password: '', code: '' })
  const [confirmDialog, confirm] = useConfirm()
  const action = useAction()
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const ready = form.password !== '' && form.code.trim() !== ''

  const newCodes = async () => {
    let got: { recovery_codes: string[] } | undefined
    const ok = await action.run(async () => {
      got = await api<{ recovery_codes: string[] }>('/api/me/two-factor/recovery-codes', { method: 'POST', body: form })
    })
    if (ok && got) {
      setForm({ password: '', code: '' })
      onCodes(got.recovery_codes)
    }
  }
  const turnOff = async () => {
    const ok = await action.run(() => api('/api/me/two-factor/disable', { method: 'POST', body: form }))
    if (ok) {
      setForm({ password: '', code: '' })
      onOff()
    }
  }

  const askOff = async () => {
    const yes = await confirm({
      title: t('settings.twoFactor.turnOffQuestion'),
      confirmLabel: t('settings.twoFactor.turnOffConfirm'),
      tone: 'danger',
    })
    if (yes) turnOff()
  }

  return (
    <Panel padded>
      <form className="settings-form" onSubmit={(e) => e.preventDefault()}>
        <p className="settings-text" role="status">
          {t('settings.twoFactor.on')}
        </p>
        <p className="settings-muted">{t('settings.twoFactor.left', { count: status.recovery_codes_left })}</p>
        <p className="settings-muted">{t('settings.twoFactor.changeHelp')}</p>
        <Field label={t('settings.twoFactor.password')}>
          <Input type="password" value={form.password} onChange={set('password')} autoComplete="current-password" />
        </Field>
        <Field label={t('settings.twoFactor.code')}>
          <Input value={form.code} onChange={set('code')} autoComplete="one-time-code" spellCheck={false} />
        </Field>
        <Alert error={action.error} />
        <FormActions>
          <Button disabled={action.busy || !ready} onClick={newCodes}>
            {t('settings.twoFactor.newCodes')}
          </Button>
          <Button variant="danger" disabled={action.busy || !ready} onClick={askOff}>
            {t('settings.twoFactor.turnOff')}
          </Button>
        </FormActions>
      </form>
      {confirmDialog}
    </Panel>
  )
}
