import { useContext, useEffect, useId, useRef, useState } from 'react'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Icon } from '../ui/Icon'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Spinner } from '../ui/Spinner'
import { type CaptureInfo, coordinates, currentPosition, type Location, manualLocation, PositionError } from './capture'
import { SubmitCapturesContext } from './submit'
import './capture.css'

export function LocationField({
  label,
  field,
  value,
  onChange,
  info,
  readOnly,
}: {
  label?: string
  field: string
  value: Location | undefined
  onChange: (value: Location | undefined) => void
  info?: CaptureInfo
  readOnly?: boolean
}) {
  const t = useT()
  const f = useFormat()
  const ids = useId()
  const submit = useContext(SubmitCapturesContext)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)
  const [manual, setManual] = useState(false)
  const [lat, setLat] = useState('')
  const [lon, setLon] = useState('')
  const [invalid, setInvalid] = useState(false)
  const current = useRef(value)
  current.current = value
  const at = info?.capture ?? 'button'

  const failMessage = (e: unknown) =>
    t(`apps.location.failed.${e instanceof PositionError ? e.reason : 'unavailable'}` as 'apps.location.failed.denied')

  const capture = async (): Promise<Location | undefined> => {
    setBusy(true)
    setFailure(null)
    try {
      const loc = await currentPosition()
      onChange(loc)
      setManual(false)
      return loc
    } catch (e) {
      setFailure(failMessage(e))
      setManual(true)
      return undefined
    } finally {
      setBusy(false)
    }
  }
  const captureRef = useRef(capture)
  captureRef.current = capture

  useEffect(() => {
    if (!submit || readOnly || at !== 'submit') return
    return submit.register(field, async () => {
      if (current.current) return {}
      const loc = await captureRef.current()
      if (loc) return { value: loc }
      return info?.required ? { block: t('apps.location.submitBlocked') } : {}
    })
  }, [submit, readOnly, at, field, info?.required, t])

  const applyManual = () => {
    const loc = manualLocation(lat, lon)
    setInvalid(!loc)
    if (!loc) return
    onChange(loc)
    setManual(false)
    setFailure(null)
    setLat('')
    setLon('')
  }

  const heading = label ?? t('apps.location.label')
  return (
    <fieldset className="ui-capture" data-submit-free aria-describedby={readOnly ? undefined : `${ids}-note`}>
      <legend className="ui-capture-label">{heading}</legend>
      {value ? (
        <p className="ui-capture-value" role="status">
          <Icon name="pin" size={16} />
          <span className="ui-capture-value-text">
            <strong>{coordinates(value, f)}</strong>
            <span className="ui-capture-detail">
              {value.accuracy_m !== undefined
                ? t('apps.location.accuracy', { meters: f.number(Math.round(value.accuracy_m)) })
                : t('apps.location.noAccuracy')}
              {' · '}
              {f.dateTime(value.captured_at)}
              {' · '}
              {t(value.source === 'device' ? 'apps.location.fromDevice' : 'apps.location.byHand')}
            </span>
          </span>
        </p>
      ) : (
        readOnly && <p className="ui-capture-empty">{t('apps.location.none')}</p>
      )}
      {!readOnly && (
        <div className="ui-capture-actions">
          <Button icon="pin" loading={busy} disabled={busy} onClick={() => void capture()}>
            {t(value ? 'apps.location.captureAgain' : 'apps.location.capture')}
          </Button>
          <Button
            variant="ghost"
            aria-expanded={manual}
            aria-controls={`${ids}-manual`}
            onClick={() => setManual((open) => !open)}
          >
            {t(manual ? 'apps.location.hideManual' : 'apps.location.enterByHand')}
          </Button>
          {value && (
            <Button variant="ghost" onClick={() => onChange(undefined)}>
              {t('apps.location.remove')}
            </Button>
          )}
        </div>
      )}
      {busy && (
        <p role="status" className="ui-capture-progress">
          <Spinner size="sm" />
          {t('apps.location.capturing')}
        </p>
      )}
      {failure && <Notice tone="danger">{failure}</Notice>}
      {!readOnly && manual && (
        <div id={`${ids}-manual`} className="ui-capture-manual">
          <Field label={t('apps.location.latitude')}>
            <Input
              inputMode="decimal"
              autoComplete="off"
              value={lat}
              invalid={invalid}
              aria-describedby={invalid ? `${ids}-manual-error` : undefined}
              onChange={(e) => setLat(e.target.value)}
            />
          </Field>
          <Field label={t('apps.location.longitude')}>
            <Input
              inputMode="decimal"
              autoComplete="off"
              value={lon}
              invalid={invalid}
              aria-describedby={invalid ? `${ids}-manual-error` : undefined}
              onChange={(e) => setLon(e.target.value)}
            />
          </Field>
          <Button onClick={applyManual}>{t('apps.location.useManual')}</Button>
          {invalid && (
            <div id={`${ids}-manual-error`} className="ui-capture-error">
              <Notice tone="danger">{t('apps.location.manualInvalid')}</Notice>
            </div>
          )}
        </div>
      )}
      {!readOnly && (
        <p id={`${ids}-note`} className="ui-capture-note">
          {t(at === 'submit' ? 'apps.location.submitNote' : 'apps.location.buttonNote')}
          {info?.editable === false && ` ${t('apps.location.locked')}`}
        </p>
      )}
    </fieldset>
  )
}
