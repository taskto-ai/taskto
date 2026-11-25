// The knowledge base's data and the rules every part shares: the API's shapes, a document's
// path in a URL, what a refusal means, what kind of file a path names, and the loading of the
// list and of one document.

import { useRef } from 'react'
import { ApiError, api, type Doc } from '../api'
import { has } from '../i18n'
import { useLoad } from '../load'

export type Change = { commit: string; doc?: Doc }

export type Commit = {
  commit: string
  author: { name: string; email: string }
  agent?: string
  time: string
  message: string
  change?: 'create' | 'update' | 'delete'
  version?: string
  // false for a version whose bytes aren't kept: an unversioned file's, or one removed
  // from history.
  versioned?: boolean
}

export const encodePath = (path: string) => path.split('/').map(encodeURIComponent).join('/')

export const isConflict = (e: unknown) => e instanceof ApiError && e.status === 409
// A conflict the server named (doc_changed, doc_exists…): its message says what happened.
export const isNamedConflict = (e: unknown) =>
  e instanceof ApiError && e.status === 409 && !!e.code && e.code !== 'doc_conflict' && has(`errors.${e.code}`)
export const isMarkdown = (path: string) => /\.md$/i.test(path)
// Audio and video the browser can play, by extension (the server sends their media type).
export const isVideo = (path: string) => /\.(mp4|m4v|mov|webm|ogv)$/i.test(path)
export const isAudio = (path: string) => /\.(mp3|m4a|aac|wav|flac|ogg|oga|opus|weba)$/i.test(path)

// Every page of the list: a space has at most a few thousand documents.
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

// A link in a document to another one (relative to its folder) opens it here; null for a
// link that isn't one, or leaves the space.
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

// What is shown now, which a callback made earlier can read: an answer is applied only if what
// it was asked for (the space, the document, the list's generation) is still what is shown.
//
//   const now = useLatest(base)
//   ...
//   const made = base
//   api(...).then((res) => now.current === made && apply(res))
export function useLatest<T>(value: T) {
  const ref = useRef(value)
  ref.current = value
  return ref
}

// The space's documents, every page of them; nothing while there is no space.
export const useDocs = (base: string | null) =>
  useLoad(() => (base ? listDocs(base) : Promise.resolve(undefined)), [base])

// One document (or a version of it, with `?at=` in the URL), with its content.
export const useDoc = (url: string) => useLoad(() => api<Doc>(url), [url])
