import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { type LinkHandler, Markdown } from '../markdown'
import { ButtonLink } from '../ui/Button'
import { isAudio, isVideo, isWord } from './docs'
import { FilePreview } from './preview/FilePreview'
import './prose.css'

export function Content({ url, doc, onLink }: { url: string; doc: Doc; onLink: LinkHandler }) {
  const t = useT()
  const f = useFormat()
  if (doc.content !== undefined) {
    return doc.content.trim() ? (
      <Markdown text={doc.content} onLink={onLink} look="document" />
    ) : (
      <p className="kb-note">{t('knowledge.doc.empty')}</p>
    )
  }
  const raw = `${url}${url.includes('?') ? '&' : '?'}raw=true`
  return (
    <div className="kb-file">
      {isVideo(doc.path) && <video className="kb-media" controls preload="metadata" src={raw} aria-label={doc.path} />}
      {isAudio(doc.path) && <audio className="kb-media" controls preload="metadata" src={raw} aria-label={doc.path} />}
      {isWord(doc.path) && <FilePreview src={raw} name={doc.path.split('/').pop() ?? doc.path} size={doc.size} />}
      <div className="kb-file-download">
        <ButtonLink plain href={raw} download icon="down">
          {t('knowledge.doc.download')}
        </ButtonLink>
        <span className="kb-note">{f.bytes(doc.size)}</span>
      </div>
      {doc.versioned === false && <p className="kb-note">{t('knowledge.notVersioned')}</p>}
    </div>
  )
}
