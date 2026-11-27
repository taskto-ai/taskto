import { type FormEvent, useId, useState } from 'react'
import type { App } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { AppAvatar } from './AppAvatar'
import { APP_ICONS, isAppIcon } from './appIcons'
import './IconDialog.css'

const listed = (icon: string) => (APP_ICONS as readonly string[]).includes(icon)

export function IconDialog({
  app,
  onClose,
  onSave,
}: {
  app: App
  onClose: () => void
  onSave: (icon: string) => Promise<string | null>
}) {
  const t = useT()
  const formId = useId()
  const group = useId()
  const [picked, setPicked] = useState(listed(app.icon) ? app.icon : '')
  const [typed, setTyped] = useState(listed(app.icon) ? '' : app.icon)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const custom = typed.trim()
  const icon = custom === '' ? picked : custom
  const invalid = custom !== '' && !isAppIcon(custom)
  const unchanged = icon === app.icon

  const pick = (value: string) => {
    setPicked(value)
    setTyped('')
    setError(null)
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || invalid || unchanged) return
    setBusy(true)
    setError(null)
    const failed = await onSave(icon)
    setBusy(false)
    setError(failed)
  }

  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!busy}
      title={t('apps.iconTitle', { name: app.name })}
      size="md"
      actions={
        <>
          <Button disabled={busy} onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={busy} disabled={invalid || unchanged}>
            {t('apps.save')}
          </Button>
        </>
      }
    >
      <form id={formId} className="apps-form" onSubmit={submit}>
        <div className="apps-icon-preview">
          <AppAvatar name={app.name} icon={invalid ? '' : icon} size="lg" decorative />
          <span className="apps-icon-preview-name">
            <span className="apps-note">{t('apps.iconPreview')}</span>
            {app.name}
          </span>
        </div>
        <div role="radiogroup" aria-label={t('apps.changeIcon')} className="apps-icon-choices">
          <label className="apps-icon-none">
            <input
              type="radio"
              className="ui-control apps-icon-input"
              name={group}
              checked={custom === '' && picked === ''}
              onChange={() => pick('')}
            />
            <span className="apps-icon-face apps-icon-face-text">{t('apps.iconNone')}</span>
          </label>
          <div className="apps-icon-grid">
            {APP_ICONS.map((emoji) => (
              <label key={emoji} className="apps-icon-choice">
                <input
                  type="radio"
                  className="ui-control apps-icon-input"
                  name={group}
                  aria-label={emoji}
                  checked={custom === '' && picked === emoji}
                  onChange={() => pick(emoji)}
                />
                <span className="apps-icon-face" aria-hidden="true">
                  {emoji}
                </span>
              </label>
            ))}
          </div>
        </div>
        <Field
          label={t('apps.iconCustom')}
          hint={t('apps.iconCustomHelp')}
          error={invalid ? t('apps.iconInvalid') : undefined}
        >
          <Input
            value={typed}
            onChange={(e) => {
              setTyped(e.target.value)
              setError(null)
            }}
            autoComplete="off"
            maxLength={32}
          />
        </Field>
        {error && <Notice tone="danger">{error}</Notice>}
      </form>
    </Dialog>
  )
}
