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
import {
  answerChange,
  answerNavigate,
  applyUpdate,
  attachmentFields,
  downloadedItem,
  fieldsOf,
  itemsOf,
  parseInsert,
  parseUpdate,
  viewOf,
} from './local'
import type { Runtime } from './runtime'
import { type BlobRow, type Download, kindOf, mayBeOnServer, NotSaved, type OutboxItem, RecordBusy } from './store'

const hex = (n: number) => {
  const bytes = new Uint8Array(n)
  globalThis.crypto.getRandomValues(bytes)
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')
}

const uuid = () => globalThis.crypto?.randomUUID?.() ?? hex(16)

// 8 to 64 letters, digits, - and _ (the server's operation key); made once for an item.
export const newOperation = () => `off-${uuid()}`

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
  copy: Download,
  opts: { onSaved?: (item: OutboxItem) => void } = {},
): OfflineTransport {
  // The copy is read again from the device before each load and action: choosing what becomes of a
  // change that needs attention (offline/choices.ts) changes the records it holds.
  let download = copy
  const refresh = async () => {
    download = (await runtime.store.getDownload(copy)) ?? download
  }
  const base = `/api/workspaces/${download.workspace}/spaces/${download.space}/apps/${download.app}`
  const photos = localPhotos()
  const queue = () => runtime.store.listItems(download)

  // A change to a downloaded record: queued with the revision it is based on, and shown as the
  // person left it. Several changes to one record are one item while none was sent (the later
  // values win; a delete takes the place of the changes it makes pointless); once a send of it was
  // started and not answered for good, the record takes no further change until it is known.
  const change = async (action: ActionPayload, kind: 'update' | 'delete', context: Record<string, unknown>) => {
    const id = Number(context.id)
    if (!Number.isInteger(id)) throw new ApiError(422, 'bad request', { code: 'bad_request' })
    if (id < 0) throw new ApiError(409, 'not on the server yet', { code: 'offline_record_unsynced' })
    const unavailable = () => new ApiError(409, 'download again', { code: 'offline_edit_unavailable' })
    // A copy made before the server said which records may be changed offers no change.
    if (download.modifiable === undefined) throw unavailable()
    if (!download.modifiable.includes(id)) {
      throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'modify_records' } })
    }
    const items = await queue()
    const shown = itemsOf(download, items).find((i) => i.id === id)
    if (!shown) throw new ApiError(422, 'row not found', { error: 'row not found', code: 'app_row_not_found' })
    // The revision the person saw: mandatory, since the server decides by it.
    const expected = (action as { expectedRevision?: unknown }).expectedRevision
    if (typeof expected !== 'number' || !Number.isInteger(expected) || expected < 1) throw unavailable()
    if (kind === 'update') {
      const changes = parseUpdate(download.definition, context)
      // New photos are held on the device for a record's insert; adding them to a record that
      // already exists needs the connection.
      for (const field of attachmentFields(download.definition)) {
        const list = changes[field]
        if (Array.isArray(list) && list.some((x) => typeof x === 'string' && photos.take(x))) {
          throw new ApiError(409, 'photos need a connection', { code: 'offline_photos_unsupported' })
        }
      }
      applyUpdate(download.definition, fieldsOf(shown), changes)
      if (Object.keys(changes).length === 0) return answerChange(download, items, action.name, kind, id)
    }
    const now = new Date().toISOString()
    const fresh: Omit<OutboxItem, 'seq'> = {
      kind,
      id: uuid(),
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
        context: kind === 'delete' ? { id } : context,
      },
      operation: newOperation(),
      record: { id, revision: expected },
      original: fieldsOf(downloadedItem(download, id) ?? shown),
      attachments: [],
      state: 'queued',
      attempts: 0,
      createdAt: now,
      updatedAt: now,
    }
    let saved: OutboxItem
    try {
      saved = await runtime.store.changeRecord(download, id, (prev) => {
        if (!prev) return fresh
        if (mayBeOnServer(prev)) throw new RecordBusy()
        // Not sent yet, so the request is still the person's to change: one item, under a new key.
        // A deleted record isn't shown, so a change to it can't come from the screen.
        if (kindOf(prev) === 'delete') return prev
        const merged: OutboxItem = { ...prev, operation: fresh.operation, updatedAt: now }
        if (kind === 'delete') return { ...merged, kind: 'delete', action: fresh.action }
        return {
          ...merged,
          action: {
            ...prev.action,
            timestamp: fresh.action.timestamp,
            context: { ...prev.action.context, ...context },
          },
        }
      })
    } catch (e) {
      if (e instanceof RecordBusy)
        throw new ApiError(409, 'an earlier change is being sent', { code: 'offline_record_busy' })
      throw e instanceof NotSaved ? new Error(notSavedText(e)) : e
    }
    opts.onSaved?.(saved)
    void runtime.engine.run()
    return answerChange(download, await queue(), action.name, kind, id)
  }

  return {
    photos,
    files: { base, uploads: true, local: photos },
    load: async () => {
      await refresh()
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
      await refresh()
      const def = download.definition.actions[action.name]
      const context = (action.context ?? {}) as Record<string, unknown>
      if (def?.kind === 'navigate') return answerNavigate(download, await queue(), action.name, context)
      if (def?.kind === 'update' || def?.kind === 'delete') return change(action, def.kind, context)
      // Only a change to a record can be made without the server: everything else needs it.
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
        kind: 'insert',
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
      return answerChange(download, await queue(), action.name, 'insert')
    },
  }
}
