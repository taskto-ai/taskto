// A location field's input (docs/api.md, "Location, signature and barcode fields").
//
// The position is read once, in the foreground: when the person presses "Capture location" or,
// for a field whose capture is "submit", as they submit the form (submit.ts). It is never watched
// and never read in the background. Where the browser can't give it (the person said no, the
// device has no position, the page isn't secure) the reason is said in words and the person types
// the coordinates, which are stored as entered by hand. What is stored is evidence: the server
// refuses to change it afterwards (unless the field says it is editable), so a stored location is
// drawn read-only.

import { useContext, useEffect, useId, useRef, useState } from 'react'
import { useFormat, useT } from '../i18n'
import { type CaptureInfo, coordinates, currentPosition, type Location, manualLocation, PositionError } from './capture'
import { SubmitCapturesContext } from './submit'

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
  // Called with the new location, or undefined when the person removes it.
  onChange: (value: Location | undefined) => void
  info?: CaptureInfo
  // Only shows what is stored.
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

  // A field taken at submit: the screen asks for it as the form's button is pressed, once, if the
  // person hasn't already given one.
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
    <fieldset className="tt-capture tt-location" aria-describedby={readOnly ? undefined : `${ids}-note`}>
      <legend className="tt-capture-label">{heading}</legend>
      {value ? (
        <p className="tt-capture-value" role="status">
          <strong>{coordinates(value, f)}</strong>
          <span className="tt-muted tt-small">
            {' '}
            {value.accuracy_m !== undefined
              ? t('apps.location.accuracy', { meters: f.number(Math.round(value.accuracy_m)) })
              : t('apps.location.noAccuracy')}
            {' · '}
            {f.dateTime(value.captured_at)}
            {' · '}
            {t(value.source === 'device' ? 'apps.location.fromDevice' : 'apps.location.byHand')}
          </span>
        </p>
      ) : (
        readOnly && <p className="tt-muted">{t('apps.location.none')}</p>
      )}
      {!readOnly && (
        <div className="tt-capture-actions">
          <button type="button" className="tt-button" disabled={busy} onClick={() => void capture()}>
            {t(value ? 'apps.location.captureAgain' : 'apps.location.capture')}
          </button>
          <button
            type="button"
            className="tt-button"
            aria-expanded={manual}
            aria-controls={`${ids}-manual`}
            onClick={() => setManual((open) => !open)}
          >
            {t(manual ? 'apps.location.hideManual' : 'apps.location.enterByHand')}
          </button>
          {value && (
            <button type="button" className="tt-button" onClick={() => onChange(undefined)}>
              {t('apps.location.remove')}
            </button>
          )}
        </div>
      )}
      {busy && (
        <p role="status" className="tt-muted tt-small">
          {t('apps.location.capturing')}
        </p>
      )}
      {failure && (
        <p role="alert" className="tt-error tt-small">
          {failure}
        </p>
      )}
      {!readOnly && manual && (
        <div id={`${ids}-manual`} className="tt-capture-manual">
          <label className="tt-capture-field">
            {t('apps.location.latitude')}
            <input
              className="tt-capture-input"
              inputMode="decimal"
              autoComplete="off"
              value={lat}
              aria-invalid={invalid || undefined}
              onChange={(e) => setLat(e.target.value)}
            />
          </label>
          <label className="tt-capture-field">
            {t('apps.location.longitude')}
            <input
              className="tt-capture-input"
              inputMode="decimal"
              autoComplete="off"
              value={lon}
              aria-invalid={invalid || undefined}
              onChange={(e) => setLon(e.target.value)}
            />
          </label>
          <button type="button" className="tt-button" onClick={applyManual}>
            {t('apps.location.useManual')}
          </button>
          {invalid && (
            <p role="alert" className="tt-error tt-small">
              {t('apps.location.manualInvalid')}
            </p>
          )}
        </div>
      )}
      {!readOnly && (
        <p id={`${ids}-note`} className="tt-muted tt-small">
          {t(at === 'submit' ? 'apps.location.submitNote' : 'apps.location.buttonNote')}
          {info?.editable === false && ` ${t('apps.location.locked')}`}
        </p>
      )}
    </fieldset>
  )
}
