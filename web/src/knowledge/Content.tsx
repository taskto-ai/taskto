import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { type LinkHandler, Markdown } from '../markdown'
import { isAudio, isVideo } from './docs'

// A document's text as a preview, or a file's download link, with a player for audio and
// video.
export function Content({ url, doc, onLink }: { url: string; doc: Doc; onLink: LinkHandler }) {
  const t = useT()
  const f = useFormat()
  if (doc.content !== undefined) {
    return doc.content.trim() ? (
      <Markdown text={doc.content} onLink={onLink} />
    ) : (
      <p className="tt-muted">{t('knowledge.doc.empty')}</p>
    )
  }
  const raw = `${url}${url.includes('?') ? '&' : '?'}raw=true`
  return (
    <>
      {isVideo(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <video className="tt-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
      {isAudio(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <audio className="tt-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
      <p>
        <a href={raw} download>
          {t('knowledge.doc.download')}
        </a>{' '}
        <span className="tt-muted tt-small">{f.bytes(doc.size)}</span>
      </p>
      {doc.versioned === false && <p className="tt-muted tt-small">{t('knowledge.notVersioned')}</p>}
    </>
  )
}
