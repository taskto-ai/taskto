// Your own two-factor login (docs/api.md#two-factor-login): turn it on with the password, a QR
// code (or the secret as text) for an authenticator app, and a code from it; then ten recovery
// codes, shown once; turn it off or make new recovery codes with the password and a code. The
// secret and the recovery codes live in this panel's state while they are on screen, and nowhere
// else (no storage, no log).

import { type FormEvent, useEffect, useState } from 'react'
import { api } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, CopyButton, useAction } from './parts'
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
    <section className="tt-panel" aria-labelledby="tt-two-factor">
      <h2 id="tt-two-factor">{t('settings.twoFactor.title')}</h2>
      <p className="tt-muted tt-small">{t('settings.twoFactor.intro')}</p>
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
        !status.error && <p className="tt-muted">{t('app.loading')}</p>
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
    </section>
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
    <form className="tt-form" onSubmit={submit}>
      <p>{t('settings.twoFactor.off')}</p>
      <label>
        {t('settings.twoFactor.password')}
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          autoComplete="current-password"
        />
      </label>
      <Alert error={action.error} />
      <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
        {t('settings.twoFactor.turnOn')}
      </button>
    </form>
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
    <form className="tt-form" onSubmit={submit}>
      <p>{t('settings.twoFactor.step1')}</p>
      <Qr text={setup.otpauth_uri} />
      <p className="tt-small">{t('settings.twoFactor.orType')}</p>
      <div className="tt-copy">
        <input
          readOnly
          value={setup.secret}
          aria-label={t('settings.twoFactor.secret')}
          onFocus={(e) => e.target.select()}
        />
        <CopyButton text={setup.secret} name={t('settings.twoFactor.secretName')} />
      </div>
      <label>
        {t('settings.twoFactor.step2')}
        <input
          value={code}
          onChange={(e) => setCode(e.target.value)}
          required
          inputMode="numeric"
          autoComplete="one-time-code"
          spellCheck={false}
        />
      </label>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={action.busy || code.trim() === ''}>
          {t('settings.twoFactor.confirm')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel} disabled={action.busy}>
          {t('settings.twoFactor.cancel')}
        </button>
      </div>
    </form>
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
  if (failed) return <p className="tt-muted tt-small">{t('settings.twoFactor.qrFailed')}</p>
  return (
    <div className="tt-qr">
      {src ? (
        <img src={src} alt={t('settings.twoFactor.qrAlt')} />
      ) : (
        <span className="tt-muted">{t('app.loading')}</span>
      )}
    </div>
  )
}

function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const t = useT()
  return (
    <div className="tt-notice" role="status">
      <p>
        <strong>{t('settings.twoFactor.codesTitle')}</strong> {t('settings.twoFactor.codesHelp')}
      </p>
      <ul className="tt-recovery" aria-label={t('settings.twoFactor.codesTitle')}>
        {codes.map((c) => (
          <li key={c}>
            <code>{c}</code>
          </li>
        ))}
      </ul>
      <div className="tt-row-actions">
        <CopyButton text={codes.join('\n')} name={t('settings.twoFactor.codesName')} />
        <button type="button" className="tt-button tt-primary" onClick={onDone}>
          {t('settings.twoFactor.codesSaved')}
        </button>
      </div>
    </div>
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
  const [off, setOff] = useState(false)
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
      setOff(false)
      onOff()
    }
  }

  return (
    <form className="tt-form" onSubmit={(e) => e.preventDefault()}>
      <p role="status">{t('settings.twoFactor.on')}</p>
      <p className="tt-muted tt-small">{t('settings.twoFactor.left', { count: status.recovery_codes_left })}</p>
      <p className="tt-muted tt-small">{t('settings.twoFactor.changeHelp')}</p>
      <label>
        {t('settings.twoFactor.password')}
        <input type="password" value={form.password} onChange={set('password')} autoComplete="current-password" />
      </label>
      <label>
        {t('settings.twoFactor.code')}
        <input value={form.code} onChange={set('code')} autoComplete="one-time-code" spellCheck={false} />
      </label>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="button" className="tt-button" disabled={action.busy || !ready} onClick={newCodes}>
          {t('settings.twoFactor.newCodes')}
        </button>
        {off ? (
          <>
            <button type="button" className="tt-button tt-danger" disabled={action.busy || !ready} onClick={turnOff}>
              {t('settings.twoFactor.turnOffConfirm')}
            </button>
            <button type="button" className="tt-button" onClick={() => setOff(false)} disabled={action.busy}>
              {t('settings.twoFactor.cancel')}
            </button>
          </>
        ) : (
          <button type="button" className="tt-button" disabled={action.busy || !ready} onClick={() => setOff(true)}>
            {t('settings.twoFactor.turnOff')}
          </button>
        )}
      </div>
    </form>
  )
}
