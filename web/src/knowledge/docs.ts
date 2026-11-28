import { useRef } from 'react'
import { ApiError, api, type Doc } from '../api'
import { has, type T } from '../i18n'
import { useLoad } from '../load'

export type Change = { commit: string; doc?: Doc }

const DELETED_AUTHOR = 'deleted-user@deleted.invalid'
export const authorName = (a: { name: string; email: string }, t: T) =>
  a.email === DELETED_AUTHOR ? t('common.deletedUser') : a.name

export type Commit = {
  commit: string
  author: { name: string; email: string }
  agent?: string
  time: string
  message: string
  change?: 'create' | 'update' | 'delete'
  version?: string
  versioned?: boolean
}

export const encodePath = (path: string) => path.split('/').map(encodeURIComponent).join('/')

export const isConflict = (e: unknown) => e instanceof ApiError && e.status === 409
export const isStale = (e: unknown) => e instanceof ApiError && (e.status === 409 || e.status === 404)
export const isNamedConflict = (e: unknown) =>
  e instanceof ApiError && e.status === 409 && !!e.code && e.code !== 'doc_conflict' && has(`errors.${e.code}`)
export const isMarkdown = (path: string) => /\.md$/i.test(path)
export const isWord = (path: string) => /\.docx$/i.test(path)
export const isVideo = (path: string) => /\.(mp4|m4v|mov|webm|ogv)$/i.test(path)
export const isAudio = (path: string) => /\.(mp3|m4a|aac|wav|flac|ogg|oga|opus|weba)$/i.test(path)

async function listDocs(base: string): Promise<Doc[]> {
  const docs: Doc[] = []
  let after = ''
  do {
    const page = await api<{ docs: Doc[]; next?: string }>(
      `${base}/docs?limit=1000${after ? `&after=${encodeURIComponent(after)}` : ''}`,
    )
    docs.push(...page.docs)
    after = page.next ?? ''
  } while (after)
  return docs
}

export function resolveDocLink(from: string, href: string): string | null {
  if (/^([a-z][a-z0-9+.-]*:|\/|#|\?)/i.test(href)) return null
  let target: string
  try {
    target = decodeURIComponent(href.replace(/[?#].*$/, ''))
  } catch {
    return null
  }
  const parts = from.split('/').slice(0, -1)
  for (const part of target.split('/')) {
    if (part === '..') {
      if (parts.length === 0) return null
      parts.pop()
    } else if (part !== '.' && part !== '') parts.push(part)
  }
  return parts.length > 0 ? parts.join('/') : null
}

export function useLatest<T>(value: T) {
  const ref = useRef(value)
  ref.current = value
  return ref
}

export { useMounted } from '../mounted'

export const useDocs = (base: string | null) =>
  useLoad(() => (base ? listDocs(base) : Promise.resolve(undefined)), [base])

export const useDoc = (url: string) => useLoad(() => api<Doc>(url), [url])
