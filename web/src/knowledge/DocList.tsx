import { type CSSProperties, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { Doc } from '../api'
import { useFormat, useLocale, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Icon } from '../ui/Icon'
import { Skeleton } from '../ui/Skeleton'
import { Tooltip } from '../ui/Tooltip'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { useLoadingStatus } from './loading'

export type Folder = { name: string; path: string; folders: Folder[]; files: Doc[]; count: number }

export function buildTree(docs: Doc[], collator = new Intl.Collator(undefined, { numeric: true })): Folder {
  const root: Folder = { name: '', path: '', folders: [], files: [], count: 0 }
  const folders = new Map<string, Folder>([['', root]])
  for (const doc of docs) {
    const parts = doc.path.split('/')
    let at = root
    at.count++
    for (const name of parts.slice(0, -1)) {
      const path = at.path ? `${at.path}/${name}` : name
      let next = folders.get(path)
      if (!next) {
        next = { name, path, folders: [], files: [], count: 0 }
        folders.set(path, next)
        at.folders.push(next)
      }
      next.count++
      at = next
    }
    at.files.push(doc)
  }
  const name = (d: Doc) => d.path.split('/').pop() ?? d.path
  for (const f of folders.values()) {
    f.folders.sort((a, b) => collator.compare(a.name, b.name))
    f.files.sort((a, b) => collator.compare(name(a), name(b)))
  }
  return root
}

const foldersOf = (path: string) =>
  path
    .split('/')
    .slice(0, -1)
    .map((_, i, parts) => parts.slice(0, i + 1).join('/'))

const baseName = (path: string) => path.split('/').pop() ?? path

const withFoldersOf = (path: string) => (was: ReadonlySet<string>) => {
  const missing = foldersOf(path).filter((f) => !was.has(f))
  return missing.length === 0 ? was : new Set([...was, ...missing])
}

export type Reveal = { path: string }

const MAX_INDENT = 6

export function DocList({
  docs,
  loading,
  current,
  reveal = null,
  onOpen,
}: {
  docs: Doc[] | undefined
  loading: boolean
  current: string | undefined
  reveal?: Reveal | null
  onOpen: (path: string) => void
}) {
  const t = useT()
  const locale = useLocale()
  useLoadingStatus(loading)
  const tree = useMemo(
    () => (docs ? buildTree(docs, new Intl.Collator(locale, { numeric: true })) : null),
    [docs, locale],
  )
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set(current ? foldersOf(current) : []))
  useEffect(() => {
    if (current) setOpen(withFoldersOf(current))
  }, [current])
  useEffect(() => {
    if (reveal) setOpen(withFoldersOf(reveal.path))
  }, [reveal])
  const toggle = (path: string) =>
    setOpen((was) => {
      const next = new Set(was)
      if (!next.delete(path)) next.add(path)
      return next
    })

  const list = useRef<HTMLUListElement>(null)
  const [cut, setCut] = useState<ReadonlySet<string>>(new Set())
  const measure = useCallback(() => {
    const el = list.current
    if (!el) return
    const now = new Set<string>()
    for (const name of el.querySelectorAll<HTMLElement>('[data-name-of]')) {
      if (name.scrollWidth > name.clientWidth) now.add(name.dataset.nameOf ?? '')
    }
    setCut((was) => (was.size === now.size && [...now].every((k) => was.has(k)) ? was : now))
  }, [])
  useLayoutEffect(measure)
  const shown = !loading && tree !== null && tree.count > 0
  useLayoutEffect(() => {
    const el = list.current
    if (!shown || !el || typeof ResizeObserver === 'undefined') return
    const watch = new ResizeObserver(measure)
    watch.observe(el)
    return () => watch.disconnect()
  }, [shown, measure])

  if (loading) {
    return (
      <div className="kb-tree-loading">
        {['60%', '45%', '75%', '55%', '40%', '65%'].map((width) => (
          <Skeleton key={width} width={width} />
        ))}
      </div>
    )
  }
  if (!tree || tree.count === 0) return null
  return (
    <ul className="kb-tree" aria-label={t('knowledge.documents')} ref={list}>
      <Level folder={tree} depth={0} open={open} toggle={toggle} current={current} onOpen={onOpen} cut={cut} />
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
  cut,
}: {
  folder: Folder
  depth: number
  open: ReadonlySet<string>
  toggle: (path: string) => void
  current: string | undefined
  onOpen: (path: string) => void
  cut: ReadonlySet<string>
}) {
  const t = useT()
  const f = useFormat()
  const indent = { '--kb-depth': Math.min(depth, MAX_INDENT) } as CSSProperties
  return (
    <>
      {folder.folders.map((sub) => {
        const isOpen = open.has(sub.path)
        const key = `folder:${sub.path}`
        return (
          <li key={key}>
            <Tooltip content={cut.has(key) ? sub.path : null} describes={false}>
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
                <span className="kb-tree-name" data-name-of={key}>
                  {sub.name}
                </span>
                {!isOpen && (
                  <span className="kb-tree-count">
                    <span aria-hidden="true">{f.number(sub.count)}</span>
                    <VisuallyHidden>{t('knowledge.tree.count', { count: sub.count })}</VisuallyHidden>
                  </span>
                )}
              </Button>
            </Tooltip>
            {isOpen && (
              <ul>
                <Level
                  folder={sub}
                  depth={depth + 1}
                  open={open}
                  toggle={toggle}
                  current={current}
                  onOpen={onOpen}
                  cut={cut}
                />
              </ul>
            )}
          </li>
        )
      })}
      {folder.files.map((d) => {
        const key = `doc:${d.path}`
        return (
          <li key={key}>
            <Tooltip content={cut.has(key) ? d.path : null} describes={false}>
              <Button
                variant="ghost"
                size="sm"
                className="kb-tree-item"
                style={indent}
                data-path={d.path}
                aria-current={d.path === current || undefined}
                onClick={() => onOpen(d.path)}
              >
                <span className="kb-tree-spacer" />
                <Icon name="file" size={16} />
                <span className="kb-tree-name" data-name-of={key}>
                  {baseName(d.path)}
                </span>
                <span className="kb-tree-size">
                  {d.versioned === false ? t('knowledge.sizeUnversioned', { size: f.bytes(d.size) }) : f.bytes(d.size)}
                </span>
              </Button>
            </Tooltip>
          </li>
        )
      })}
    </>
  )
}
