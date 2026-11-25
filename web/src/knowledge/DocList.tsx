// The space's documents as a tree of folders, built from their paths: a folder opens and closes,
// and a closed one says how many documents it holds; the open document is marked, and the folders
// it is in open themselves. A long name is cut, with the whole path in a tooltip.

import { useEffect, useMemo, useState } from 'react'
import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Icon } from '../ui/Icon'
import { Skeleton } from '../ui/Skeleton'
import { Tooltip } from '../ui/Tooltip'
import { VisuallyHidden } from '../ui/VisuallyHidden'

type Folder = { name: string; path: string; folders: Folder[]; files: Doc[]; count: number }

// Folders first, then documents, each by name in the person's language.
export function buildTree(docs: Doc[], collator = new Intl.Collator()): Folder {
  const root: Folder = { name: '', path: '', folders: [], files: [], count: 0 }
  for (const doc of docs) {
    const parts = doc.path.split('/')
    let at = root
    at.count++
    for (const name of parts.slice(0, -1)) {
      const path = at.path ? `${at.path}/${name}` : name
      let next = at.folders.find((f) => f.name === name)
      if (!next) {
        next = { name, path, folders: [], files: [], count: 0 }
        at.folders.push(next)
      }
      next.count++
      at = next
    }
    at.files.push(doc)
  }
  const sort = (f: Folder) => {
    f.folders.sort((a, b) => collator.compare(a.name, b.name))
    f.files.sort((a, b) => collator.compare(a.path, b.path))
    f.folders.forEach(sort)
  }
  sort(root)
  return root
}

// The folders a path is in: "a/b/c.md" is in "a" and "a/b".
const foldersOf = (path: string) =>
  path
    .split('/')
    .slice(0, -1)
    .map((_, i, parts) => parts.slice(0, i + 1).join('/'))

const baseName = (path: string) => path.split('/').pop() ?? path

// A name longer than this may be cut in the column, so it gets its whole path in a tooltip.
const LONG = 24

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
  const tree = useMemo(() => (docs ? buildTree(docs) : null), [docs])
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set(current ? foldersOf(current) : []))
  // A document opened from elsewhere (a link, a search) shows where it is.
  useEffect(() => {
    if (!current) return
    setOpen((was) => {
      const missing = foldersOf(current).filter((f) => !was.has(f))
      return missing.length === 0 ? was : new Set([...was, ...missing])
    })
  }, [current])
  const toggle = (path: string) =>
    setOpen((was) => {
      const next = new Set(was)
      if (!next.delete(path)) next.add(path)
      return next
    })

  if (loading) {
    return (
      <div className="kb-tree-loading" aria-busy="true">
        <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
        {['60%', '45%', '75%', '55%', '40%', '65%'].map((width, i) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity but their place
          <Skeleton key={i} width={width} />
        ))}
      </div>
    )
  }
  if (!tree || tree.count === 0) return null
  return (
    <ul className="kb-tree" aria-label={t('knowledge.documents')}>
      <Level folder={tree} depth={0} open={open} toggle={toggle} current={current} onOpen={onOpen} />
    </ul>
  )
}

function Level({
  folder,
  depth,
  open,
  toggle,
  current,
  onOpen,
}: {
  folder: Folder
  depth: number
  open: ReadonlySet<string>
  toggle: (path: string) => void
  current: string | undefined
  onOpen: (path: string) => void
}) {
  const t = useT()
  const f = useFormat()
  const indent = { '--kb-depth': depth } as React.CSSProperties
  return (
    <>
      {folder.folders.map((sub) => {
        const isOpen = open.has(sub.path)
        const button = (
          <Button
            variant="ghost"
            size="sm"
            className="kb-tree-item"
            style={indent}
            aria-expanded={isOpen}
            onClick={() => toggle(sub.path)}
          >
            <Icon name={isOpen ? 'chevron-down' : 'chevron-end'} size={16} />
            <Icon name="folder" size={16} />
            <span className="kb-tree-name">{sub.name}</span>
            {!isOpen && (
              <span className="kb-tree-count">
                <span aria-hidden="true">{f.number(sub.count)}</span>
                <VisuallyHidden>{t('knowledge.tree.count', { count: sub.count })}</VisuallyHidden>
              </span>
            )}
          </Button>
        )
        return (
          <li key={sub.path}>
            {sub.name.length > LONG ? (
              <Tooltip content={sub.path} describes={false}>
                {button}
              </Tooltip>
            ) : (
              button
            )}
            {isOpen && (
              <ul>
                <Level folder={sub} depth={depth + 1} open={open} toggle={toggle} current={current} onOpen={onOpen} />
              </ul>
            )}
          </li>
        )
      })}
      {folder.files.map((d) => {
        const name = baseName(d.path)
        const button = (
          <Button
            variant="ghost"
            size="sm"
            className="kb-tree-item"
            style={indent}
            data-path={d.path}
            aria-current={d.path === current || undefined}
            onClick={() => onOpen(d.path)}
          >
            {/* The chevron's room, so names line up with their folders'. */}
            <span className="kb-tree-spacer" />
            <Icon name="file" size={16} />
            <span className="kb-tree-name">{name}</span>
            <span className="kb-tree-size">
              {d.versioned === false ? t('knowledge.sizeUnversioned', { size: f.bytes(d.size) }) : f.bytes(d.size)}
            </span>
          </Button>
        )
        return (
          <li key={d.path}>
            {name.length > LONG ? (
              <Tooltip content={d.path} describes={false}>
                {button}
              </Tooltip>
            ) : (
              button
            )}
          </li>
        )
      })}
    </>
  )
}
