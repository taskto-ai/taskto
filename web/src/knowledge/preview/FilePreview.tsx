import { useEffect, useState } from 'react'
import { apiBytes } from '../../api'
import { useFormat, useT } from '../../i18n'
import { Notice } from '../../ui/Notice'
import { Skeleton } from '../../ui/Skeleton'
import { renderDocx } from './docx'
import { PreviewFrame } from './PreviewFrame'
import './preview.css'

export const MAX_PREVIEW_BYTES = 20 * 1000 * 1000

type State = { phase: 'loading' } | { phase: 'tooLarge' } | { phase: 'failed' } | { phase: 'ready'; bytes: ArrayBuffer }

export interface FilePreviewProps {
  src: string
  name: string
  size: number
}

export function FilePreview({ src, name, size }: FilePreviewProps) {
  const t = useT()
  const f = useFormat()
  const tooLarge = size > MAX_PREVIEW_BYTES
  const [state, setState] = useState<State>({ phase: 'loading' })
  const [drawn, setDrawn] = useState(false)

  useEffect(() => {
    setDrawn(false)
    if (tooLarge) {
      setState({ phase: 'tooLarge' })
      return
    }
    setState({ phase: 'loading' })
    const controller = new AbortController()
    apiBytes(src, { signal: controller.signal })
      .then((bytes) => {
        if (controller.signal.aborted) return
        setState(bytes.byteLength > MAX_PREVIEW_BYTES ? { phase: 'tooLarge' } : { phase: 'ready', bytes })
      })
      .catch(() => {
        if (!controller.signal.aborted) setState({ phase: 'failed' })
      })
    return () => controller.abort()
  }, [src, tooLarge])

  if (state.phase === 'tooLarge') {
    return <Notice tone="neutral">{t('knowledge.preview.tooLarge', { size: f.bytes(MAX_PREVIEW_BYTES) })}</Notice>
  }
  if (state.phase === 'failed') return <Notice tone="warning">{t('knowledge.preview.failed')}</Notice>
  const { bytes } = state.phase === 'ready' ? state : { bytes: null }
  return (
    <div className="kb-preview" aria-busy={!drawn}>
      {!drawn && (
        <div className="kb-preview-loading" role="status" aria-label={t('knowledge.preview.loading')}>
          <Skeleton width="60%" />
          <Skeleton shape="block" height="12rem" />
        </div>
      )}
      {bytes && (
        <PreviewFrame
          key={src}
          title={t('knowledge.preview.label', { name })}
          render={(body) => renderDocx(bytes, body)}
          onRendered={() => setDrawn(true)}
          onFailed={() => setState({ phase: 'failed' })}
          hidden={!drawn}
        />
      )}
    </div>
  )
}
