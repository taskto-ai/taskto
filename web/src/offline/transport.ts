// The transport of an app used offline: the screen is the downloaded release's, and the buttons
// that make a record put it in the outbox instead of sending it. It is the same interface as the
// server's (AppView's AppTransport), so the renderer doesn't know the difference.
//
// A record is "saved on this device" only when the transaction that holds it, and its photos, has
// completed (OfflineStore.enqueue): `act` resolves, and the screen is told the record is there,
// after that and not before. A write that failed (the storage is full, it is not available)
// rejects, the person is told so in words, and the record is not shown as saved: it stays in the
// form, where it was typed.

import { ApiError } from '../api'
import type { ActionPayload, AppTransport } from '../apps/AppView'
import type { LocalPhotos } from '../apps/PhotoInput'
import { ATTACHMENT_ID } from '../apps/photos'
import { t as translate } from '../i18n'
import { answerInsert, answerNavigate, attachmentFields, parseInsert, viewOf } from './local'
import type { Runtime } from './runtime'
import { type BlobRow, type Download, NotSaved, type OutboxItem } from './store'

const hex = (n: number) => {
  const bytes = new Uint8Array(n)
  globalThis.crypto.getRandomValues(bytes)
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')
}

const uuid = () => globalThis.crypto?.randomUUID?.() ?? hex(16)

// 8 to 64 letters, digits, - and _ (the server's operation key); made once for an item.
const newOperation = () => `off-${uuid()}`

// Photos picked in the form and not yet in a queued record: held in memory, with something to
// show, until the record is queued (their blobs are then written with it) or they are removed.
export function localPhotos(): LocalPhotos & {
  take(id: string): Blob | undefined
  adopt(id: string, blob: Blob): void
  release(): void
} {
  const blobs = new Map<string, Blob>()
  const urls = new Map<string, string>()
  const show = (id: string) => {
    const blob = blobs.get(id)
    if (!blob) return undefined
    let url = urls.get(id)
    if (!url) {
      url = typeof URL.createObjectURL === 'function' ? URL.createObjectURL(blob) : ''
      urls.set(id, url)
    }
    return url || undefined
  }
  const drop = (id: string) => {
    const url = urls.get(id)
    if (url && typeof URL.revokeObjectURL === 'function') URL.revokeObjectURL(url)
    urls.delete(id)
    blobs.delete(id)
  }
  return {
    stage(file) {
      const id = hex(16)
      blobs.set(id, file)
      return id
    },
    url: show,
    forget: drop,
    take: (id) => blobs.get(id),
    adopt: (id, blob) => void blobs.set(id, blob),
    release: () => {
      for (const id of [...blobs.keys()]) drop(id)
    },
  }
}

export type OfflineTransport = AppTransport & {
  photos: ReturnType<typeof localPhotos>
}

// The words for a failed write, in the person's language.
export function notSavedText(e: NotSaved): string {
  if (e.reason === 'quota') return translate('offline.notSaved.quota')
  if (e.reason === 'unavailable') return translate('offline.notSaved.unavailable')
  return translate('offline.notSaved.failed')
}

export function offlineTransport(
  runtime: Runtime,
  download: Download,
  opts: { onSaved?: (item: OutboxItem) => void } = {},
): OfflineTransport {
  const base = `/api/workspaces/${download.workspace}/spaces/${download.space}/apps/${download.app}`
  const photos = localPhotos()
  const queue = () => runtime.store.listItems(download)
  return {
    photos,
    files: { base, uploads: true, local: photos },
    load: async () => {
      // The photos of records still queued are on this device: shown from there.
      const items = await queue()
      for (const item of items) {
        for (const a of item.attachments) {
          const blob = await runtime.store.getBlob(a.blobId)
          if (blob) photos.adopt(a.localId, blob)
        }
      }
      return viewOf(download, items)
    },
    act: async (action: ActionPayload) => {
      const def = download.definition.actions[action.name]
      const context = (action.context ?? {}) as Record<string, unknown>
      if (def?.kind === 'navigate') return answerNavigate(download, await queue(), action.name, context)
      // Only an insert can be made without the server: everything else needs its records as they are.
      if (def?.kind !== 'insert') {
        throw new ApiError(409, 'this needs a connection', { code: 'offline_unsupported' })
      }
      if (!download.canInsert)
        throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'submit' } })
      const values = parseInsert(download.definition, context)
      const id = uuid()
      // The photos the record holds: the ones staged in this form, which must still be here.
      const attachments: OutboxItem['attachments'] = []
      const blobs: BlobRow[] = []
      for (const field of attachmentFields(download.definition)) {
        const list = values[field]
        for (const localId of Array.isArray(list) ? (list as string[]) : []) {
          const blob = ATTACHMENT_ID.test(localId) ? photos.take(localId) : undefined
          if (!blob) throw new ApiError(422, 'the photo is not here', { code: 'app_attachment_bad', params: { field } })
          const blobId = `${id}:${localId}`
          blobs.push({ id: blobId, item: id, blob })
          attachments.push({ localId, field, blobId, size: blob.size, type: blob.type, state: 'pending' })
        }
      }
      const now = new Date().toISOString()
      const item: Omit<OutboxItem, 'seq'> = {
        id,
        user: runtime.user,
        scopeKey: download.key,
        workspace: download.workspace,
        space: download.space,
        app: download.app,
        base,
        release: download.release,
        schema: download.definition.definition_version,
        action: {
          name: action.name,
          surfaceId: action.surfaceId,
          sourceComponentId: action.sourceComponentId ?? '',
          timestamp: typeof action.timestamp === 'string' ? action.timestamp : now,
          context,
        },
        operation: newOperation(),
        record: { id: -(1 + (Number.parseInt(hex(4), 16) >>> 1)), revision: null },
        attachments,
        state: 'queued',
        attempts: 0,
        createdAt: now,
        updatedAt: now,
      }
      let saved: OutboxItem
      try {
        saved = await runtime.store.enqueue(item, blobs)
      } catch (e) {
        // Not kept: nothing says it was.
        throw e instanceof NotSaved ? new Error(notSavedText(e)) : e
      }
      opts.onSaved?.(saved)
      // If the network is there the record goes now; if not, the engine sends it when it is.
      void runtime.engine.run()
      return answerInsert(download, await queue(), action.name)
    },
  }
}
