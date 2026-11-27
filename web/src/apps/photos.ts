import { ApiError, api } from '../api'
import { sameSession } from '../session'

export type Attachment = {
  id: string
  field: string
  status: 'uploading' | 'ready'
  size: number
  received: number
  content_type?: string
  width?: number
  height?: number
  attached: boolean
  chunk_size?: number
  max_chunk?: number
}

export type PhotoLimits = { max_files: number; max_bytes: number }

export const DEFAULT_LIMITS: PhotoLimits = { max_files: 1, max_bytes: 8 << 20 }

export const PHOTO_TYPES = ['image/jpeg', 'image/png', 'image/gif']

export const ATTACHMENT_ID = /^[0-9a-f]{32}$/

export function photoIds(value: unknown): string[] {
  if (typeof value === 'string') return ATTACHMENT_ID.test(value) ? [value] : []
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string' && ATTACHMENT_ID.test(v)) : []
}

export function signatureId(value: unknown): string | undefined {
  const id = Array.isArray(value) ? value[0] : value
  return typeof id === 'string' && ATTACHMENT_ID.test(id) ? id : undefined
}

export function thumbnailUrl(base: string, id: string): string {
  return `${base}/attachments/${id}/thumbnail`
}

export function photoUrl(base: string, id: string): string {
  return `${base}/attachments/${id}`
}

export type UploadOptions = {
  resume?: string
  onStarted?: (id: string) => void
  onProgress?: (sent: number, total: number) => void
  cancelled?: () => boolean
  retries?: number
  backoff?: number
  headers?: Record<string, string>
  signal?: AbortSignal
}

export class UploadCancelled extends Error {}

const OFFSET = 'app_upload_offset'

const STORAGE_FULL = new Set(['quota_workspace', 'quota_user', 'knowledge_full'])
export const storageFull = (e: unknown) => e instanceof ApiError && !!e.code && STORAGE_FULL.has(e.code)

const transient = (e: unknown) => !(e instanceof ApiError) || (e.status >= 500 && !storageFull(e))

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

export async function uploadPhoto(
  base: string,
  field: string,
  file: Blob,
  opts: UploadOptions = {},
): Promise<Attachment> {
  const retries = opts.retries ?? 3
  const upload = (id: string, rest = '') => `${base}/uploads/${id}${rest}`
  const same = sameSession()
  const online = opts.headers === undefined
  const headers = opts.headers
  const signal = opts.signal
  let state: Attachment | undefined
  if (opts.resume) {
    try {
      state = await api<Attachment>(upload(opts.resume), { headers, signal })
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) throw e
    }
  }
  if (online && !same()) throw new UploadCancelled()
  if (!state)
    state = await api<Attachment>(`${base}/uploads`, {
      method: 'POST',
      body: { field, size: file.size },
      headers,
      signal,
    })
  const id = state.id
  opts.onStarted?.(id)
  if (state.status === 'ready') return state
  const chunk = Math.min(state.chunk_size ?? 1 << 20, state.max_chunk ?? 1 << 20)
  let offset = state.received
  let failures = 0
  for (let trips = 0, max = Math.ceil(file.size / chunk) * 3 + 20; trips < max; trips++) {
    if (opts.cancelled?.() || signal?.aborted || (online && !same())) throw new UploadCancelled()
    opts.onProgress?.(offset, file.size)
    try {
      if (offset >= file.size)
        return await api<Attachment>(upload(id, '/finalize'), { method: 'POST', body: {}, headers, signal })
      const next = await api<Attachment>(`${upload(id)}?offset=${offset}`, {
        method: 'PUT',
        bytes: file.slice(offset, offset + chunk),
        headers,
        signal,
      })
      offset = next.received
      failures = 0
    } catch (e) {
      if (signal?.aborted) throw new UploadCancelled()
      if (e instanceof ApiError && e.code === OFFSET && typeof e.params?.offset === 'number') {
        offset = e.params.offset
        continue
      }
      if (!transient(e) || ++failures > retries) throw e
      await sleep((opts.backoff ?? 500) * failures)
      if (online && !same()) throw new UploadCancelled()
      try {
        offset = (await api<Attachment>(upload(id), { headers, signal })).received
      } catch {}
    }
  }
  throw new Error('the upload did not finish')
}

export async function discardPhoto(base: string, id: string): Promise<void> {
  try {
    await api(`${base}/uploads/${id}`, { method: 'DELETE' })
  } catch {}
}
