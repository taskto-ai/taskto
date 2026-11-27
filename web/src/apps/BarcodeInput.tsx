import { useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Icon } from '../ui/Icon'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Spinner } from '../ui/Spinner'
import type { CaptureInfo } from './capture'
import { canUseCamera, createDetector, DecoderError, scan } from './scanner'
import './capture.css'

export function BarcodeField({
  label,
  value,
  onChange,
  info,
}: {
  label?: string
  value: string
  onChange: (value: string) => void
  info?: CaptureInfo
}) {
  const t = useT()
  const ids = useId()
  const video = useRef<HTMLVideoElement>(null)
  const [scanning, setScanning] = useState(false)
  const [problem, setProblem] = useState<string | null>(null)
  const [scanned, setScanned] = useState<string | null>(null)
  const change = useRef(onChange)
  change.current = onChange
  const format = info?.format ?? ''
  const camera = canUseCamera()

  useEffect(() => {
    if (!scanning) return
    let live = true
    let stream: MediaStream | undefined
    let session: ReturnType<typeof scan> | undefined
    const fail = (message: string) => {
      if (!live) return
      setProblem(message)
      setScanning(false)
    }
    void (async () => {
      try {
        stream = await navigator.mediaDevices.getUserMedia({
          video: { facingMode: { ideal: 'environment' } },
          audio: false,
        })
      } catch (e) {
        const name = e instanceof DOMException ? e.name : ''
        return fail(
          name === 'NotAllowedError' || name === 'SecurityError'
            ? t('apps.barcode.denied')
            : name === 'NotFoundError' || name === 'OverconstrainedError'
              ? t('apps.barcode.noDevice')
              : t('apps.barcode.cameraFailed', { error: errorText(e) }),
        )
      }
      if (!live) {
        for (const track of stream.getTracks()) track.stop()
        return
      }
      try {
        const el = video.current
        if (!el) return fail(t('apps.barcode.cameraFailed', { error: '' }))
        el.srcObject = stream
        await el.play?.()
        const { detector } = await createDetector(format ? [format] : [])
        if (!live) return
        session = scan(el, detector)
        const found = await session.result
        if (live && found) {
          change.current(found.rawValue)
          setScanned(found.rawValue)
          setScanning(false)
        }
      } catch (e) {
        fail(
          e instanceof DecoderError
            ? t('apps.barcode.decoderStopped')
            : t('apps.barcode.decoderFailed', { error: errorText(e) }),
        )
      }
    })()
    return () => {
      live = false
      session?.stop()
      for (const track of stream?.getTracks() ?? []) track.stop()
    }
  }, [scanning, format, t])

  const heading = label ?? t('apps.barcode.label')
  return (
    <fieldset className="ui-capture" data-submit-free aria-describedby={`${ids}-note`}>
      <legend className="ui-capture-label">{heading}</legend>
      <div className="ui-capture-row">
        <Field label={t('apps.barcode.typed')}>
          <Input
            value={value}
            autoComplete="off"
            autoCapitalize="off"
            spellCheck={false}
            onChange={(e) => {
              setScanned(null)
              onChange(e.target.value)
            }}
          />
        </Field>
        {camera && (
          <Button
            icon={scanning ? 'stop' : 'search'}
            aria-pressed={scanning}
            onClick={() => {
              setProblem(null)
              setScanned(null)
              setScanning((on) => !on)
            }}
          >
            {t(scanning ? 'apps.barcode.stop' : 'apps.barcode.scan')}
          </Button>
        )}
      </div>
      {scanning && (
        <div className="ui-capture-camera">
          <video ref={video} playsInline muted aria-label={t('apps.barcode.view')} />
          <p role="status" className="ui-capture-progress">
            <Spinner size="sm" />
            {t('apps.barcode.scanning')}
          </p>
        </div>
      )}
      {scanned && (
        <p role="status" className="ui-capture-done">
          <Icon name="check" size={16} />
          {t('apps.barcode.scanned', { code: scanned })}
        </p>
      )}
      {problem && <Notice tone="danger">{problem}</Notice>}
      <p id={`${ids}-note`} className="ui-capture-note">
        {camera ? t('apps.barcode.note') : t('apps.barcode.noCamera')}
        {format &&
          ` ${t('apps.barcode.format', { format: t(`apps.barcode.formats.${format}` as 'apps.barcode.formats.qr_code') })}`}
      </p>
    </fieldset>
  )
}
