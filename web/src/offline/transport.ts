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
import type { LocalPhotos } from '../apps/PhotoInput'
import { ATTACHMENT_ID } from '../apps/photos'
import type { ActionPayload, AppTransport } from '../apps/transport'
import { t as translate } from '../i18n'
import {
  answerChange,
  answerNavigate,
  applyUpdate,
  attachmentFields,
  attachmentIdsOf,
  fieldsOf,
  itemsOf,
  parseInsert,
  parseUpdate,
  viewOf,
} from './local'
import type { Runtime } from './runtime'
import {
  type BlobRow,
  type Download,
  kindOf,
  mayBeOnServer,
  NotSaved,
  NotWriter,
  type OutboxItem,
  queueSignature,
  RecordBusy,
  RecordChanged,
  versionOf,
} from './store'

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

// What a change, a choice or a discard is answered with in a tab that is not the offline writer.
export const NOT_WRITER = 'offline_not_writer'

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
  // The release the screen on display was drawn from (its definition, as `load` read it): a button
  // pressed on it is resolved there, never in a newer release that was downloaded since, where the
  // same name may be another action. The records are always the copy's as it is now.
  let drawn = copy
  const screenOf = (): Download => ({ ...download, definition: drawn.definition, release: drawn.release })
  const base = `/api/workspaces/${download.workspace}/spaces/${download.space}/apps/${download.app}`
  const photos = localPhotos()
  const queue = () => runtime.store.listItems(download)

  // What the screen on display was drawn from: the writer session it was drawn in, and the queue it
  // showed. A press that changes a record is bound to it: the writer's queue may have been changed
  // since (the right was handed to another tab and back, or this tab became the writer after the
  // screen was drawn, or an item of the record was changed here), and a screen that doesn't show
  // that must not queue a change on top of it, merging into an edit it never showed. It is drawn
  // again (the renderer loads on `offline_record_changed`) before it can. The session is read before
  // the queue, so a hand-over in between makes a screen stale, never fresh. A transport that has
  // drawn nothing yet (`load` was not called) has nothing to be stale against: it adopts what it reads.
  let screen: { session: number; queue: OutboxItem[] } | undefined
  const readQueue = async () => {
    const session = runtime.writer.session()
    const items = await queue()
    return { session, items }
  }
  const versionsOf = (items: readonly OutboxItem[], id: number) =>
    items
      .filter((i) => kindOf(i) !== 'insert' && i.record.id === id)
      .map((i) => `${i.id}:${versionOf(i)}`)
      .sort()
      .join(',')

  // A change to a downloaded record: queued with the revision it is based on, and shown as the
  // person left it. Changes to one record are one item while none was sent only when they are the
  // same action, made on the same base (the same revision, the same release): the later values
  // win. A different action has other effects, and is an item of its own (so is a deletion); a
  // change made on another base, or to a record deleted meanwhile, is refused, and never mixed
  // with what is queued. Once a send of the item was started and not answered for good, the record
  // takes no further change until it is known.
  const change = async (
    download: Download,
    action: ActionPayload,
    kind: 'update' | 'delete',
    context: Record<string, unknown>,
  ) => {
    // Only the tab that holds the offline writer changes existing records (writer.ts).
    if (!runtime.writer.holds()) throw notWriter()
    const read = await readQueue()
    const id = Number(context.id)
    if (!Number.isInteger(id)) throw new ApiError(422, 'bad request', { code: 'bad_request' })
    if (id < 0) throw new ApiError(409, 'not on the server yet', { code: 'offline_record_unsynced' })
    const unavailable = () => new ApiError(409, 'download again', { code: 'offline_edit_unavailable' })
    // A copy made before the server said which records may be changed offers no change.
    if (download.modifiable === undefined) throw unavailable()
    if (!download.modifiable.includes(id)) {
      throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'modify_records' } })
    }
    const items = read.items
    // Bound to what the screen showed: the writer session it was drawn in, and the versions of the
    // record's items.
    screen ??= { session: read.session, queue: items }
    if (screen.session !== read.session || versionsOf(screen.queue, id) !== versionsOf(items, id)) {
      throw recordChanged()
    }
    const shown = itemsOf(download, items).find((i) => i.id === id)
    if (!shown) throw new ApiError(422, 'row not found', { error: 'row not found', code: 'app_row_not_found' })
    // The revision the person saw: mandatory, since the server decides by it.
    const expected = (action as { expectedRevision?: unknown }).expectedRevision
    if (typeof expected !== 'number' || !Number.isInteger(expected) || expected < 1) throw unavailable()
    // The screen is drawn from another revision of the record than the device holds now (another
    // tab settled it, or downloaded it again): the values and the revision are never mixed.
    if (shown._revision !== expected) throw recordChanged()
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
    const fresh: Omit<OutboxItem, 'seq' | 'version'> = {
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
      original: fieldsOf(shown),
      attachments: [],
      state: 'queued',
      attempts: 0,
      createdAt: now,
      updatedAt: now,
    }
    let saved: OutboxItem
    try {
      // Decided in the transaction, on the items of the record as they are stored in it, and only if
      // they are what the screen was drawn from (`shown` and `expected` were read from `items`).
      saved = await runtime.store.changeRecord(
        download,
        id,
        (prev) => {
          if (!prev) return fresh
          if (mayBeOnServer(prev)) throw new RecordBusy()
          // Deleted in another tab meanwhile: this change was not made to anything.
          if (kindOf(prev) === 'delete') throw new RecordChanged()
          if (
            prev.record.revision !== fresh.record.revision ||
            prev.release !== fresh.release ||
            prev.schema !== fresh.schema
          )
            throw new RecordChanged()
          // Another action, or a deletion: its own item, which keeps its own identity.
          if (kind !== 'update' || prev.action.name !== fresh.action.name) return fresh
          // Not sent yet, so the request is still the person's to change: one item, under a new key.
          return {
            ...prev,
            operation: fresh.operation,
            updatedAt: now,
            action: {
              ...prev.action,
              timestamp: fresh.action.timestamp,
              context: { ...prev.action.context, ...context },
            },
          }
        },
        queueSignature(items.filter((i) => i.record.id === id)),
      )
    } catch (e) {
      if (e instanceof RecordBusy)
        throw new ApiError(409, 'an earlier change is being sent', { code: 'offline_record_busy' })
      if (e instanceof RecordChanged) throw recordChanged()
      if (e instanceof NotWriter) throw notWriter()
      throw e instanceof NotSaved ? new Error(notSavedText(e)) : e
    }
    opts.onSaved?.(saved)
    void runtime.engine.run()
    const after = await readQueue()
    screen = { session: after.session, queue: after.items }
    return answerChange(download, after.items, action.name, kind, id)
  }
  // A refusal that says what is on screen is stale: the lists of this tab read the queue again too
  // (the screen itself is drawn again by the renderer, on `offline_record_changed`).
  const recordChanged = () => {
    runtime.store.refresh()
    return new ApiError(409, 'this record changed', { code: 'offline_record_changed' })
  }
  const notWriter = () => {
    runtime.store.refresh()
    return new ApiError(409, 'changes are being made in another tab', { code: NOT_WRITER })
  }

  return {
    photos,
    files: { base, uploads: true, local: photos },
    load: async () => {
      await refresh()
      drawn = download
      // The photos of records still queued are on this device: shown from there.
      const read = await readQueue()
      const items = read.items
      for (const item of items) {
        for (const a of item.attachments) {
          const blob = await runtime.store.getBlob(a.blobId)
          if (blob) photos.adopt(a.localId, blob)
        }
      }
      screen = { session: read.session, queue: items }
      return viewOf(download, items)
    },
    act: async (action: ActionPayload) => {
      await refresh()
      // Everything this press does is in the release its screen was drawn from.
      const download = screenOf()
      const def = download.definition.actions[action.name]
      const context = (action.context ?? {}) as Record<string, unknown>
      if (def?.kind === 'navigate') {
        // A screen drawn from the queue as it is now.
        const read = await readQueue()
        screen = { session: read.session, queue: read.items }
        return answerNavigate(download, read.items, action.name, context)
      }
      if (def?.kind === 'update' || def?.kind === 'delete') return change(download, action, def.kind, context)
      // Only a change to a record can be made without the server: everything else needs it.
      if (def?.kind !== 'insert') {
        throw new ApiError(409, 'this needs a connection', { code: 'offline_unsupported' })
      }
      if (!download.canInsert)
        throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'submit' } })
      const values = parseInsert(download.definition, context)
      const id = uuid()
      // The pictures the record holds (photos, and the picture of a signature): the ones staged in this
      // form, which must still be here.
      const attachments: OutboxItem['attachments'] = []
      const blobs: BlobRow[] = []
      for (const field of attachmentFields(download.definition)) {
        for (const localId of attachmentIdsOf(values[field])) {
          const blob = ATTACHMENT_ID.test(localId) ? photos.take(localId) : undefined
          if (!blob) throw new ApiError(422, 'the photo is not here', { code: 'app_attachment_bad', params: { field } })
          const blobId = `${id}:${localId}`
          blobs.push({ id: blobId, item: id, blob })
          attachments.push({ localId, field, blobId, size: blob.size, type: blob.type, state: 'pending' })
        }
      }
      const now = new Date().toISOString()
      const item: Omit<OutboxItem, 'seq' | 'version'> = {
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
      const after = await readQueue()
      screen = { session: after.session, queue: after.items }
      return answerChange(download, after.items, action.name, 'insert')
    },
  }
}
