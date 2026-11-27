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

export const newOperation = () => `off-${uuid()}`

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

export const NOT_WRITER = 'offline_not_writer'

export type OfflineTransport = AppTransport & {
  photos: ReturnType<typeof localPhotos>
}

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
  let download = copy
  const refresh = async () => {
    download = (await runtime.store.getDownload(copy)) ?? download
  }
  let drawn = copy
  const screenOf = (): Download => ({ ...download, definition: drawn.definition, release: drawn.release })
  const base = `/api/workspaces/${download.workspace}/spaces/${download.space}/apps/${download.app}`
  const photos = localPhotos()
  const queue = () => runtime.store.listItems(download)

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

  const change = async (
    download: Download,
    action: ActionPayload,
    kind: 'update' | 'delete',
    context: Record<string, unknown>,
  ) => {
    if (!runtime.writer.holds()) throw notWriter()
    const read = await readQueue()
    const id = Number(context.id)
    if (!Number.isInteger(id)) throw new ApiError(422, 'bad request', { code: 'bad_request' })
    if (id < 0) throw new ApiError(409, 'not on the server yet', { code: 'offline_record_unsynced' })
    const unavailable = () => new ApiError(409, 'download again', { code: 'offline_edit_unavailable' })
    if (download.modifiable === undefined) throw unavailable()
    if (!download.modifiable.includes(id)) {
      throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'modify_records' } })
    }
    const items = read.items
    screen ??= { session: read.session, queue: items }
    if (screen.session !== read.session || versionsOf(screen.queue, id) !== versionsOf(items, id)) {
      throw recordChanged()
    }
    const shown = itemsOf(download, items).find((i) => i.id === id)
    if (!shown) throw new ApiError(422, 'row not found', { error: 'row not found', code: 'app_row_not_found' })
    const expected = (action as { expectedRevision?: unknown }).expectedRevision
    if (typeof expected !== 'number' || !Number.isInteger(expected) || expected < 1) throw unavailable()
    if (shown._revision !== expected) throw recordChanged()
    if (kind === 'update') {
      const changes = parseUpdate(download.definition, context)
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
      saved = await runtime.store.changeRecord(
        download,
        id,
        (prev) => {
          if (!prev) return fresh
          if (mayBeOnServer(prev)) throw new RecordBusy()
          if (kindOf(prev) === 'delete') throw new RecordChanged()
          if (
            prev.record.revision !== fresh.record.revision ||
            prev.release !== fresh.release ||
            prev.schema !== fresh.schema
          )
            throw new RecordChanged()
          if (kind !== 'update' || prev.action.name !== fresh.action.name) return fresh
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
      const download = screenOf()
      const def = download.definition.actions[action.name]
      const context = (action.context ?? {}) as Record<string, unknown>
      if (def?.kind === 'navigate') {
        const read = await readQueue()
        screen = { session: read.session, queue: read.items }
        return answerNavigate(download, read.items, action.name, context)
      }
      if (def?.kind === 'update' || def?.kind === 'delete') return change(download, action, def.kind, context)
      if (def?.kind !== 'insert') {
        throw new ApiError(409, 'this needs a connection', { code: 'offline_unsupported' })
      }
      if (!download.canInsert)
        throw new ApiError(403, 'not allowed', { code: 'app_forbidden', params: { capability: 'submit' } })
      const values = parseInsert(download.definition, context)
      const id = uuid()
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
        throw e instanceof NotSaved ? new Error(notSavedText(e)) : e
      }
      opts.onSaved?.(saved)
      void runtime.engine.run()
      const after = await readQueue()
      screen = { session: after.session, queue: after.items }
      return answerChange(download, after.items, action.name, 'insert')
    },
  }
}
