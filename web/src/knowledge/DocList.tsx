// The space's documents, each with its size; the open one is marked.

import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'

// loading: the list is still on its way (or the space is); docs is undefined until it comes.
export function DocList({
  docs,
  loading,
  current,
  onOpen,
}: {
  docs: Doc[] | undefined
  loading: boolean
  current: string | undefined
  onOpen: (path: string) => void
}) {
  const t = useT()
  const f = useFormat()
  return (
    <>
      {loading && <p className="tt-muted">{t('app.loading')}</p>}
      {docs?.length === 0 && <p className="tt-muted">{t('knowledge.empty')}</p>}
      {docs && docs.length > 0 && (
        <ul className="tt-list" aria-label={t('knowledge.documents')}>
          {docs.map((d) => (
            <li key={d.path}>
              <button type="button" aria-current={d.path === current || undefined} onClick={() => onOpen(d.path)}>
                <span className="tt-list-title">{d.path}</span>
                <span className="tt-muted tt-small">
                  {d.versioned === false ? t('knowledge.sizeUnversioned', { size: f.bytes(d.size) }) : f.bytes(d.size)}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
