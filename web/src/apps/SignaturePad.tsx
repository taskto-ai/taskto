// A signature field's input (docs/api.md, "Location, signature and barcode fields").
//
// Two ways to sign, both ending in a PNG that is uploaded exactly like a photo (photos.ts, the same
// three steps and the same resume) and whose id the record holds:
//
// - drawing on a pad, with a finger, a stylus or a mouse. The pad is a canvas that takes pointer
//   events and has `touch-action: none`, on the canvas only: a touch that starts on the pad draws
//   and does not scroll, and the rest of the page scrolls as ever. The strokes are kept as fractions
//   of the pad's size, so turning the device (the pad is resized) redraws them where they were;
// - typing the name, with a checkbox saying that is the signature, for anyone who can't or would
//   rather not draw (a keyboard, a switch device, a screen reader). The name is drawn into the same
//   kind of picture.
//
// Once a record holds a signature the server refuses to change it, so a stored one is drawn
// read-only; before that it can be redone, and the discarded picture is removed from the server.

import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { errorText } from '../api'
import { useT } from '../i18n'
import type { AppFiles } from './PhotoInput'
import { discardPhoto, photoUrl, thumbnailUrl, uploadPhoto } from './photos'

// A point of a stroke as fractions of the pad's width and height.
type Point = { x: number; y: number }
type Stroke = Point[]

// The picture's size in pixels: three times as wide as high, as the pad is.
export const SIGNATURE_WIDTH = 600
export const SIGNATURE_HEIGHT = 200
const INK = 'rgb(17 17 16)'
const PAPER = '#fff'

// Draws strokes into a context of a canvas of w by h pixels, ink on paper.
export function drawStrokes(ctx: CanvasRenderingContext2D, strokes: Stroke[], w: number, h: number, ink: number) {
  ctx.fillStyle = PAPER
  ctx.fillRect(0, 0, w, h)
  ctx.strokeStyle = INK
  ctx.fillStyle = INK
  ctx.lineWidth = ink
  ctx.lineCap = 'round'
  ctx.lineJoin = 'round'
  for (const stroke of strokes) {
    if (stroke.length === 0) continue
    if (stroke.length === 1) {
      // A tap is a dot.
      ctx.beginPath()
      ctx.arc(stroke[0].x * w, stroke[0].y * h, ink / 2, 0, Math.PI * 2)
      ctx.fill()
      continue
    }
    ctx.beginPath()
    ctx.moveTo(stroke[0].x * w, stroke[0].y * h)
    for (const p of stroke.slice(1)) ctx.lineTo(p.x * w, p.y * h)
    ctx.stroke()
  }
}

// The PNG of strokes, at the picture's fixed size.
export function strokesToCanvas(strokes: Stroke[]): HTMLCanvasElement {
  const canvas = document.createElement('canvas')
  canvas.width = SIGNATURE_WIDTH
  canvas.height = SIGNATURE_HEIGHT
  const ctx = canvas.getContext('2d')
  if (ctx) drawStrokes(ctx, strokes, SIGNATURE_WIDTH, SIGNATURE_HEIGHT, 3)
  return canvas
}

// The PNG of a typed name, centred, in a script-like italic.
export function nameToCanvas(name: string): HTMLCanvasElement {
  const canvas = document.createElement('canvas')
  canvas.width = SIGNATURE_WIDTH
  canvas.height = SIGNATURE_HEIGHT
  const ctx = canvas.getContext('2d')
  if (ctx) {
    ctx.fillStyle = PAPER
    ctx.fillRect(0, 0, SIGNATURE_WIDTH, SIGNATURE_HEIGHT)
    ctx.fillStyle = INK
    ctx.textAlign = 'center'
    ctx.textBaseline = 'middle'
    let size = 64
    ctx.font = `italic ${size}px "Snell Roundhand", "Segoe Script", "Brush Script MT", cursive, serif`
    // Shrink a long name to fit the picture.
    const width = ctx.measureText(name).width
    if (width > SIGNATURE_WIDTH - 40) {
      size = Math.max(16, Math.floor((size * (SIGNATURE_WIDTH - 40)) / width))
      ctx.font = `italic ${size}px "Snell Roundhand", "Segoe Script", "Brush Script MT", cursive, serif`
    }
    ctx.fillText(name, SIGNATURE_WIDTH / 2, SIGNATURE_HEIGHT / 2, SIGNATURE_WIDTH - 40)
  }
  return canvas
}

export function canvasToPng(canvas: HTMLCanvasElement): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error('the signature could not be saved'))), 'image/png')
  })
}

type Mode = 'draw' | 'type'

export function SignatureField({
  label,
  files,
  field,
  value,
  onChange,
  readOnly,
}: {
  label?: string
  files: AppFiles | null
  field: string
  // The id of the signature's attachment.
  value: string | undefined
  onChange: (id: string | undefined) => void
  readOnly?: boolean
}) {
  const t = useT()
  const ids = useId()
  const heading = label ?? t('apps.signature.label')
  const canvas = useRef<HTMLCanvasElement>(null)
  const strokes = useRef<Stroke[]>([])
  const active = useRef<number | null>(null)
  const [count, setCount] = useState(0)
  const [mode, setMode] = useState<Mode>('draw')
  const [name, setName] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [saving, setSaving] = useState(false)
  const [problem, setProblem] = useState<string | null>(null)
  // The signatures uploaded here and not yet in a record: signing again discards the last.
  const mine = useRef(new Set<string>())
  const uploads = !!files?.uploads

  const redraw = useCallback(() => {
    const el = canvas.current
    const ctx = el?.getContext('2d')
    if (!el || !ctx) return
    drawStrokes(ctx, strokes.current, el.width, el.height, Math.max(2, el.width / 200))
  }, [])

  // The pad is as wide as its space and a third as high; its pixels follow (with the device's pixel
  // ratio), and the strokes, being fractions, are drawn again where they were. That is also what
  // turning the device does.
  // biome-ignore lint/correctness/useExhaustiveDependencies: redraw is stable; the pad exists while signing
  useEffect(() => {
    const el = canvas.current
    if (!el) return
    const resize = () => {
      const ratio = typeof window !== 'undefined' ? Math.min(window.devicePixelRatio || 1, 3) : 1
      const rect = el.getBoundingClientRect()
      if (rect.width > 0) {
        el.width = Math.round(rect.width * ratio)
        el.height = Math.round((rect.width / 3) * ratio)
      }
      redraw()
    }
    resize()
    const observer = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(resize) : undefined
    observer?.observe(el)
    window.addEventListener('resize', resize)
    window.addEventListener('orientationchange', resize)
    return () => {
      observer?.disconnect()
      window.removeEventListener('resize', resize)
      window.removeEventListener('orientationchange', resize)
    }
  }, [mode, value])

  const point = (e: React.PointerEvent<HTMLCanvasElement>): Point => {
    const rect = e.currentTarget.getBoundingClientRect()
    const w = rect.width || 1
    const h = rect.height || 1
    return {
      x: Math.min(1, Math.max(0, (e.clientX - rect.left) / w)),
      y: Math.min(1, Math.max(0, (e.clientY - rect.top) / h)),
    }
  }

  const down = (e: React.PointerEvent<HTMLCanvasElement>) => {
    // Only the primary button of a mouse, and one finger or pen at a time.
    if ((e.pointerType === 'mouse' && e.button !== 0) || active.current !== null || !uploads || saving) return
    active.current = e.pointerId
    e.currentTarget.setPointerCapture?.(e.pointerId)
    strokes.current = [...strokes.current, [point(e)]]
    setCount(strokes.current.length)
    setProblem(null)
    redraw()
  }
  const move = (e: React.PointerEvent<HTMLCanvasElement>) => {
    if (active.current !== e.pointerId) return
    const last = strokes.current[strokes.current.length - 1]
    if (!last) return
    const events = typeof e.nativeEvent.getCoalescedEvents === 'function' ? e.nativeEvent.getCoalescedEvents() : []
    for (const c of events.length > 0 ? events : [e.nativeEvent]) {
      const rect = e.currentTarget.getBoundingClientRect()
      last.push({
        x: Math.min(1, Math.max(0, (c.clientX - rect.left) / (rect.width || 1))),
        y: Math.min(1, Math.max(0, (c.clientY - rect.top) / (rect.height || 1))),
      })
    }
    redraw()
  }
  const up = (e: React.PointerEvent<HTMLCanvasElement>) => {
    if (active.current !== e.pointerId) return
    active.current = null
    e.currentTarget.releasePointerCapture?.(e.pointerId)
  }

  const undo = () => {
    strokes.current = strokes.current.slice(0, -1)
    setCount(strokes.current.length)
    redraw()
  }
  const clear = () => {
    strokes.current = []
    setCount(0)
    redraw()
  }

  // Uploads the picture like a photo; its id goes to the form. Offline there is no server to upload to:
  // the picture is staged on the device, as a photo is, and the queue uploads it with the record.
  const save = async (picture: HTMLCanvasElement) => {
    if (!files) return
    setSaving(true)
    setProblem(null)
    try {
      const png = await canvasToPng(picture)
      if (files.local) {
        onChange(files.local.stage(png))
      } else {
        const done = await uploadPhoto(files.base, field, png)
        mine.current.add(done.id)
        onChange(done.id)
      }
      strokes.current = []
      setCount(0)
      setName('')
      setConfirmed(false)
    } catch (e) {
      // What was drawn or typed stays, to send again.
      setProblem(t('apps.signature.failed', { error: errorText(e) }))
    } finally {
      setSaving(false)
    }
  }

  const signAgain = () => {
    if (value && files?.local) files.local.forget(value)
    else if (value && files && mine.current.delete(value)) void discardPhoto(files.base, value)
    onChange(undefined)
  }

  if (value && files) {
    return (
      <fieldset className="tt-capture tt-signature">
        <legend className="tt-capture-label">{heading}</legend>
        {files.local ? (
          // Still on this device: nothing of it is on the server to link to.
          <img className="tt-signature-image" src={files.local.url(value)} alt={t('apps.signature.alt')} />
        ) : (
          <a href={photoUrl(files.base, value)} target="_blank" rel="noopener noreferrer">
            <img className="tt-signature-image" src={thumbnailUrl(files.base, value)} alt={t('apps.signature.alt')} />
          </a>
        )}
        {!readOnly && (
          <div className="tt-capture-actions">
            <button type="button" className="tt-button" onClick={signAgain}>
              {t('apps.signature.again')}
            </button>
          </div>
        )}
      </fieldset>
    )
  }
  if (readOnly) {
    return (
      <fieldset className="tt-capture tt-signature">
        <legend className="tt-capture-label">{heading}</legend>
        <p className="tt-muted">{t('apps.signature.none')}</p>
      </fieldset>
    )
  }

  const typedReady = name.trim() !== '' && confirmed
  return (
    <fieldset className="tt-capture tt-signature" aria-describedby={`${ids}-note`}>
      <legend className="tt-capture-label">{heading}</legend>
      <fieldset className="tt-capture tt-capture-actions" aria-label={t('apps.signature.how')}>
        <button type="button" className="tt-button" aria-pressed={mode === 'draw'} onClick={() => setMode('draw')}>
          {t('apps.signature.draw')}
        </button>
        <button type="button" className="tt-button" aria-pressed={mode === 'type'} onClick={() => setMode('type')}>
          {t('apps.signature.type')}
        </button>
      </fieldset>
      {mode === 'draw' ? (
        <>
          {/* touch-action: none is on the pad and nowhere else: the page scrolls around it. */}
          <canvas
            ref={canvas}
            className="tt-signature-canvas"
            style={{ touchAction: 'none' }}
            role="img"
            aria-label={t('apps.signature.pad')}
            onPointerDown={down}
            onPointerMove={move}
            onPointerUp={up}
            onPointerCancel={up}
          />
          <div className="tt-capture-actions">
            <button type="button" className="tt-button" disabled={count === 0 || saving} onClick={undo}>
              {t('apps.signature.undo')}
            </button>
            <button type="button" className="tt-button" disabled={count === 0 || saving} onClick={clear}>
              {t('apps.signature.clear')}
            </button>
            <button
              type="button"
              className="tt-button tt-primary"
              disabled={count === 0 || saving || !uploads}
              onClick={() => void save(strokesToCanvas(strokes.current))}
            >
              {t('apps.signature.save')}
            </button>
          </div>
        </>
      ) : (
        <div className="tt-capture-manual">
          <label className="tt-capture-field">
            {t('apps.signature.name')}
            <input
              className="tt-capture-input"
              value={name}
              autoComplete="name"
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="tt-capture-field tt-capture-check">
            <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />
            {t('apps.signature.confirm')}
          </label>
          <button
            type="button"
            className="tt-button tt-primary"
            disabled={!typedReady || saving || !uploads}
            onClick={() => void save(nameToCanvas(name.trim()))}
          >
            {t('apps.signature.saveTyped')}
          </button>
        </div>
      )}
      {saving && (
        <p role="status" className="tt-muted tt-small">
          {t('apps.signature.saving')}
        </p>
      )}
      {problem && (
        <p role="alert" className="tt-error tt-small">
          {problem}
        </p>
      )}
      <p id={`${ids}-note`} className="tt-muted tt-small">
        {!uploads
          ? t(files ? 'apps.signature.previewOnly' : 'apps.signature.notHere')
          : t(files?.local ? 'apps.signature.noteOffline' : 'apps.signature.note')}
      </p>
    </fieldset>
  )
}
