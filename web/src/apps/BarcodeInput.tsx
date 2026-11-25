// A barcode field's input (docs/api.md, "Location, signature and barcode fields"): the code's
// text, which can always be typed, and can be scanned with the camera where the browser lets the
// page use one. The decoder is the browser's own BarcodeDetector if it has one, else a bundled one
// loaded on first use (scanner.ts). A camera that is blocked, missing or insecure, or a decoder that
// can't load (offline, before it was ever cached), is said in words and leaves the typed field as
// it was: nothing here depends on scanning working.

import { useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import type { CaptureInfo } from './capture'
import { canUseCamera, createDetector, DecoderError, scan } from './scanner'

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
        // A decoder that can't work (it isn't supported, or failed every frame from the start) turns the
        // camera off (the cleanup of this effect) and leaves the typed field, which the message says.
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
    <fieldset className="tt-capture" aria-describedby={`${ids}-note`}>
      <legend className="tt-capture-label">{heading}</legend>
      <label className="tt-capture-field">
        {t('apps.barcode.typed')}
        <input
          className="tt-capture-input"
          value={value}
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          onChange={(e) => {
            setScanned(null)
            onChange(e.target.value)
          }}
        />
      </label>
      <div className="tt-capture-actions">
        {camera && (
          <button
            type="button"
            className="tt-button"
            aria-pressed={scanning}
            onClick={() => {
              setProblem(null)
              setScanned(null)
              setScanning((on) => !on)
            }}
          >
            {t(scanning ? 'apps.barcode.stop' : 'apps.barcode.scan')}
          </button>
        )}
      </div>
      {scanning && (
        <div className="tt-barcode-camera">
          <video ref={video} playsInline muted aria-label={t('apps.barcode.view')} />
          <p role="status" className="tt-muted tt-small">
            {t('apps.barcode.scanning')}
          </p>
        </div>
      )}
      {scanned && (
        <p role="status" className="tt-small">
          {t('apps.barcode.scanned', { code: scanned })}
        </p>
      )}
      {problem && (
        <p role="alert" className="tt-error tt-small">
          {problem}
        </p>
      )}
      <p id={`${ids}-note`} className="tt-muted tt-small">
        {camera ? t('apps.barcode.note') : t('apps.barcode.noCamera')}
        {format &&
          ` ${t('apps.barcode.format', { format: t(`apps.barcode.formats.${format}` as 'apps.barcode.formats.qr_code') })}`}
      </p>
    </fieldset>
  )
}
