// Uploading a photo to an app (docs/api.md, "Photo attachments"): create an upload for a field,
// send the file in chunks that each say the offset they start at, finalize. The server answers each
// chunk with how much it has, so an upload that was interrupted (a dropped connection, a chunk
// whose answer was lost) goes on from the server's offset and never sends a byte twice; a failed
// upload can be resumed by giving its id back.

import { ApiError, api } from '../api'

export type Attachment = {
  id: string
  field: string
  status: 'uploading' | 'ready'
  size: number
  // How much the server has: the offset the next chunk starts at.
  received: number
  content_type?: string
  width?: number
  height?: number
  attached: boolean
  chunk_size?: number
  max_chunk?: number
}

// The limits of an attachment field, which the server puts in the screen's data model.
export type PhotoLimits = { max_files: number; max_bytes: number }

export const DEFAULT_LIMITS: PhotoLimits = { max_files: 1, max_bytes: 8 << 20 }

// The pictures the server accepts. The server finds the type in the bytes; this only saves a trip.
export const PHOTO_TYPES = ['image/jpeg', 'image/png', 'image/gif']

export const ATTACHMENT_ID = /^[0-9a-f]{32}$/

// A field's value as the screen holds it: a list of attachment ids.
export function photoIds(value: unknown): string[] {
  // A signature field holds one id, as text.
  if (typeof value === 'string') return ATTACHMENT_ID.test(value) ? [value] : []
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string' && ATTACHMENT_ID.test(v)) : []
}

// A signature field's value as the screen holds it: one attachment id.
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
  // The id of an upload that was started and didn't finish: it goes on from where the server is.
  resume?: string
  // Called once the upload has an id, so a failure can be resumed.
  onStarted?: (id: string) => void
  onProgress?: (sent: number, total: number) => void
  // Checked between chunks: true stops the upload.
  cancelled?: () => boolean
  // How many times in a row a chunk may fail for the network's or the server's sake before the
  // upload gives up; the client can resume it later.
  retries?: number
  // The wait before a retry, in milliseconds.
  backoff?: number
}

export class UploadCancelled extends Error {}

const OFFSET = 'app_upload_offset'

// An error worth trying again: no answer at all, or the server's own trouble.
const transient = (e: unknown) => !(e instanceof ApiError) || e.status >= 500

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

// Uploads file for field of the app at base (…/spaces/{space}/apps/{app}) and returns the
// finished attachment.
export async function uploadPhoto(
  base: string,
  field: string,
  file: Blob,
  opts: UploadOptions = {},
): Promise<Attachment> {
  const retries = opts.retries ?? 3
  const upload = (id: string, rest = '') => `${base}/uploads/${id}${rest}`
  let state: Attachment | undefined
  if (opts.resume) {
    try {
      state = await api<Attachment>(upload(opts.resume))
    } catch (e) {
      // Gone (it expired): start again.
      if (!(e instanceof ApiError && e.status === 404)) throw e
    }
  }
  if (!state) state = await api<Attachment>(`${base}/uploads`, { method: 'POST', body: { field, size: file.size } })
  const id = state.id
  opts.onStarted?.(id)
  if (state.status === 'ready') return state
  const chunk = Math.min(state.chunk_size ?? 1 << 20, state.max_chunk ?? 1 << 20)
  let offset = state.received
  let failures = 0
  // A bound on the trips, so a server that never agrees can't keep the upload going for ever.
  for (let trips = 0, max = Math.ceil(file.size / chunk) * 3 + 20; trips < max; trips++) {
    if (opts.cancelled?.()) throw new UploadCancelled()
    opts.onProgress?.(offset, file.size)
    try {
      if (offset >= file.size) return await api<Attachment>(upload(id, '/finalize'), { method: 'POST', body: {} })
      const next = await api<Attachment>(`${upload(id)}?offset=${offset}`, {
        method: 'PUT',
        bytes: file.slice(offset, offset + chunk),
      })
      offset = next.received
      failures = 0
    } catch (e) {
      if (e instanceof ApiError && e.code === OFFSET && typeof e.params?.offset === 'number') {
        // The server is somewhere else than we thought (a chunk we never heard was applied): go on
        // from where it is.
        offset = e.params.offset
        continue
      }
      if (!transient(e) || ++failures > retries) throw e
      await sleep((opts.backoff ?? 500) * failures)
      try {
        offset = (await api<Attachment>(upload(id))).received
      } catch {
        // Still not reachable: the next try finds out.
      }
    }
  }
  throw new Error('the upload did not finish')
}

// Drops an upload, or a photo that is in no record yet, on the server. Best effort: what is
// left expires by itself.
export async function discardPhoto(base: string, id: string): Promise<void> {
  try {
    await api(`${base}/uploads/${id}`, { method: 'DELETE' })
  } catch {
    // Expires on its own after a day.
  }
}
