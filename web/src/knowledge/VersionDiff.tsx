import { Fragment, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { api, type Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { diffLines, diffWords, type Line, type LineDiff, type WordPart } from './diff'
import { authorName, type Commit } from './docs'
import { useLoadingStatus } from './loading'

export type DiffAgainst = { kind: 'previous'; commit: Commit | null } | { kind: 'current' }

type Shown = Extract<LineDiff, { tooLarge: false }>

const useText = (url: string | null) => useLoad(() => (url ? api<Doc>(url) : Promise.resolve(null)), [url])

function wordParts(lines: Line[]): Map<Line, WordPart[]> {
  const parts = new Map<Line, WordPart[]>()
  let i = 0
  while (i < lines.length) {
    if (lines[i].kind !== 'del') {
      i++
      continue
    }
    const delStart = i
    while (i < lines.length && lines[i].kind === 'del') i++
    const addStart = i
    while (i < lines.length && lines[i].kind === 'add') i++
    const pairs = Math.min(addStart - delStart, i - addStart)
    for (let p = 0; p < pairs; p++) {
      const del = lines[delStart + p]
      const add = lines[addStart + p]
      const words = diffWords(del.text, add.text)
      if (!words) continue
      parts.set(del, words.old)
      parts.set(add, words.new)
    }
  }
  return parts
}

export function VersionDiff({
  url,
  commit,
  against,
  onRestore,
  restoring,
}: {
  url: string
  commit: Commit
  against: DiffAgainst
  onRestore?: () => void
  restoring: boolean
}) {
  const t = useT()
  const f = useFormat()
  const at = (c: Commit) => `${url}?at=${c.commit}`
  const oldUrl = against.kind === 'current' ? at(commit) : against.commit ? at(against.commit) : null
  const newUrl = against.kind === 'current' ? url : at(commit)
  const before = useText(oldUrl)
  const after = useText(newUrl)
  const loading = (!before.data && before.data !== null && !before.error) || (!after.data && !after.error)
  const failed = before.error ?? after.error
  const oldText = before.data === null ? '' : before.data?.content
  const newText = after.data?.content
  const unreadable = !loading && !failed && (oldText === undefined || newText === undefined)
  useLoadingStatus(loading)

  const diff = useMemo(
    () => (oldText === undefined || newText === undefined ? null : diffLines(oldText, newText)),
    [oldText, newText],
  )
  const section = useRef<HTMLElement>(null)
  useLayoutEffect(() => {
    section.current?.querySelector<HTMLElement>('h3')?.focus()
  }, [])

  const time = f.dateTime(commit.time)
  const title =
    against.kind === 'current'
      ? t('knowledge.history.diffWithCurrent', { time })
      : t('knowledge.history.diffTitle', { time, name: authorName(commit.author, t) })

  return (
    <section className="kb-version kb-diff" aria-label={title} ref={section}>
      <Heading level={3} size="subsection" tabIndex={-1}>
        {title}
      </Heading>
      {failed && <Notice tone="danger">{failed}</Notice>}
      {unreadable && <Notice tone="danger">{t('knowledge.errors.document')}</Notice>}
      {loading && !failed && (
        <div className="kb-history-loading">
          <Skeleton width="80%" />
          <Skeleton width="60%" />
        </div>
      )}
      {diff?.tooLarge && <Notice tone="neutral">{t('knowledge.history.tooLarge')}</Notice>}
      {diff && !diff.tooLarge && <DiffBody diff={diff} />}
      {onRestore && !loading && (
        <div className="kb-actions">
          <Button size="sm" disabled={restoring} onClick={onRestore}>
            {t('knowledge.history.restore')}
          </Button>
        </div>
      )}
    </section>
  )
}

function DiffBody({ diff }: { diff: Shown }) {
  const t = useT()
  const [open, setOpen] = useState<ReadonlySet<number>>(new Set())
  const words = useMemo(() => wordParts(diff.lines), [diff])
  if (diff.hunks.length === 0) return <p className="kb-note">{t('knowledge.history.noChanges')}</p>

  const gapAfter = (i: number) => {
    const from = i === 0 ? 0 : diff.hunks[i - 1].end
    const to = i === diff.hunks.length ? diff.lines.length : diff.hunks[i].start
    return diff.lines.slice(from, to)
  }
  const reveal = (i: number) => setOpen((o) => new Set(o).add(i))

  const rows = (lines: Line[]) =>
    lines.map((line) => <DiffRow key={`${line.aLine ?? ''}:${line.bLine ?? ''}`} line={line} parts={words.get(line)} />)

  return (
    <>
      <p className="kb-diff-counts">
        <span className="kb-diff-count-add">{t('knowledge.history.added', { count: diff.added })}</span>
        <span className="kb-diff-count-del">{t('knowledge.history.removed', { count: diff.removed })}</span>
      </p>
      <div className="kb-diff-scroll">
        <table className="kb-diff-table">
          <tbody>
            {Array.from({ length: diff.hunks.length + 1 }, (_, i) => {
              const gap = gapAfter(i)
              const hunk = diff.hunks[i]
              return (
                <Fragment key={hunk ? hunk.start : 'end'}>
                  {gap.length > 0 &&
                    (open.has(i) ? (
                      rows(gap)
                    ) : (
                      <tr className="kb-diff-fold">
                        <td colSpan={3}>
                          <Button size="sm" variant="ghost" onClick={() => reveal(i)}>
                            {t('knowledge.history.unchanged', { count: gap.length })}
                          </Button>
                        </td>
                      </tr>
                    ))}
                  {hunk && rows(hunk.lines)}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
    </>
  )
}

function DiffRow({ line, parts }: { line: Line; parts: WordPart[] | undefined }) {
  const t = useT()
  return (
    <tr className={`kb-diff-row kb-diff-${line.kind}`}>
      <td className="kb-diff-num" aria-hidden="true">
        {line.aLine}
      </td>
      <td className="kb-diff-num" aria-hidden="true">
        {line.bLine}
      </td>
      <td className="kb-diff-code">
        {line.kind !== 'same' && (
          <VisuallyHidden>
            {t(line.kind === 'add' ? 'knowledge.history.addedLine' : 'knowledge.history.removedLine')}
          </VisuallyHidden>
        )}
        <LineText line={line} parts={parts} />
        {line.noEol && <span className="kb-diff-eol">{t('knowledge.history.noNewline')}</span>}
      </td>
    </tr>
  )
}

function LineText({ line, parts }: { line: Line; parts: WordPart[] | undefined }) {
  if (line.kind === 'same') return <>{line.text}</>
  const Mark = line.kind === 'add' ? 'ins' : 'del'
  if (parts === undefined) return <Mark>{line.text}</Mark>
  let offset = 0
  return (
    <>
      {parts.map((part) => {
        const key = offset
        offset += part.text.length
        return part.changed ? <Mark key={key}>{part.text}</Mark> : <Fragment key={key}>{part.text}</Fragment>
      })}
    </>
  )
}
