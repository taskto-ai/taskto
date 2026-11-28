import { type SyntheticEvent, useEffect, useRef } from 'react'
import './preview.css'

const POLICY = "default-src 'none'; style-src 'unsafe-inline'; img-src blob: data:; font-src data:"

const SRC_DOC = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${POLICY}"><style>html,body{margin:0;background:transparent}</style></head><body data-preview-frame></body></html>`

export const FRAME_SANDBOX = 'allow-same-origin'

export function webAddress(href: string | null): string | null {
  if (!href) return null
  try {
    const url = new URL(href)
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null
  } catch {
    return null
  }
}

export interface PreviewFrameProps {
  title: string
  render: (body: HTMLElement) => Promise<void>
  onRendered: () => void
  onFailed: () => void
  hidden?: boolean
}

export function PreviewFrame({ title, render, onRendered, onFailed, hidden = false }: PreviewFrameProps) {
  const frame = useRef<HTMLIFrameElement>(null)
  const latest = useRef({ render, onRendered, onFailed })
  latest.current = { render, onRendered, onFailed }
  const started = useRef(false)
  const alive = useRef(true)
  const observer = useRef<ResizeObserver | null>(null)

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      observer.current?.disconnect()
    }
  }, [])

  const loaded = (event: SyntheticEvent<HTMLIFrameElement>) => {
    const doc = event.currentTarget.contentDocument
    const body = doc?.body
    if (!doc || !body || started.current) return
    if (!body.hasAttribute('data-preview-frame')) return
    started.current = true
    doc.addEventListener('click', (click) => {
      const anchor = (click.target as Element | null)?.closest?.('a')
      if (!anchor) return
      click.preventDefault()
      const address = webAddress(anchor.getAttribute('href'))
      if (address) window.open(address, '_blank', 'noopener,noreferrer')
    })
    const resize = () => {
      const el = frame.current
      if (el) el.style.blockSize = `${doc.documentElement.scrollHeight}px`
    }
    if (typeof ResizeObserver !== 'undefined') {
      observer.current = new ResizeObserver(resize)
      observer.current.observe(body)
    }
    latest.current.render(body).then(
      () => {
        if (!alive.current) return
        resize()
        latest.current.onRendered()
      },
      () => {
        if (alive.current) latest.current.onFailed()
      },
    )
  }

  return (
    <iframe
      ref={frame}
      className="kb-preview-frame"
      title={title}
      sandbox={FRAME_SANDBOX}
      srcDoc={SRC_DOC}
      onLoad={loaded}
      hidden={hidden}
    />
  )
}
