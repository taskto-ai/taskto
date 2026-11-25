import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { type LinkHandler, Markdown } from '../markdown'
import { ButtonLink } from '../ui/Button'
import { isAudio, isVideo } from './docs'
import './prose.css'

// A document's text as it is read, or a file's download link, with a player for audio and
// video.
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
      {isVideo(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <video className="kb-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
      {isAudio(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <audio className="kb-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
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
